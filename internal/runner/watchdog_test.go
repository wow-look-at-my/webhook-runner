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

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
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

// An unarmed watchdog never fires, no matter how much time passes: the idle
// clock arms only at container launch, so a run queued behind a concurrency
// group (or still building its image) cannot idle out.
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

// A run that goes silent for longer than idle_timeout is killed with status
// timeout — and an error message naming the idle semantics, distinguishable
// from the total-timeout message.
func TestRunnerIdleTimeout(t *testing.T) {
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
		ID:             "h",
		Command:        []string{"one-line-then-silence", "SLEEP_30"},
		IdleTimeoutRaw: "200ms",
		// A generous total ceiling proves the idle watchdog, not the total
		// timeout, is what fires.
		TimeoutRaw: "1h",
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)

	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	r.Wait()

	assert.Equal(t, runs.StatusTimeout, run.Status())
	assert.Contains(t, run.Error(), "idle timeout after 200ms")
	assert.Contains(t, run.Error(), "no output")
}

// Output resets the idle clock: a run whose gaps between lines stay under
// idle_timeout completes normally even though its total runtime exceeds the
// idle limit — the incident this feature fixes (a 47-part map-reduce that
// logged every <=45s was killed at a 15m wall-clock ceiling).
func TestRunnerIdleTimeoutOutputKeepsRunAlive(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	// tick, 1s of silence, tock, 1s of silence, done — ~2s total runtime
	// with every silent gap well under the 5s idle limit.
	hook := diskHook(t, dir, &hooks.Hook{
		ID:             "h",
		Command:        []string{"tick", "SLEEP_1", "tock", "SLEEP_1", "done"},
		IdleTimeoutRaw: "5s",
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status,
		"steady output must keep an idle-limited run alive: %s", snap.Error)
	assert.Contains(t, snap.Output, "done")
}

// The idle clock follows the same arming rule as the total timeout: a run
// queued behind a concurrency group must not idle out while it waits.
func TestRunnerIdleTimeoutQueuedRunDoesNotIdleOut(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 1}},
	})
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Groups:  mgr,
	})

	// A holds the single slot for ~1s.
	hookA := diskHook(t, dir, &hooks.Hook{ID: "a", Command: []string{"SLEEP_1"}, ConcurrencyGroup: "g"})
	runA, err := r.Start(context.Background(), hookA, []byte("p"), http.Header{})
	require.NoError(t, err)
	waitStatus(t, runA, runs.StatusRunning, 2*time.Second)

	// B waits ~1s in the queue — far beyond its 300ms idle limit — but the
	// watchdog only arms at container launch, so B still succeeds.
	hookB := diskHook(t, dir, &hooks.Hook{ID: "b", Command: []string{"echo", "b"}, IdleTimeoutRaw: "300ms", ConcurrencyGroup: "g"})
	runB, err := r.Start(context.Background(), hookB, []byte("p"), http.Header{})
	require.NoError(t, err)
	assert.Equal(t, runs.StatusPending, runB.Status())

	r.Wait()
	assert.Equal(t, runs.StatusSuccess, runB.Status(),
		"a queued run must not idle out while waiting for its slot")
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
