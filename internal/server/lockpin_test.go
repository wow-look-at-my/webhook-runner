package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

// Pin end to end: the holder pins, a steal is refused naming the
// pinned holder (nothing cancelled, feed records the refusal), unpin
// restores steal semantics.
func TestStateLockPinRefusesStealUntilUnpin(t *testing.T) {
	s, store, tr, rec := newWaitServer(t)

	holder := tr.New("h")
	holder.SetRunning()
	holderTok := store.Token("h", holder.ID())
	require.Equal(t, 200, stateReq(t, s, "POST", "/kv/pr-7/acquire", holderTok, nil).Code)
	require.Equal(t, 204, stateReq(t, s, "POST", "/kv/pr-7/pin", holderTok, nil).Code)

	thief := tr.New("h")
	thief.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/pr-7/steal", store.Token("h", thief.ID()), nil)
	require.Equal(t, 409, rr.Code)
	var res struct {
		Error  string `json:"error"`
		HeldBy struct {
			RunID  string `json:"run_id"`
			Pinned bool   `json:"pinned"`
		} `json:"held_by"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	assert.Contains(t, res.Error, "pinned")
	assert.Equal(t, holder.ID(), res.HeldBy.RunID)
	assert.True(t, res.HeldBy.Pinned)

	// The refusal is loud and the holder untouched.
	assert.Contains(t, eventKinds(rec.ListByHook("h", 20)), "lock.steal_refused")
	assert.False(t, holder.Snapshot(0).CancelRequested)

	// Unpin, then the steal transfers (and cancels) normally.
	require.Equal(t, 204, stateReq(t, s, "POST", "/kv/pr-7/unpin", holderTok, nil).Code)
	rr = stateReq(t, s, "POST", "/kv/pr-7/steal", store.Token("h", thief.ID()), nil)
	require.Equal(t, 200, rr.Code)
	assert.True(t, holder.Snapshot(0).CancelRequested)
}

// Owner-only + not-held mappings on the HTTP surface: for nothing
// held, for another run's lock, idempotent for the owner.
func TestStateLockPinOwnershipMapping(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)

	runA := tr.New("h")
	runA.SetRunning()
	tokA := store.Token("h", runA.ID())

	require.Equal(t, 404, stateReq(t, s, "POST", "/kv/free/pin", tokA, nil).Code)
	require.Equal(t, 404, stateReq(t, s, "POST", "/kv/free/unpin", tokA, nil).Code)

	require.Equal(t, 200, stateReq(t, s, "POST", "/kv/l/acquire", tokA, nil).Code)

	runB := tr.New("h")
	runB.SetRunning()
	tokB := store.Token("h", runB.ID())
	require.Equal(t, 409, stateReq(t, s, "POST", "/kv/l/pin", tokB, nil).Code)
	require.Equal(t, 409, stateReq(t, s, "POST", "/kv/l/unpin", tokB, nil).Code)

	require.Equal(t, 204, stateReq(t, s, "POST", "/kv/l/pin", tokA, nil).Code)
	require.Equal(t, 204, stateReq(t, s, "POST", "/kv/l/pin", tokA, nil).Code)
	require.Equal(t, 204, stateReq(t, s, "POST", "/kv/l/unpin", tokA, nil).Code)
}

// Atomic take-and-pin on acquire: {"pinned": true} leaves no window — the
// acquire's own response already reports the pin, and a steal is refused.
func TestStateLockAcquirePinnedAtomically(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)

	holder := tr.New("h")
	holder.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/l/acquire", store.Token("h", holder.ID()),
		strings.NewReader(`{"pinned": true}`))
	require.Equal(t, 200, rr.Code)
	var info kv.LockInfo
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &info))
	assert.True(t, info.Pinned)

	thief := tr.New("h")
	thief.SetRunning()
	require.Equal(t, 409, stateReq(t, s, "POST", "/kv/l/steal", store.Token("h", thief.ID()), nil).Code)
	assert.False(t, holder.Snapshot(0).CancelRequested)
}

// A refused stealer's fallback works: blocking acquire (with its own pin
// request) wins the moment the pinned holder releases — the deferred-not-
// dropped contract for latest-event-wins callers.
func TestStateLockPinnedStealFallbackBlockingAcquire(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)

	holder := tr.New("h")
	holder.SetRunning()
	holderTok := store.Token("h", holder.ID())
	require.Equal(t, 200, stateReq(t, s, "POST", "/kv/l/acquire", holderTok,
		strings.NewReader(`{"pinned": true}`)).Code)

	waiter := tr.New("h")
	waiter.SetRunning()
	waiterTok := store.Token("h", waiter.ID())
	require.Equal(t, 409, stateReq(t, s, "POST", "/kv/l/steal", waiterTok, nil).Code)

	done := make(chan int, 1)
	go func() {
		rr := stateReq(t, s, "POST", "/kv/l/acquire", waiterTok,
			strings.NewReader(`{"block": true, "block_timeout_seconds": 60, "pinned": true}`))
		done <- rr.Code
	}()

	// Holder finishes its critical section and releases; the blocked acquire takes the lock (pinned, per its own request).
	require.Equal(t, 204, stateReq(t, s, "POST", "/kv/l/release", holderTok, nil).Code)
	require.Equal(t, 200, <-done)

	info, err := store.AcquireLock("h", "l", waiter.ID(), 0)
	require.NoError(t, err)
	assert.True(t, info.Pinned, "the blocking acquire carried its pinned request through the retry loop")
}
