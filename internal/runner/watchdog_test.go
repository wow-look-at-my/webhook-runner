package runner

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// fakeClock is a hand-advanced clock for driving the watchdog's pure logic
// without sleeping (the scheduler's injected-clock house style).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 9, 2, 31, 14, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// An unarmed watchdog never fires, no matter how much time passes: the
// clock arms only at container launch, so a run queued behind a concurrency
// group (or still building its image) cannot time out.
func TestIdleWatchdogNeverFiresBeforeArm(t *testing.T) {
	clock := newFakeClock()
	w := newIdleWatchdog(100*time.Millisecond, clock.Now)

	clock.Advance(time.Hour)
	wait, fire := w.check()
	assert.False(t, fire, "unarmed watchdog must not fire")
	assert.Equal(t, 100*time.Millisecond, wait, "unarmed watchdog re-checks a full limit out")
}

// After arming, the watchdog fires once the limit elapses with no output —
// and not a moment before.
func TestIdleWatchdogFiresOnSilence(t *testing.T) {
	clock := newFakeClock()
	w := newIdleWatchdog(time.Minute, clock.Now)
	w.Arm()

	wait, fire := w.check()
	assert.False(t, fire)
	assert.Equal(t, time.Minute, wait)

	clock.Advance(time.Minute - time.Nanosecond)
	_, fire = w.check()
	assert.False(t, fire, "must not fire before the limit elapses")

	clock.Advance(time.Nanosecond)
	wait, fire = w.check()
	assert.True(t, fire, "a full limit of silence fires")
	assert.Equal(t, time.Duration(0), wait)
}

// REGRESSION: a zero limit (a hook that omits `timeout` — "no absolute
// ceiling") must never fire, armed or not, however much time passes. Without
// the limit<=0 guard in check(), idle >= w.limit is vacuously true (idle can
// never be negative), so an armed watchdog fired on its very first check —
// every hook without an explicit timeout timed out immediately instead of
// running until it exits.
func TestIdleWatchdogZeroLimitNeverFires(t *testing.T) {
	clock := newFakeClock()
	w := newIdleWatchdog(0, clock.Now)
	w.Arm()

	_, fire := w.check()
	assert.False(t, fire, "a zero limit must mean no idle limit, not an already-elapsed one")

	clock.Advance(24 * time.Hour)
	_, fire = w.check()
	assert.False(t, fire, "no amount of silence fires a zero-limit watchdog")
}

// Output resets the idle clock: as long as something arrives within every
// limit-sized window, the watchdog never fires — however long the run lasts.
func TestIdleWatchdogResetsOnOutput(t *testing.T) {
	clock := newFakeClock()
	w := newIdleWatchdog(time.Minute, clock.Now)
	w.Arm()

	// 45s of silence, then output, ten times over — 7.5 minutes of runtime,
	// far beyond the limit, with no window of silence ever reaching it.
	for i := 0; i < 10; i++ {
		clock.Advance(45 * time.Second)
		_, fire := w.check()
		require.False(t, fire, "iteration %d: output within the window must keep the run alive", i)
		w.Touch()
	}

	// After a touch the full limit is available again...
	wait, fire := w.check()
	assert.False(t, fire)
	assert.Equal(t, time.Minute, wait)

	// ...and true silence still fires.
	clock.Advance(time.Minute)
	_, fire = w.check()
	assert.True(t, fire)
}

// The Watch loop end-to-end with a real clock: fires (closes the channel)
// after silence, and a stopped watchdog exits without firing.
func TestIdleWatchdogWatchLoop(t *testing.T) {
	w := newIdleWatchdog(30*time.Millisecond, nil)
	w.Arm()
	stop := make(chan struct{})
	fired := w.Watch(stop)
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not fire on silence")
	}
	close(stop) // harmless after the fire; the loop already exited

	// Stopping before the limit elapses must not fire.
	w2 := newIdleWatchdog(time.Hour, nil)
	w2.Arm()
	stop2 := make(chan struct{})
	fired2 := w2.Watch(stop2)
	close(stop2)
	select {
	case <-fired2:
		t.Fatal("stopped watchdog fired")
	case <-time.After(50 * time.Millisecond):
	}
}

// --- Runner-level integration (mock docker; helpers from runner_test.go) ---
//
// `timeout` is activity-based: these tests drive the watchdog through the
// hook's one timeout field. The queued-run case (a pending run must never
// tick) is covered by TestIdleWatchdogNeverFiresBeforeArm above (the pure
// arming invariant) and TestRunnerConcurrencyGroupQueuesAndDefersTimeout in
// runner_test.go (the same integration, timeout-driven).

// A run that goes silent for longer than timeout is killed with status
// timeout and an error message naming the no-output semantics.
func TestRunnerTimeoutKillsSilentRun(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := diskHook(t, dir, &hooks.Hook{
		ID:         "h",
		Command:    []string{"one-line-then-silence", "SLEEP_30"},
		TimeoutRaw: "200ms",
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)

	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	r.Wait()

	assert.Equal(t, runs.StatusTimeout, run.Status())
	assert.Contains(t, run.Error(), "timed out after 200ms")
	assert.Contains(t, run.Error(), "no output")
}

// Output resets the clock: a run whose gaps between lines stay under
// timeout completes normally even though its TOTAL runtime exceeds the
// timeout value — the incident this semantics fixes (a healthy 47-part
// map-reduce that logged every <=45s was killed at a 15m wall-clock
// ceiling mid-progress).
func TestRunnerTimeoutOutputKeepsRunAlive(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	// A line every ~1s for ~4s of runtime, against a 3s timeout: every
	// silent gap stays well under the limit while the total runtime
	// exceeds it — under the old wall-clock semantics this run died.
	hook := diskHook(t, dir, &hooks.Hook{
		ID:         "h",
		Command:    []string{"tick", "SLEEP_1", "tock", "SLEEP_1", "tick", "SLEEP_1", "tock", "SLEEP_1", "done"},
		TimeoutRaw: "3s",
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status,
		"steady output must keep the run alive past its timeout value: %s", snap.Error)
	assert.Contains(t, snap.Output, "done")
	// The run provably outlived its timeout: >=4s of processing vs 3s.
	assert.Greater(t, snap.Finished.Sub(snap.StartedAt), 3*time.Second,
		"the run must have outlived its timeout value while producing output")
}

// touchReader stamps on every successful read — bytes, not lines — so even a
// long line without a newline counts as output.
func TestTouchReaderTouchesOnBytes(t *testing.T) {
	touches := 0
	tr := &touchReader{r: strings.NewReader("no newline here"), touch: func() { touches++ }}
	buf := make([]byte, 4)
	n, err := tr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, 1, touches)

	// Drain the rest; EOF (n==0) must not touch.
	for err == nil {
		_, err = tr.Read(buf)
	}
	got := touches
	_, _ = tr.Read(buf) // pure EOF read
	assert.Equal(t, got, touches, "a zero-byte read must not reset the idle clock")
}
