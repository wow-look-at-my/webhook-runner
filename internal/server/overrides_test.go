package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// newOverrideTestServer builds a server with the kill-switch plumbing: an
// overrides store (persisted under a temp dir), an events recorder, and a
// concurrency manager with "g" group (declared limit ).
func newOverrideTestServer(t *testing.T) (*Server, *hooks.Registry, *overrides.Store, *events.Recorder) {
	t.Helper()
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	ov, err := overrides.Open(filepath.Join(dir, "overrides.json"))
	require.NoError(t, err)
	rec := events.NewRecorder(100)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	// Drain in-flight runs before the test's TempDir is removed.
	t.Cleanup(rn.Wait)
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 3}},
	})
	s := New(Options{
		Registry:    reg,
		Runner:      rn,
		Tracker:     tr,
		Logger:      logger,
		Events:      rec,
		Concurrency: mgr,
		Overrides:   ov,
		Version:     testVersion,
	})
	return s, reg, ov, rec
}

func countKind(rec *events.Recorder, kind string) int {
	n := 0
	for _, ev := range rec.List(0) {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func TestDisabledHookDeliveryRejected503(t *testing.T) {
	s, reg, ov, rec := newOverrideTestServer(t)
	reg.Set(&hooks.Hook{ID: "runaway", Command: []string{"x"}})
	_, err := ov.SetHookDisabled("runaway", true)
	require.NoError(t, err)

	// Hook port: delivery rejected with the loud, distinct .
	req := httptest.NewRequest(http.MethodPost, "/hook/runaway", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "hook disabled by operator")

	// Admin port dispatch is gated too — re-enable to run.
	req = httptest.NewRequest(http.MethodPost, "/hook/runaway", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	// The rejection is on the activity feed, hook-scoped.
	assert.Equal(t, 2, countKind(rec, "hook.disabled_rejected"))
	byHook := rec.ListByHook("runaway", 0)
	require.NotEmpty(t, byHook)
	assert.Equal(t, "hook.disabled_rejected", byHook[0].Kind)

	// The hook stays loaded/registered: it still lists, flagged disabled.
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks", nil))
	require.Equal(t, 200, w.Code)
	var list []struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "runaway", list[0].ID)
	assert.True(t, list[0].Disabled)
}

func TestDisableEnableEndpointsFlipAndAreIdempotent(t *testing.T) {
	s, reg, ov, rec := newOverrideTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	post := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		return w
	}

	// Disable: flips, persists, event.
	w := post("/hooks/h/disable")
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"disabled": true`)
	assert.True(t, ov.HookDisabled("h", true))
	assert.Equal(t, 1, countKind(rec, "hook.disabled"))

	// Idempotent repeat: still , but NOT another flip event.
	w = post("/hooks/h/disable")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, 1, countKind(rec, "hook.disabled"), "an idempotent repeat is not a flip")

	// Enable: flips back, event; repeat is idempotent.
	w = post("/hooks/h/enable")
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"disabled": false`)
	assert.False(t, ov.HookDisabled("h", true))
	assert.Equal(t, 1, countKind(rec, "hook.enabled"))
	w = post("/hooks/h/enable")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, 1, countKind(rec, "hook.enabled"))

	// Enabled again: deliveries flow (mock docker: the run is accepted).
	req := httptest.NewRequest(http.MethodPost, "/hook/h", strings.NewReader(`{}`))
	wr := httptest.NewRecorder()
	hook(s).ServeHTTP(wr, req)
	require.Equal(t, http.StatusAccepted, wr.Code)
}

// hook.json `enable: false` loads a hook DISABLED by default, with nothing
// stored; the operator endpoints write an explicit persisted override that
// wins over the default in both directions. GET /hooks (and the dispatch
// gate) expose the EFFECTIVE state.
func TestEnableFalseDefaultAndOverridePrecedence(t *testing.T) {
	s, reg, ov, _ := newOverrideTestServer(t)
	off := false
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}, Enable: &off})

	listDisabled := func() bool {
		w := httptest.NewRecorder()
		admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks", nil))
		require.Equal(t, 200, w.Code)
		var list []struct {
			ID       string `json:"id"`
			Disabled bool   `json:"disabled"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
		require.Len(t, list, 1)
		return list[0].Disabled
	}

	// Born disabled: listed disabled and deliveries — with NO override.
	assert.True(t, listDisabled(), "enable:false must load the hook disabled")
	_, hasOverride := ov.HookOverride("h")
	assert.False(t, hasOverride, "the default needs no stored override")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hook/h", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	// POST enable writes an explicit override that beats the default.
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hooks/h/enable", nil))
	require.Equal(t, 200, w.Code)
	assert.False(t, listDisabled(), "the explicit enable override must win over enable:false")
	enabled, ok := ov.HookOverride("h")
	require.True(t, ok)
	assert.True(t, enabled)
	w = httptest.NewRecorder()
	hook(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hook/h", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusAccepted, w.Code, "an explicitly enabled hook dispatches")

	// Disable pins it off again — explicit in the other direction.
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hooks/h/disable", nil))
	require.Equal(t, 200, w.Code)
	assert.True(t, listDisabled())
}

func TestDisableUnknownHook404(t *testing.T) {
	s, _, _, _ := newOverrideTestServer(t)
	for _, path := range []string{"/hooks/nope/disable", "/hooks/nope/enable"} {
		w := httptest.NewRecorder()
		admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, http.StatusNotFound, w.Code, path)
	}
}

func TestHookDetailExposesDisabled(t *testing.T) {
	s, reg, ov, _ := newOverrideTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	get := func() HookDetail {
		w := httptest.NewRecorder()
		admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks/h", nil))
		require.Equal(t, 200, w.Code)
		var d HookDetail
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
		return d
	}

	assert.False(t, get().Disabled)
	_, err := ov.SetHookDisabled("h", true)
	require.NoError(t, err)
	assert.True(t, get().Disabled)
}

// A persist failure must be loud: with the reason, an
// override.write_failed event, and the in-memory state rolled back.
func TestDisablePersistFailureIsLoud(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	ovDir := filepath.Join(dir, "ov")
	ov, err := overrides.Open(filepath.Join(ovDir, "overrides.json"))
	require.NoError(t, err)
	// Sabotage the store's directory so the next persist fails.
	require.NoError(t, os.RemoveAll(ovDir))
	require.NoError(t, os.WriteFile(ovDir, []byte("not a dir"), 0o644))

	rec := events.NewRecorder(100)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	// Drain in-flight runs before the test's TempDir is removed.
	t.Cleanup(rn.Wait)
	s := New(Options{
		Registry: reg, Runner: rn, Tracker: tr, Logger: logger,
		Events: rec, Overrides: ov, Version: testVersion,
	})
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hooks/h/disable", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "persist override")
	assert.Equal(t, 1, countKind(rec, "override.write_failed"))
	assert.False(t, ov.HookDisabled("h", true), "failed persist must roll the flip back")
	assert.Zero(t, countKind(rec, "hook.disabled"), "a rolled-back flip is not a flip")
}

func putLimit(s *Server, group, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/concurrency/"+group+"/limit", strings.NewReader(body))
	admin(s).ServeHTTP(w, req)
	return w
}

func TestConcurrencyOverridePutValidation(t *testing.T) {
	s, _, _, _ := newOverrideTestServer(t)

	// A limit would deadlock queued runs: rejected, disable hooks instead. (The encoder HTML-escapes ">", so assert on the deadlock phrase.)
	w := putLimit(s, "g", `{"limit": 0}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "would deadlock queued runs")

	w = putLimit(s, "g", `{"limit": -2}`)
	require.Equal(t, http.StatusBadRequest, w.Code)

	w = putLimit(s, "g", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)

	w = putLimit(s, "g", `not json`)
	require.Equal(t, http.StatusBadRequest, w.Code)

	w = putLimit(s, "g", `{"limit": 2, "typo": true}`)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// Unknown group: a typo must not create a phantom override.
	w = putLimit(s, "nope", `{"limit": 2}`)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestConcurrencyOverrideSetAndClear(t *testing.T) {
	s, _, ov, rec := newOverrideTestServer(t)

	getStatus := func() []concurrency.GroupStatus {
		w := httptest.NewRecorder()
		admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
		require.Equal(t, 200, w.Code)
		var doc struct {
			Groups []concurrency.GroupStatus `json:"groups"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
		return doc.Groups
	}

	// Baseline: declared , not overridden.
	st := getStatus()
	require.Len(t, st, 1)
	assert.Equal(t, 3, st[0].Limit)
	assert.Equal(t, 3, st[0].Declared)
	assert.False(t, st[0].Overridden)

	// Override to : persisted, applied live, event with declared + effective in the message.
	w := putLimit(s, "g", `{"limit": 1}`)
	require.Equal(t, 200, w.Code)
	st = getStatus()
	assert.Equal(t, 1, st[0].Limit)
	assert.Equal(t, 3, st[0].Declared)
	assert.True(t, st[0].Overridden)
	n, ok := ov.ConcurrencyLimit("g")
	require.True(t, ok)
	assert.Equal(t, 1, n)
	require.Equal(t, 1, countKind(rec, "concurrency.overridden"))
	for _, ev := range rec.List(0) {
		if ev.Kind == "concurrency.overridden" {
			assert.Contains(t, ev.Msg, "overridden to 1")
			assert.Contains(t, ev.Msg, "declared 3")
		}
	}

	// Same value again: idempotent, no event.
	w = putLimit(s, "g", `{"limit": 1}`)
	require.Equal(t, 200, w.Code)
	assert.Equal(t, 1, countKind(rec, "concurrency.overridden"))

	// Clear: reverts to declared, persisted, event.
	req := httptest.NewRequest(http.MethodDelete, "/concurrency/g/limit", nil)
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	st = getStatus()
	assert.Equal(t, 3, st[0].Limit)
	assert.False(t, st[0].Overridden)
	_, ok = ov.ConcurrencyLimit("g")
	assert.False(t, ok)
	assert.Equal(t, 1, countKind(rec, "concurrency.override_cleared"))

	// Clearing again is idempotent (, no event); a name that is neither declared nor overridden is a .
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/concurrency/g/limit", nil))
	require.Equal(t, 200, w.Code)
	assert.Equal(t, 1, countKind(rec, "concurrency.override_cleared"))
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/concurrency/nope/limit", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}

// DELETE must also clear an ORPHANED override (group no longer declared) so
// the operator can clean up — that path must not .
func TestConcurrencyOverrideClearOrphan(t *testing.T) {
	s, _, ov, rec := newOverrideTestServer(t)
	_, err := ov.SetConcurrencyLimit("gone", 2) // stored, but "gone" is not declared
	require.NoError(t, err)

	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/concurrency/gone/limit", nil))
	require.Equal(t, 200, w.Code)
	_, ok := ov.ConcurrencyLimit("gone")
	assert.False(t, ok)
	require.Equal(t, 1, countKind(rec, "concurrency.override_cleared"))
	for _, ev := range rec.List(0) {
		if ev.Kind == "concurrency.override_cleared" {
			assert.Contains(t, ev.Msg, "orphaned")
		}
	}
}

// Without an overrides store the write endpoints fail loudly (), never
// silently no-op; reads treat every hook as enabled.
func TestOverrideEndpointsWithoutStore(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hooks/h/disable", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "override store not configured")

	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks", nil))
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"disabled": false`)
}
