package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func TestHookDetail(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	reg := hooks.NewRegistry()
	hookDir := filepath.Join(dir, "d")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	reg.Set(&hooks.Hook{
		ID:               "d",
		Description:      "detail hook",
		Command:          []string{"x"},
		SourcePath:       filepath.Join(hookDir, "hook.json"),
		APIKey:           "super-secret-key",
		Settings:         json.RawMessage(`{"token":"hunter2","ai_url":"https://model.internal"}`),
		Schedule:         "5m",
		ConcurrencyGroup: "g",
		State:            true,
		TimeoutRaw:       "90s",
	})
	tr := runs.NewTracker()
	tr.New("d").Finish(runs.StatusSuccess, 0, "")
	tr.New("d").Finish(runs.StatusFailure, 1, "")
	tr.New("d") // still pending
	tr.New("unrelated").Finish(runs.StatusSuccess, 0, "")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(dir, "kv")}, []byte("s"), logger)
	require.NoError(t, err)
	require.NoError(t, store.Set("d", "cursor", []byte("abc"), 0))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, KV: store})

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/d", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got HookDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	// Config summary: features named, values never present.
	assert.Equal(t, "d", got.Info.ID)
	assert.Equal(t, "detail hook", got.Info.Description)
	assert.Equal(t, "5m", got.Info.Schedule)
	assert.Equal(t, "g", got.Info.ConcurrencyGroup)
	assert.True(t, got.Info.State)
	assert.Equal(t, "1m30s", got.Info.Timeout)
	assert.True(t, got.Info.APIKey)
	// Settings KEYS only: this port is operator-only, but a settings document
	// can hold credentials, so no value ever renders here.
	assert.Equal(t, []string{"ai_url", "token"}, got.Info.SettingsKeys)
	body := rec.Body.String()
	assert.NotContains(t, body, "super-secret-key")
	assert.NotContains(t, body, "hunter2")
	assert.NotContains(t, body, "model.internal")

	// Image state (the exit-0 mock docker reports the tag as built).
	assert.Equal(t, "d", got.Image.HookID)
	assert.True(t, got.Image.Built)
	assert.Contains(t, got.Image.Tag, "whr-hook/d:")

	// KV namespace stats — counts only, never values.
	require.NotNil(t, got.KV)
	assert.Equal(t, "d", got.KV.Namespace)
	assert.Equal(t, 1, got.KV.Keys)
	assert.Equal(t, 3, got.KV.Bytes)

	// Run stats over the bounded window: only this hook's runs.
	assert.Equal(t, 3, got.Stats.Tracked)
	assert.Equal(t, runs.MaxRunsPerHook, got.Stats.MaxTracked)
	assert.Equal(t, 2, got.Stats.Completed)
	assert.InDelta(t, 0.5, got.Stats.SuccessRate, 1e-9)
	require.NotNil(t, got.Stats.LastRun)
	assert.Equal(t, runs.StatusPending, got.Stats.LastRun.Status)
}

func TestHookDetailDefaultsAndNoKV(t *testing.T) {
	s, reg, _, _ := newTestServer(t) // no KV store configured
	hookDir := t.TempDir()
	reg.Set(&hooks.Hook{ID: "plain", Command: []string{"x"},
		SourcePath: filepath.Join(hookDir, "hook.json")})

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/plain", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got HookDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, hooks.DefaultTimeout.String(), got.Info.Timeout)
	assert.False(t, got.Info.APIKey)
	assert.Empty(t, got.Info.SettingsKeys)
	assert.Nil(t, got.KV)
	assert.Equal(t, 0, got.Stats.Tracked)
	assert.Nil(t, got.Stats.LastRun)
}

func TestHookDetailUnknownIs404(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no such hook")
}

func TestHookDetailNotOnHookPort(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "d", Command: []string{"x"}})
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/d", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEventsFilterByHook(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	rec := events.NewRecorder(10)
	rec.Record("run.started", "a run", map[string]string{"hook": "a"})
	rec.Record("reload.requested", "global", nil)
	rec.Record("run.started", "b run", map[string]string{"hook": "b"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Events: rec})

	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/events?hook=a", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var got []events.Event
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "a run", got[0].Msg)

	// Unfiltered behavior is unchanged.
	w = httptest.NewRecorder()
	admin(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/events", nil))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Len(t, got, 3)
}
