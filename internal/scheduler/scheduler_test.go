package scheduler

import (
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic scheduler tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recorder counts fire calls per hook.
type recorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func newRecorder() *recorder { return &recorder{counts: map[string]int{}} }

func (r *recorder) fire(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[id]++
}

func (r *recorder) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[id]
}

func newTestScheduler() (*Scheduler, *fakeClock, *recorder) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rec := newRecorder()
	s := New(Options{Fire: rec.fire, Now: clk.now})
	return s, clk, rec
}

func TestFiresImmediatelyOnFirstRegistration(t *testing.T) {
	s, _, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": 5 * time.Minute})
	// A freshly added schedule is due at "now", so the very first tick fires it.
	s.fireDue()
	got := rec.count("sweep")
	require.Equal(t, 1, got)

}

func TestDoesNotFireBeforeInterval(t *testing.T) {
	s, clk, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": 5 * time.Minute})
	s.fireDue() // immediate fire (1)
	clk.advance(4 * time.Minute)
	s.fireDue() // too early for the second fire
	got := rec.count("sweep")
	require.Equal(t, 1, got)

	clk.advance(1 * time.Minute) // now 5m since the first fire
	s.fireDue()
	require.Equal(t, 2, rec.count("sweep"))

}

func TestFiresEveryInterval(t *testing.T) {
	s, clk, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": time.Minute})
	s.fireDue() // 1 (immediate)
	for i := 0; i < 3; i++ {
		clk.advance(time.Minute)
		s.fireDue()
	}
	got := rec.count("sweep")
	require.Equal(t, 4, got)

}

func TestUnchangedReloadPreservesNextFire(t *testing.T) {
	s, clk, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": 5 * time.Minute})
	s.fireDue() // immediate fire (1)
	clk.advance(2 * time.Minute)
	// A reload with the SAME interval must not reset the timer or re-fire.
	s.Update(map[string]time.Duration{"sweep": 5 * time.Minute})
	s.fireDue()
	got := rec.count("sweep")
	require.Equal(t, 1, got)

	clk.advance(3 * time.Minute) // 5m total since first fire
	s.fireDue()
	require.Equal(t, 2, rec.count("sweep"))

}

func TestChangedIntervalRefiresImmediately(t *testing.T) {
	s, clk, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": time.Hour})
	s.fireDue() // immediate (1)
	clk.advance(time.Minute)
	// Changing the interval re-arms the schedule to fire immediately.
	s.Update(map[string]time.Duration{"sweep": 10 * time.Minute})
	s.fireDue()
	require.Equal(t, 2, rec.count("sweep"))

}

func TestRemovedScheduleStopsFiring(t *testing.T) {
	s, clk, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": time.Minute})
	s.fireDue()                          // 1
	s.Update(map[string]time.Duration{}) // removed
	clk.advance(time.Hour)
	s.fireDue()
	got := rec.count("sweep")
	require.Equal(t, 1, got)

	require.Equal(t, 0, len(s.Schedules()))

}

func TestNonPositiveIntervalIgnored(t *testing.T) {
	s, _, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"bad": 0, "neg": -time.Minute, "ok": time.Minute})
	s.fireDue()
	require.False(t, rec.count("bad") != 0 || rec.count("neg") != 0)

	require.Equal(t, 1, rec.count("ok"))

	_, ok := s.Schedules()["bad"]
	require.False(t, ok)

}

func TestNoBacklogBurstAfterLongPause(t *testing.T) {
	s, clk, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"sweep": time.Minute})
	s.fireDue() // 1
	// Simulate a long pause (slept/restarted process): many intervals elapse
	// before the next tick. The hook should fire ONCE, not once per missed
	// minute.
	clk.advance(time.Hour)
	s.fireDue()
	require.Equal(t, 2, rec.count("sweep"))

}

func TestMultipleHooksDeterministic(t *testing.T) {
	s, _, rec := newTestScheduler()
	s.Update(map[string]time.Duration{"a": time.Minute, "b": time.Minute})
	s.fireDue()
	require.False(t, rec.count("a") != 1 || rec.count("b") != 1)

}
