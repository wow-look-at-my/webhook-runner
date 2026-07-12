package concurrency

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tryAcquire attempts a non-blocking acquire by passing an already-closed
// cancel channel: a free slot is taken (ok=true), a saturated group returns
// immediately (ok=false) instead of blocking.
func tryAcquire(m *Manager, group string) (func(), bool, error) {
	closed := make(chan struct{})
	close(closed)
	return m.Acquire(group, closed, nil)
}

func TestAcquireEmptyGroupIsNoop(t *testing.T) {
	m := NewManager(nil)
	rel, ok, err := m.Acquire("", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)
	rel() // must not panic
}

func TestAcquireUnknownGroupErrors(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	_, ok, err := m.Acquire("nope", nil, nil)
	require.Error(t, err)
	assert.False(t, ok)
}

func TestAcquireNilManager(t *testing.T) {
	var m *Manager
	// A named group on an unconfigured manager fails closed.
	_, ok, err := m.Acquire("g", nil, nil)
	require.Error(t, err)
	assert.False(t, ok)
	// The empty group is still a no-op.
	rel, ok, err := m.Acquire("", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)
	rel()
}

func TestAcquireSerializesLimitOne(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	rel1, ok, err := m.Acquire("g", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)

	waited := make(chan struct{})
	acquired := make(chan func(), 1)
	go func() {
		rel2, ok, err := m.Acquire("g", nil, func() { close(waited) })
		assert.NoError(t, err)
		assert.True(t, ok)
		acquired <- rel2
	}()

	// onWait fires because the only slot is taken.
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire never reported queueing")
	}
	// And it must not have acquired while the first holder is active.
	select {
	case <-acquired:
		t.Fatal("second acquire succeeded before the first released")
	case <-time.After(100 * time.Millisecond):
	}

	rel1()
	select {
	case rel2 := <-acquired:
		rel2()
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire never completed after release")
	}
}

func TestAcquireFastPathDoesNotReportWait(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	waited := false
	rel, ok, err := m.Acquire("g", nil, func() { waited = true })
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, waited, "an immediately-available slot must not report queueing")
	rel()
}

func TestAcquireCancelWhileQueued(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	rel1, ok, _ := m.Acquire("g", nil, nil)
	require.True(t, ok)
	defer rel1()

	cancel := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, ok, err := m.Acquire("g", cancel, nil)
		assert.NoError(t, err)
		assert.False(t, ok, "a cancelled queued acquire must report acquired=false")
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let it block in the queue
	close(cancel)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock the queued acquire")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	rel, ok, _ := m.Acquire("g", nil, nil)
	require.True(t, ok)
	rel()
	rel() // double release must not underflow the semaphore

	rel2, ok, _ := tryAcquire(m, "g")
	require.True(t, ok, "exactly one slot should be free after a double release")
	rel2()
}

func TestUpdatePreservesResizesAndDrops(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"keep": {Limit: 1}, "drop": {Limit: 1}}})
	relKeep, ok, _ := m.Acquire("keep", nil, nil)
	require.True(t, ok)

	m.Update(&Config{Groups: map[string]Group{"keep": {Limit: 1}, "new": {Limit: 2}}})

	// "keep" kept its semaphore (same limit), so it's still saturated.
	_, ok, err := tryAcquire(m, "keep")
	require.NoError(t, err)
	assert.False(t, ok, "preserved group should retain its in-flight slot")

	// "drop" was removed entirely.
	_, _, err = m.Acquire("drop", nil, nil)
	require.Error(t, err)

	// "new" exists with room for two.
	r1, ok, _ := tryAcquire(m, "new")
	require.True(t, ok)
	r2, ok, _ := tryAcquire(m, "new")
	require.True(t, ok)
	_, ok, _ = tryAcquire(m, "new")
	assert.False(t, ok, "third acquire on a limit-2 group should not fit")
	r1()
	r2()

	relKeep()
	r, ok, _ := tryAcquire(m, "keep")
	require.True(t, ok)
	r()
}

func TestStatus(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 2}}})
	st := m.Status()
	require.Len(t, st, 1)
	assert.Equal(t, "g", st[0].Name)
	assert.Equal(t, 2, st[0].Limit)
	assert.Equal(t, 2, st[0].Declared)
	assert.False(t, st[0].Overridden)
	assert.Equal(t, 0, st[0].Active)

	rel, _, _ := m.Acquire("g", nil, nil)
	st = m.Status()
	assert.Equal(t, 1, st[0].Active)
	rel()

	var nilMgr *Manager
	assert.Nil(t, nilMgr.Status())
}

// The load-bearing override invariant: swapping a group's semaphore while
// runs are active never loses or double-counts a token. A run holding a
// slot releases into the exact channel it acquired from (captured by its
// release closure); new acquires are gated by the new semaphore only.
func TestSetLimitOverrideSwapIsSafeWithRunsInFlight(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})

	// A run is active under the declared limit-1 semaphore.
	rel1, ok, err := m.Acquire("g", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)

	// Operator raises the limit to 2: fresh semaphore, two free slots.
	require.NoError(t, m.SetLimitOverride("g", 2))
	st := m.Status()
	require.Len(t, st, 1)
	assert.Equal(t, 2, st[0].Limit)
	assert.Equal(t, 1, st[0].Declared)
	assert.True(t, st[0].Overridden)

	rel2, ok, _ := tryAcquire(m, "g")
	require.True(t, ok)
	rel3, ok, _ := tryAcquire(m, "g")
	require.True(t, ok)
	_, ok, _ = tryAcquire(m, "g")
	assert.False(t, ok, "the overridden limit (2) must gate new acquires")

	// The pre-swap run releases into the OLD semaphore: it must not free a
	// slot in the new one (that would double-count), and it must not panic.
	rel1()
	rel1() // double release stays idempotent across the swap
	_, ok, _ = tryAcquire(m, "g")
	assert.False(t, ok, "an old-semaphore release must not free a new-semaphore slot")

	// Releasing a post-swap holder frees exactly one new-semaphore slot.
	rel2()
	rel4, ok, _ := tryAcquire(m, "g")
	require.True(t, ok, "a new-semaphore release must free a slot")

	// Clearing the override reverts to the declared limit (1) with the same
	// swap semantics: the two in-flight holders drain into their own
	// channel, and new acquires see exactly one declared slot.
	m.ClearLimitOverride("g")
	st = m.Status()
	assert.Equal(t, 1, st[0].Limit)
	assert.False(t, st[0].Overridden)
	rel5, ok, _ := tryAcquire(m, "g")
	require.True(t, ok)
	_, ok, _ = tryAcquire(m, "g")
	assert.False(t, ok, "the declared limit must gate after the override clears")
	rel3()
	rel4()
	rel5()
}

func TestSetLimitOverrideRejectsBelowOne(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	require.Error(t, m.SetLimitOverride("g", 0), "a 0 limit would deadlock queued runs")
	require.Error(t, m.SetLimitOverride("g", -1))
	st := m.Status()
	assert.Equal(t, 1, st[0].Limit)
	assert.False(t, st[0].Overridden)
}

// A reload (Update) must re-apply the operator's override on top of the
// fresh declared config — never silently revert it.
func TestUpdateReappliesOverride(t *testing.T) {
	cfg := &Config{Groups: map[string]Group{"g": {Limit: 3}}}
	m := NewManager(cfg)
	require.NoError(t, m.SetLimitOverride("g", 1))

	// Saturate the overridden limit, then reload with the same declared
	// config: the override must still be in effect, and — because the
	// effective limit didn't change — the in-flight slot must survive.
	rel, ok, _ := tryAcquire(m, "g")
	require.True(t, ok)
	m.Update(&Config{Groups: map[string]Group{"g": {Limit: 3}}})
	st := m.Status()
	require.Len(t, st, 1)
	assert.Equal(t, 1, st[0].Limit, "reload must re-apply the override")
	assert.Equal(t, 3, st[0].Declared)
	assert.True(t, st[0].Overridden)
	assert.Equal(t, 1, st[0].Active, "in-flight accounting must survive an override-preserving reload")
	_, ok, _ = tryAcquire(m, "g")
	assert.False(t, ok)
	rel()

	// Clearing the override then reloading reports the declared limit.
	m.ClearLimitOverride("g")
	m.Update(&Config{Groups: map[string]Group{"g": {Limit: 3}}})
	st = m.Status()
	assert.Equal(t, 3, st[0].Limit)
	assert.False(t, st[0].Overridden)
}

// An override for a group that is not currently declared stays inert (the
// group still errors on Acquire) and takes effect when a later Update
// declares the group — the orphaned-override re-apply contract.
func TestOverrideForUndeclaredGroupIsInertUntilDeclared(t *testing.T) {
	m := NewManager(nil)
	require.NoError(t, m.SetLimitOverride("later", 2))

	_, _, err := m.Acquire("later", nil, nil)
	require.Error(t, err, "an override must not conjure an undeclared group")
	assert.Empty(t, m.Status())
	_, ok := m.Declared("later")
	assert.False(t, ok)

	m.Update(&Config{Groups: map[string]Group{"later": {Limit: 5}}})
	st := m.Status()
	require.Len(t, st, 1)
	assert.Equal(t, 2, st[0].Limit, "the stored override applies once the group is declared")
	assert.Equal(t, 5, st[0].Declared)
	assert.True(t, st[0].Overridden)

	d, ok := m.Declared("later")
	require.True(t, ok)
	assert.Equal(t, 5, d)
}

// Setting an override equal to the current effective limit must keep the
// semaphore (no swap), so in-flight accounting is preserved.
func TestOverrideEqualLimitKeepsSemaphore(t *testing.T) {
	m := NewManager(&Config{Groups: map[string]Group{"g": {Limit: 1}}})
	rel, ok, _ := tryAcquire(m, "g")
	require.True(t, ok)

	require.NoError(t, m.SetLimitOverride("g", 1)) // same as declared
	st := m.Status()
	assert.True(t, st[0].Overridden)
	assert.Equal(t, 1, st[0].Active, "no-swap override must keep the in-flight slot")
	_, ok, _ = tryAcquire(m, "g")
	assert.False(t, ok)

	m.ClearLimitOverride("g") // back to declared 1: still no swap
	st = m.Status()
	assert.False(t, st[0].Overridden)
	assert.Equal(t, 1, st[0].Active)
	rel()
}
