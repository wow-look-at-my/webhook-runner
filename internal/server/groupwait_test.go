// Concurrency-group queue waits on the run read paths: /runs and /runs/{id}
// carry waiting_on {kind: "group", key, holder_run_ids, position}, and
// attachWaiters inverts the graph so every slot HOLDER lists the queued
// runs as its waiters (key "group:<name>").
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func TestRunsCarryGroupWait(t *testing.T) {
	s, _, tr, _ := newTestServer(t)

	holder := tr.New("pr-resolve")
	holder.SetRunning()
	queued := tr.New("pr-resolve")
	queued.SetWaitingOn(runs.WaitingOn{
		Kind:         runs.WaitingOnGroup,
		Key:          "model-gateway",
		HolderRunIDs: []string{holder.ID()},
		Position:     1,
	})

	// List view: the queued run's waiting_on carries the group fields.
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs", nil))
	require.Equal(t, 200, rec.Code)
	var list []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	byID := map[string]runs.RunState{}
	for _, st := range list {
		byID[st.ID] = st
	}
	q := byID[queued.ID()]
	require.NotNil(t, q.WaitingOn, "queued run must expose waiting_on")
	assert.Equal(t, runs.WaitingOnGroup, q.WaitingOn.Kind)
	assert.Equal(t, "model-gateway", q.WaitingOn.Key)
	assert.Equal(t, []string{holder.ID()}, q.WaitingOn.HolderRunIDs)
	assert.Equal(t, 1, q.WaitingOn.Position)

	// Inversion: the HOLDER lists the queued run as a waiter, marked as a group wait so renderers can tell it from a lock wait.
	h := byID[holder.ID()]
	require.Len(t, h.Waiters, 1)
	assert.Equal(t, queued.ID(), h.Waiters[0].RunID)
	assert.Equal(t, "pr-resolve", h.Waiters[0].HookID)
	assert.Equal(t, "group:model-gateway", h.Waiters[0].Key)

	// Single-run view: same decoration.
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+holder.ID(), nil))
	require.Equal(t, 200, rec.Code)
	var one runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &one))
	require.Len(t, one.Waiters, 1)
	assert.Equal(t, "group:model-gateway", one.Waiters[0].Key)

	// Acquire clears the wait: after ClearWaitingOn the queued run shows no waiting_on and the holder no waiters.
	seq := queued.SetWaitingOn(runs.WaitingOn{Kind: runs.WaitingOnGroup, Key: "model-gateway"})
	queued.ClearWaitingOn(seq)
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs", nil))
	list = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, st := range list {
		if st.ID == queued.ID() {
			assert.Nil(t, st.WaitingOn)
		}
		if st.ID == holder.ID() {
			assert.Empty(t, st.Waiters)
		}
	}
}

// A group wait whose holders include a run of ANOTHER hook still inverts:
// waiter entries land on the holder regardless of hook (groups are shared
// across hooks by design).
func TestGroupWaitInversionAcrossHooks(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	holder := tr.New("pr-describe")
	holder.SetRunning()
	queued := tr.New("pr-resolve")
	queued.SetWaitingOn(runs.WaitingOn{
		Kind:         runs.WaitingOnGroup,
		Key:          "model-gateway",
		HolderRunIDs: []string{holder.ID()},
		Position:     1,
	})

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+holder.ID(), nil))
	var one runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &one))
	require.Len(t, one.Waiters, 1)
	assert.Equal(t, "pr-resolve", one.Waiters[0].HookID)
}
