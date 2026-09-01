package concurrency

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalDefaults(t *testing.T) {
	assert.Equal(t, 64, DefaultGlobalLimit, "the shipped default is 64")

	g := NewGlobal(0) // < falls back to the built-in default
	st := g.Status()
	assert.Equal(t, DefaultGlobalLimit, st.Limit)
	assert.Equal(t, DefaultGlobalLimit, st.Default)
	assert.False(t, st.Overridden)
	assert.Equal(t, DefaultGlobalLimit, g.Default())

	g = NewGlobal(7)
	assert.Equal(t, 7, g.Status().Limit)
	assert.Equal(t, 7, g.Default())
}

func TestGlobalNilSafe(t *testing.T) {
	var g *Global
	release, acquired := g.Acquire("r", nil, nil)
	assert.True(t, acquired, "a nil cap applies no limit")
	release()
	assert.Error(t, g.SetLimitOverride(3), "writes on nil must error, never silently no-op")
	g.ClearLimitOverride()
	assert.Zero(t, g.Status())
	h, w := g.QueueDetail()
	assert.Nil(t, h)
	assert.Nil(t, w)
}

// The directive's core property: with cap N, N+ concurrent acquires run at
// most N at ; the extra QUEUES (reported via onQueue) and runs a
// slot frees. Nothing is dropped or errored.
func TestGlobalCapHoldsAtLimit(t *testing.T) {
	const capN = 3
	g := NewGlobal(capN)

	var active, peak, ran atomic.Int64
	gate := make(chan struct{}) // holds the N inside their critical section
	queuedCh := make(chan struct{}, 1)

	var wg sync.WaitGroup
	acquireOne := func(id string, onQueue func(QueueState)) {
		defer wg.Done()
		release, acquired := g.Acquire(id, nil, onQueue)
		require.True(t, acquired)
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-gate
		active.Add(-1)
		ran.Add(1)
		release()
	}

	wg.Add(capN)
	for i := 0; i < capN; i++ {
		go acquireOne(string(rune('a'+i)), nil)
	}
	// Wait until all N hold slots.
	require.Eventually(t, func() bool { return active.Load() == capN }, 2*time.Second, 5*time.Millisecond)

	// The N+th must QUEUE, not run and not error.
	wg.Add(1)
	go acquireOne("extra", func(QueueState) {
		select {
		case queuedCh <- struct{}{}:
		default:
		}
	})
	select {
	case <-queuedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the N+1th acquire never reported queueing")
	}
	assert.Equal(t, int64(capN), active.Load(), "the cap must hold at N while the extra queues")
	st := g.Status()
	assert.Equal(t, capN, st.Active)
	assert.Equal(t, 1, st.Waiting)

	// Free the holders: the queued acquire runs; everything completes.
	close(gate)
	wg.Wait()
	assert.Equal(t, int64(capN+1), ran.Load(), "nothing dropped: all N+1 executions ran")
	assert.Equal(t, int64(capN), peak.Load(), "at most N ran simultaneously")
	st = g.Status()
	assert.Zero(t, st.Active)
	assert.Zero(t, st.Waiting)
}

// A raised cap admits already-queued acquires immediately (the retired-
// channel re-bind), without waiting for any holder to release.
func TestGlobalRaiseAdmitsQueuedImmediately(t *testing.T) {
	g := NewGlobal(1)
	release, acquired := g.Acquire("holder", nil, nil)
	require.True(t, acquired)
	defer release()

	got := make(chan struct{})
	go func() {
		rel, ok := g.Acquire("queued", nil, nil)
		if ok {
			close(got)
			rel()
		}
	}()
	require.Eventually(t, func() bool { return g.Status().Waiting == 1 }, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, g.SetLimitOverride(2))
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("raising the cap did not admit the queued acquire (holder never released)")
	}
	st := g.Status()
	assert.Equal(t, 2, st.Limit)
	assert.True(t, st.Overridden)
	assert.Equal(t, g.Default(), st.Default)
}

func TestGlobalClearRevertsToDefault(t *testing.T) {
	g := NewGlobal(5)
	require.NoError(t, g.SetLimitOverride(9))
	st := g.Status()
	require.Equal(t, 9, st.Limit)
	require.True(t, st.Overridden)

	g.ClearLimitOverride()
	st = g.Status()
	assert.Equal(t, 5, st.Limit, "clearing reverts to the configured default")
	assert.False(t, st.Overridden)

	assert.Error(t, g.SetLimitOverride(0), "a 0 cap is rejected — it would block every run")
}

// Cancel while queued on the cap: acquired=false, nothing leaked.
func TestGlobalCancelWhileQueued(t *testing.T) {
	g := NewGlobal(1)
	release, acquired := g.Acquire("holder", nil, nil)
	require.True(t, acquired)
	defer release()

	cancel := make(chan struct{})
	done := make(chan bool)
	go func() {
		_, ok := g.Acquire("victim", cancel, nil)
		done <- ok
	}()
	require.Eventually(t, func() bool { return g.Status().Waiting == 1 }, 2*time.Second, 5*time.Millisecond)
	close(cancel)
	select {
	case ok := <-done:
		assert.False(t, ok, "a cancelled queued acquire reports acquired=false")
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled acquire never returned")
	}
	assert.Zero(t, g.Status().Waiting)
}

// The cap's queue detail carries holder/waiter run ids for the dashboard
// drill-down, exactly like a group's.
func TestGlobalQueueDetail(t *testing.T) {
	g := NewGlobal(1)
	release, acquired := g.Acquire("holder-run", nil, nil)
	require.True(t, acquired)

	got := make(chan struct{})
	go func() {
		rel, ok := g.Acquire("waiting-run", nil, nil)
		if ok {
			close(got)
			rel()
		}
	}()
	require.Eventually(t, func() bool { return g.Status().Waiting == 1 }, 2*time.Second, 5*time.Millisecond)

	holders, waiting := g.QueueDetail()
	require.Len(t, holders, 1)
	assert.Equal(t, "holder-run", holders[0].ID)
	require.Len(t, waiting, 1)
	assert.Equal(t, "waiting-run", waiting[0].ID)

	release()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never admitted after release")
	}
}
