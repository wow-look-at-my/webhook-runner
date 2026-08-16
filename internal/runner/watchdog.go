package runner

import (
	"io"
	"sync"
	"time"
)

// idleWatchdog fires when a run's container produces no output for longer
// than a configured limit. It is what implements the hook `idle_timeout`:
// a hook that keeps logging is making forward progress and may run
// indefinitely under this watchdog alone (the separate `timeout` field
// bounds total wall-clock time, if set); one that has gone silent for the
// whole limit is stuck and gets killed. A limit of zero means no idle
// limit — the watchdog never fires.
//
// Like the scheduler, the decision logic is pure and takes an injected clock
// (now), so it is unit-testable without sleeping: Arm/Touch/check hold every
// decision, and Watch is a thin timer loop around check.
//
// The watchdog is armed only after the concurrency-group slot is acquired
// and the container has actually launched (see execute). An unarmed watchdog
// never fires, whatever the clock says — a queued run cannot time out.
type idleWatchdog struct {
	limit time.Duration
	now   func() time.Time

	mu    sync.Mutex
	armed bool
	last  time.Time // last observed output (or the arm instant)

	fired chan struct{}
}

// noIdleLimitRecheck is the wait check() returns for a limit-less watchdog.
// It can never fire, so the exact value only bounds how promptly Watch's
// loop notices its stop channel closing — a day is plenty.
const noIdleLimitRecheck = 24 * time.Hour

// newIdleWatchdog constructs an unarmed watchdog. now defaults to time.Now.
func newIdleWatchdog(limit time.Duration, now func() time.Time) *idleWatchdog {
	if now == nil {
		now = time.Now
	}
	return &idleWatchdog{limit: limit, now: now, fired: make(chan struct{})}
}

// Arm starts the clock. Called at container launch — never earlier, so
// secrets decryption, the image build, and queue time can't count as silence.
func (w *idleWatchdog) Arm() {
	w.mu.Lock()
	w.armed = true
	w.last = w.now()
	w.mu.Unlock()
}

// Touch records output, resetting the idle clock. Any output byte counts.
func (w *idleWatchdog) Touch() {
	w.mu.Lock()
	w.last = w.now()
	w.mu.Unlock()
}

// Disarm stops the clock without firing. Manager sessions arm the watchdog
// only while an inbox event is checked out (delivered and not yet followed
// by the manager's next /inbox/next call) — a manager parked in its
// long-poll with an empty inbox owes no output and must never be reaped,
// however long it idles. Hook runs never disarm (their whole lifetime is
// the checked-out section).
func (w *idleWatchdog) Disarm() {
	w.mu.Lock()
	w.armed = false
	w.mu.Unlock()
}

// check reports whether the watchdog should fire now and, when it shouldn't,
// how long to wait before re-checking: the earliest instant it could
// possibly fire (so output flowing steadily costs one wake-up per limit),
// or the full limit while still unarmed.
func (w *idleWatchdog) check() (wait time.Duration, fire bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// limit <= 0 means no idle limit at all (a hook that omits
	// `idle_timeout` runs uncapped) — never fire. Without this,
	// idle >= w.limit below is vacuously true the instant the watchdog is
	// armed (idle can never be negative), so every run without an idle
	// limit would time out immediately instead of running until it exits.
	// Re-check on a long interval rather than busy-looping on a zero wait.
	if w.limit <= 0 {
		return noIdleLimitRecheck, false
	}
	if !w.armed {
		return w.limit, false
	}
	idle := w.now().Sub(w.last)
	if idle >= w.limit {
		return 0, true
	}
	return w.limit - idle, false
}

// Watch launches the firing loop and returns the channel it closes if the
// idle limit ever elapses with no output. The loop exits without firing when
// stop closes.
func (w *idleWatchdog) Watch(stop <-chan struct{}) <-chan struct{} {
	go func() {
		for {
			wait, fire := w.check()
			if fire {
				close(w.fired)
				return
			}
			timer := time.NewTimer(wait)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return w.fired
}

// touchReader stamps the watchdog on every successful read from a container
// output pipe. Wrapping the read side (rather than the line scanner) means
// ANY output byte resets the idle clock — a hook midway through emitting a
// very long line without a newline still counts as producing output.
type touchReader struct {
	r     io.Reader
	touch func()
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}
