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
	assert.Equal(t, 0, st[0].Active)

	rel, _, _ := m.Acquire("g", nil, nil)
	st = m.Status()
	assert.Equal(t, 1, st[0].Active)
	rel()

	var nilMgr *Manager
	assert.Nil(t, nilMgr.Status())
}
