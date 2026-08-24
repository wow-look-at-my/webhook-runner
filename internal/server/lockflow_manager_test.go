package server

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

// Manager-instance lock blocking shares blockOnLock with run lock blocking
// (see the lockWaiter interface in statelock.go) — these tests cover the
// manager-instance side of that unification.

func hasEventKind(evs []events.Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestStateLockBlockingManagerInstanceWaitsForRelease(t *testing.T) {
	s, fm, _, _, rec, store := managerServer(t, managerDoc)
	fm.bind("coord", "inst-1")

	// The holder need not be a tracked run at all — a manager instance can
	// contend on a lock anything else holds.
	_, err := store.AcquireLock("coord", "gate", "external-holder", 0)
	require.NoError(t, err)

	tok := store.Token("coord", "inst-1")
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/kv/gate/acquire", tok,
			strings.NewReader(`{"block":true,"block_timeout_seconds":10}`))
	}()

	require.Eventually(t, func() bool {
		return hasEventKind(rec.ListByHook("coord", 20), "lock.waiting")
	}, 2*time.Second, 5*time.Millisecond)
	msgs := rec.ListByHook("coord", 20)
	found := false
	for _, e := range msgs {
		if e.Kind == "lock.waiting" {
			assert.Contains(t, e.Msg, "instance inst-1")
			assert.Contains(t, e.Msg, "external-holder")
			found = true
		}
	}
	assert.True(t, found)

	require.NoError(t, store.ReleaseLock("coord", "gate", "external-holder"))
	select {
	case rr := <-respCh:
		require.Equal(t, 200, rr.Code)
		assert.Contains(t, rr.Body.String(), "inst-1", "the waiting instance owns the lock now")
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not complete after release")
	}
}

// The regression: a manager instance's blocking acquire must work with NO
// run tracker configured at all. blockOnLock used to consult s.tracker
// FIRST and 503 immediately when it was nil, before ever checking whether
// the caller was a manager instance — wrong, since a manager instance's
// liveness (TouchInstance) is independent of run tracking.
func TestStateLockBlockingManagerInstanceWorksWithoutARunTracker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	rec := events.NewRecorder(50)
	fm := newFakeManagers("coord")
	fm.bind("coord", "inst-1")
	s := New(Options{Logger: logger, KV: store, Events: rec, Managers: fm}) // Tracker: nil, deliberately

	_, err = store.AcquireLock("coord", "gate", "external-holder", 0)
	require.NoError(t, err)

	tok := store.Token("coord", "inst-1")
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/kv/gate/acquire", tok,
			strings.NewReader(`{"block":true,"block_timeout_seconds":10}`))
	}()

	require.Eventually(t, func() bool {
		return hasEventKind(rec.ListByHook("coord", 20), "lock.waiting")
	}, 2*time.Second, 5*time.Millisecond, "a manager instance must be able to block with no run tracker configured")

	require.NoError(t, store.ReleaseLock("coord", "gate", "external-holder"))
	select {
	case rr := <-respCh:
		require.Equal(t, 200, rr.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not complete after release")
	}
}

// A manager instance that stops being current mid-wait (superseded by a
// fresher one) gets refused on the next poll tick — it must never be handed
// a lock, or block forever, on behalf of an instance that is gone.
func TestStateLockBlockingManagerInstanceStopsWhenSuperseded(t *testing.T) {
	s, fm, _, _, _, store := managerServer(t, managerDoc)
	fm.bind("coord", "inst-1")

	_, err := store.AcquireLock("coord", "gate", "external-holder", 0)
	require.NoError(t, err)

	tok := store.Token("coord", "inst-1")
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/kv/gate/acquire", tok,
			strings.NewReader(`{"block":true,"block_timeout_seconds":10}`))
	}()

	// A fresher instance replaces inst-1 (a redeploy). inst-1's own block
	// must not linger believing it is still current.
	time.Sleep(20 * time.Millisecond)
	fm.bind("coord", "inst-2")

	select {
	case rr := <-respCh:
		require.Equal(t, 409, rr.Code)
		assert.Contains(t, rr.Body.String(), "not the current instance")
	case <-time.After(5 * time.Second):
		t.Fatal("superseded instance's blocking acquire never returned")
	}
}
