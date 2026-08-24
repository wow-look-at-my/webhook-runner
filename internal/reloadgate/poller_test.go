package reloadgate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock is a manually-advanced clock (the scheduler-test pattern).
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

type fireCounter struct {
	mu sync.Mutex
	n  int
}

func (f *fireCounter) fire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
}

func (f *fireCounter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func newTestPoller(interval time.Duration) (*Poller, *fakeClock, *fireCounter) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	fc := &fireCounter{}
	p := NewPoller(PollerOptions{Interval: interval, Fire: fc.fire, Now: clk.now})
	return p, clk, fc
}

func TestPollerFiresImmediatelyOnStart(t *testing.T) {
	p, _, fc := newTestPoller(time.Hour)
	p.fireDue()
	require.Equal(t, 1, fc.count(), "the first pass is due immediately (startup catch-up)")
}

func TestPollerFiresOnInterval(t *testing.T) {
	p, clk, fc := newTestPoller(time.Hour)
	p.fireDue() // immediate (1)
	clk.advance(59 * time.Minute)
	p.fireDue()
	require.Equal(t, 1, fc.count(), "before the interval elapses nothing fires")

	clk.advance(time.Minute) // 1h since the first fire
	p.fireDue()
	require.Equal(t, 2, fc.count())

	clk.advance(time.Hour)
	p.fireDue()
	require.Equal(t, 3, fc.count())
}

func TestPollerNoBacklogBurstAfterPause(t *testing.T) {
	p, clk, fc := newTestPoller(time.Hour)
	p.fireDue() // 1
	// A slept/restarted process misses many intervals: fire ONCE, not once per missed hour.
	clk.advance(7 * time.Hour)
	p.fireDue()
	require.Equal(t, 2, fc.count())
	p.fireDue()
	require.Equal(t, 2, fc.count(), "the catch-up fire re-arms one interval out")
}

func TestPollerDisabledAtZero(t *testing.T) {
	p, clk, fc := newTestPoller(0)
	p.fireDue()
	clk.advance(24 * time.Hour)
	p.fireDue()
	require.Zero(t, fc.count(), "interval 0 must never fire")

	// Run returns immediately when disabled — no timer, no fire (a hang here would time the test out).
	p.Run(context.Background())
	require.Zero(t, fc.count())
}

func TestPollerRunStopsOnCancel(t *testing.T) {
	// With a cancelled context Run performs the one immediate pass and returns on the first select — deterministic, no sleeps (the ticker, at the.
	p, _, fc := newTestPoller(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx)
	require.Equal(t, 1, fc.count(), "the immediate startup pass still runs; then Run returns")
}
