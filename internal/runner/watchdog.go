package runner

import (
	"io"
	"sync"
	"time"
)

// idleWatchdog fires when a run's container produces no output for longer than a configured limit.
type idleWatchdog struct {
	limit time.Duration
	now   func() time.Time

	mu    sync.Mutex
	armed bool
	last  time.Time // last observed output (or the arm instant)

	fired chan struct{}
}

// noIdleLimitRecheck is the wait check() returns for a limit-less watchdog.
const noIdleLimitRecheck = 24 * time.Hour

// newIdleWatchdog constructs an unarmed watchdog. now defaults to time.Now.
func newIdleWatchdog(limit time.Duration, now func() time.Time) *idleWatchdog {
	if now == nil {
		now = time.Now
	}
	return &idleWatchdog{limit: limit, now: now, fired: make(chan struct{})}
}

// Arm starts the clock.
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

// Disarm stops the clock without firing.
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
	// limit <= 0 means no idle limit at all (a hook that omits `idle_timeout` runs uncapped) — never fire.
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

// touchReader stamps the watchdog on every successful read from a container output pipe.
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
