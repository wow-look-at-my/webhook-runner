package concurrency

// Waiter migration across semaphore swaps (limit changes) — split from
// manager_test.go for the -line cap.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queueSignal returns an onQueue callback that signals ch , on the
// waiter's (registration) notification.
func queueSignal(ch chan<- struct{}) func(QueueState) {
	first := true
	return func(QueueState) {
		if first {
			first = false
			ch <- struct{}{}
		}
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitAcquire(t *testing.T, ch <-chan func(), what string) func() {
	t.Helper()
	select {
	case rel := <-ch:
		return rel
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func assertNoAcquire(t *testing.T, ch <-chan func(), what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected acquire: %s", what)
	case <-time.After(100 * time.Millisecond):
	}
}

// The production regression these tests pin: a limit change swaps the
// group's semaphore, but runs already QUEUED used to stay blocked on the
// retired channel — so raising a limit (the dashboard's → gha-runner
// override) had no effect on the queued backlog, which kept draining at the
// old limit. Blocked waiters must re-bind to the group's current semaphore
// at every swap site: SetLimitOverride, Update, and ClearLimitOverride.
func TestLimitRaiseAdmitsQueuedWaiters(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *Manager // group "g" at effective limit
		raise func(t *testing.T, m *Manager)
	}{
		{
			name: "set-override",
			setup: func(t *testing.T) *Manager {
				return NewManager(&Config{Groups: map[string]Group{"g": {Limit: 2}}})
			},
			raise: func(t *testing.T, m *Manager) { require.NoError(t, m.SetLimitOverride("g", 10)) },
		},
		{
			name: "update-declared",
			setup: func(t *testing.T) *Manager {
				return NewManager(&Config{Groups: map[string]Group{"g": {Limit: 2}}})
			},
			raise: func(t *testing.T, m *Manager) { m.Update(&Config{Groups: map[string]Group{"g": {Limit: 10}}}) },
		},
		{
			name: "clear-override",
			setup: func(t *testing.T) *Manager {
				m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 10}}})
				require.NoError(t, m.SetLimitOverride("g", 2))
				return m
			},
			raise: func(t *testing.T, m *Manager) { m.ClearLimitOverride("g") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.setup(t)

			// holders saturate the effective limit- semaphore.
			relA, ok, err := m.Acquire("g", "run-a", nil, nil)
			require.NoError(t, err)
			require.True(t, ok)
			relB, ok, _ := m.Acquire("g", "run-b", nil, nil)
			require.True(t, ok)

			// runs queue behind them, all registered before the raise.
			const waiters = 4
			queued := make(chan struct{}, waiters)
			acquired := make(chan func(), waiters)
			for i := 0; i < waiters; i++ {
				id := fmt.Sprintf("run-w%d", i)
				go func() {
					rel, ok, err := m.Acquire("g", id, nil, queueSignal(queued))
					assert.NoError(t, err)
					assert.True(t, ok)
					acquired <- rel
				}()
			}
			for i := 0; i < waiters; i++ {
				awaitSignal(t, queued, "waiter registration")
			}
			assertNoAcquire(t, acquired, "a waiter acquired below the limit")

			// Raise the limit: every queued waiter must acquire promptly.
			tc.raise(t, m)
			rels := make([]func(), 0, waiters)
			for i := 0; i < waiters; i++ {
				rels = append(rels, awaitAcquire(t, acquired, "a queued waiter after the raise"))
			}

			_, waiting := m.QueueDetail("g")
			assert.Empty(t, waiting, "no waiter may stay stranded after the raise")
			for _, rel := range rels {
				rel()
			}
			relA()
			relB()
			holders, _ := m.QueueDetail("g")
			assert.Empty(t, holders)
		})
	}
}

// Lowering the limit with runs queued: waiters re-bind to the new (smaller)
// semaphore — at most new-limit of them admitted immediately (the fresh
// channel starts empty; old holders above the limit finishing normally is
// the pre-existing transient) — and the rest keep contending at the new
// limit. Old holders release into their retired channel, which must never
// free a new-semaphore slot.
func TestLimitLowerMigratesQueuedWaiters(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *Manager    // group "g" at effective limit
		lower func(t *testing.T, m *Manager) // -> effective limit
	}{
		{
			name: "set-override",
			setup: func(t *testing.T) *Manager {
				return NewManager(&Config{Groups: map[string]Group{"g": {Limit: 4}}})
			},
			lower: func(t *testing.T, m *Manager) { require.NoError(t, m.SetLimitOverride("g", 2)) },
		},
		{
			name: "update-declared",
			setup: func(t *testing.T) *Manager {
				return NewManager(&Config{Groups: map[string]Group{"g": {Limit: 4}}})
			},
			lower: func(t *testing.T, m *Manager) { m.Update(&Config{Groups: map[string]Group{"g": {Limit: 2}}}) },
		},
		{
			name: "clear-override",
			setup: func(t *testing.T) *Manager {
				m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 2}}})
				require.NoError(t, m.SetLimitOverride("g", 4))
				return m
			},
			lower: func(t *testing.T, m *Manager) { m.ClearLimitOverride("g") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.setup(t)

			// holders saturate limit .
			holderRels := make([]func(), 0, 4)
			for i := 0; i < 4; i++ {
				rel, ok, err := m.Acquire("g", fmt.Sprintf("run-h%d", i), nil, nil)
				require.NoError(t, err)
				require.True(t, ok)
				holderRels = append(holderRels, rel)
			}

			// runs queue behind them.
			queued := make(chan struct{}, 3)
			acquired := make(chan func(), 3)
			for i := 0; i < 3; i++ {
				id := fmt.Sprintf("run-w%d", i)
				go func() {
					rel, ok, err := m.Acquire("g", id, nil, queueSignal(queued))
					assert.NoError(t, err)
					assert.True(t, ok)
					acquired <- rel
				}()
			}
			for i := 0; i < 3; i++ {
				awaitSignal(t, queued, "waiter registration")
			}

			tc.lower(t, m)

			// Exactly migrate into the fresh limit- channel.
			relW1 := awaitAcquire(t, acquired, "first migrated waiter")
			relW2 := awaitAcquire(t, acquired, "second migrated waiter")
			assertNoAcquire(t, acquired, "a third waiter fit a limit-2 semaphore")

			// An OLD holder releasing frees nothing at the new limit.
			holderRels[0]()
			assertNoAcquire(t, acquired, "an old-semaphore release admitted a waiter")

			// A NEW holder releasing admits the last waiter.
			relW1()
			relW3 := awaitAcquire(t, acquired, "the last waiter after a new-semaphore release")

			// Drain everything; the new semaphore gates at exactly .
			relW2()
			relW3()
			for _, rel := range holderRels[1:] {
				rel()
			}
			r1, ok, _ := tryAcquire(m, "g")
			require.True(t, ok)
			r2, ok, _ := tryAcquire(m, "g")
			require.True(t, ok)
			_, ok, _ = tryAcquire(m, "g")
			assert.False(t, ok, "the lowered limit must gate at 2")
			r1()
			r2()
		})
	}
}

// The cancel branch keeps working on every loop iteration: a waiter that
// migrated to a swapped-in semaphore (and found it full) still honors its
// cancel channel, and the counters come back clean.
func TestCancelAfterWaiterMigration(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	relA, ok, err := m.Acquire("g", "run-a", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)

	// waiters share cancel channel.
	cancel := make(chan struct{})
	queued := make(chan struct{}, 3)
	acquired := make(chan func(), 3)
	notAcquired := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("run-w%d", i)
		go func() {
			rel, ok, err := m.Acquire("g", id, cancel, queueSignal(queued))
			assert.NoError(t, err)
			if ok {
				acquired <- rel
			} else {
				notAcquired <- struct{}{}
			}
		}()
	}
	for i := 0; i < 3; i++ {
		awaitSignal(t, queued, "waiter registration")
	}

	// Raise to : exactly waiters fit the fresh semaphore; the
	require.NoError(t, m.SetLimitOverride("g", 2))
	rel1 := awaitAcquire(t, acquired, "first migrated waiter")
	rel2 := awaitAcquire(t, acquired, "second migrated waiter")
	assertNoAcquire(t, acquired, "a third waiter fit a limit-2 semaphore")

	// Cancel unblocks the migrated waiter as not-acquired.
	close(cancel)
	awaitSignal(t, notAcquired, "the cancelled migrated waiter")
	_, waiting := m.QueueDetail("g")
	assert.Empty(t, waiting)
	st := m.Status()
	require.Len(t, st, 1)
	assert.Equal(t, 0, st[0].Waiting, "no waiter counter may linger after a cancel")
	rel1()
	rel2()
	relA()
}

// A group removed by a reload while runs are queued must fail those
// acquires loudly — never leave them blocked on a channel nothing will
// release into, never run them unbounded.
func TestUpdateRemovingGroupFailsQueuedWaiters(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	relA, ok, err := m.Acquire("g", "run-a", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)

	queued := make(chan struct{}, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("run-w%d", i)
		go func() {
			_, ok, err := m.Acquire("g", id, nil, queueSignal(queued))
			assert.False(t, ok)
			errs <- err
		}()
	}
	awaitSignal(t, queued, "first waiter registration")
	awaitSignal(t, queued, "second waiter registration")

	m.Update(&Config{Groups: map[string]Group{"other": {Limit: 1}}})

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			require.Error(t, err)
			assert.Contains(t, err.Error(), "removed while queued")
		case <-time.After(2 * time.Second):
			t.Fatal("queued waiter never failed after its group was removed")
		}
	}

	// The wait line is gone; the removed group's holder stays listed until
	holders, waiting := m.QueueDetail("g")
	assert.Empty(t, waiting)
	require.Len(t, holders, 1)
	assert.Equal(t, "run-a", holders[0].ID)
	relA()
	holders, _ = m.QueueDetail("g")
	assert.Empty(t, holders)
}

// QueueDetail and Status stay coherent across a raise: waiters are listed
// (in registration order) before, everyone lands in holders after, and no
// counter goes negative or lingers.
func TestQueueBookkeepingCoherentAcrossLimitRaise(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 2}}})
	relA, ok, _ := m.Acquire("g", "run-a", nil, nil)
	require.True(t, ok)
	relB, ok, _ := m.Acquire("g", "run-b", nil, nil)
	require.True(t, ok)

	// Register the waiters at a time so the line order is fixed.
	queued := make(chan struct{}, 2)
	acquired := make(chan func(), 2)
	for _, id := range []string{"run-w0", "run-w1"} {
		go func() {
			rel, ok, err := m.Acquire("g", id, nil, queueSignal(queued))
			assert.NoError(t, err)
			assert.True(t, ok)
			acquired <- rel
		}()
		awaitSignal(t, queued, id+" registration")
	}

	holders, waiting := m.QueueDetail("g")
	require.Len(t, holders, 2)
	require.Len(t, waiting, 2)
	assert.Equal(t, "run-w0", waiting[0].ID)
	assert.Equal(t, "run-w1", waiting[1].ID)

	require.NoError(t, m.SetLimitOverride("g", 5))
	rel1 := awaitAcquire(t, acquired, "first waiter after the raise")
	rel2 := awaitAcquire(t, acquired, "second waiter after the raise")

	holders, waiting = m.QueueDetail("g")
	assert.Len(t, holders, 4, "both old holders and both migrated waiters stay listed")
	assert.Empty(t, waiting)
	st := m.Status()
	require.Len(t, st, 1)
	assert.Equal(t, 5, st[0].Limit)
	assert.Equal(t, 0, st[0].Waiting, "no waiter counter may linger after migration")
	// Active counts the CURRENT semaphore's tokens only — the migrated
	assert.Equal(t, 2, st[0].Active)

	rel1()
	rel2()
	relA()
	relB()
	holders, _ = m.QueueDetail("g")
	assert.Empty(t, holders)
	st = m.Status()
	assert.Equal(t, 0, st[0].Active)
	assert.Equal(t, 0, st[0].Waiting)
}
