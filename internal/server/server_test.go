package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func writeMockDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return path
}

// testVersion is the build identity newTestServer injects, so tests can
// assert /health and /version report exactly what the server was given.
var testVersion = VersionInfo{Version: "v1.2.3-test", Revision: "abcdef123456", Time: "2026-07-04T00:00:00Z"}

func newTestServer(t *testing.T) (*Server, *hooks.Registry, *runs.Tracker, *runner.Runner) {
	t.Helper()
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{
		Tracker: tr,
		Logger:  logger,
		TmpDir:  dir,
		Docker:  docker,
	})
	s := New(Options{
		Registry: reg,
		Runner:   rn,
		Tracker:  tr,
		Logger:   logger,
		Version:  testVersion,
	})
	// Drain the runner before the TempDir above is removed. An accepted
	// delivery starts an async run on context.Background(), so it outlives
	// the test body and keeps creating its temp dir under `dir` — which
	// t.TempDir()'s cleanup is meanwhile trying to delete ("unlinkat ...:
	// directory not empty"). Registered here rather than per test because it
	// is a property of this harness, not of any one case: every test that
	// accepts a delivery had the same race waiting for it.
	t.Cleanup(rn.Wait)
	return s, reg, tr, rn
}

func hook(s *Server) http.Handler  { return s.HookHandler() }
func admin(s *Server) http.Handler { return s.AdminHandler() }

func TestListHooks(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "a", Description: "alpha", Command: []string{"x"},
		Secret: "supersecret",
	})
	reg.Set(&hooks.Hook{ID: "b", Description: "beta", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodGet, "/hooks", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)

	var got []hooks.Summary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ID)
	assert.Equal(t, "b", got[1].ID)
	assert.NotContains(t, rec.Body.String(), "supersecret")
}

func TestListHooksNotOnHookPort(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/hooks", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTriggerInvalidSignature(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID:      "secret-hook",
		Command: []string{"echo"},
		Secret:  "abc",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/secret-hook", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerValidSignature(t *testing.T) {
	s, reg, tr, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID:      "ok",
		Command: []string{"echo"},
		Secret:  "abc",
	})

	body := []byte(`{"x":1}`)
	mac := hmac.New(sha256.New, []byte("abc"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/hook/ok", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	id := resp["run_id"]
	assert.NotEmpty(t, id)

	rn.Wait()
	got := tr.Get(id)
	require.NotNil(t, got)
}

func TestTriggerSyncMode(t *testing.T) {
	s, reg, _, rn := newTestServer(t)
	// Sync mode surfaces the run's final status, so this hook needs a real
	// directory for its image-tag content hash to resolve.
	hookDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	reg.Set(&hooks.Hook{
		ID:         "sync",
		Command:    []string{"x"},
		SourcePath: filepath.Join(hookDir, "hook.json"),
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/sync?wait=true", strings.NewReader(``))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var snap runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	rn.Wait()
}

func TestTriggerInvalidWaitParam(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodPost, "/hook/h?wait=banana", strings.NewReader(``))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTriggerInvalidTimeoutParam(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodPost, "/hook/h?wait=true&timeout=banana", strings.NewReader(``))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTriggerNotFound(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/hook/missing", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetRunNotFound(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/runs/zzz", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetRun(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	r := tr.New("h")
	r.AppendOutput("first")
	r.AppendOutput("second")
	r.Finish(runs.StatusSuccess, 0, "")

	req := httptest.NewRequest(http.MethodGet, "/runs/"+r.ID(), nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var snap runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	assert.Equal(t, []string{"first", "second"}, snap.Output)
}

func TestGetRunWithTail(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	r := tr.New("h")
	for i := 0; i < 5; i++ {
		r.AppendOutput("line")
	}
	r.Finish(runs.StatusSuccess, 0, "")

	req := httptest.NewRequest(http.MethodGet, "/runs/"+r.ID()+"?tail=2", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var snap runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	assert.Equal(t, 2, len(snap.Output))
}

func TestListRuns(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	r1 := tr.New("a")
	r1.AppendOutput("verbose output")
	r1.Finish(runs.StatusSuccess, 0, "")
	r2 := tr.New("b")
	r2.Finish(runs.StatusFailure, 1, "")

	req := httptest.NewRequest(http.MethodGet, "/runs", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
	for _, g := range got {
		assert.Empty(t, g.Output)
	}
}

func TestListRunsByHook(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	tr.New("a")
	tr.New("a")
	tr.New("b")

	req := httptest.NewRequest(http.MethodGet, "/runs?hook=a&max=10", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

func TestDashboardServesIndex(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<html")
}

func TestDashboardServesAssets(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	for _, p := range []string{"/dashboard.css", "/dashboard.js", "/timeline.js"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "path %s", p)
	}
}

func TestDashboardUnknownPath404(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/typo", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTriggerBodyTooLarge(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}})

	big := strings.Repeat("x", MaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/hook/h", strings.NewReader(big))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestTriggerAPIKeyValid(t *testing.T) {
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Command: []string{"x"},
		APIKey: "test-key-123",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "test-key-123")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()
}

func TestTriggerAPIKeyInvalid(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Command: []string{"x"},
		APIKey: "test-key-123",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "wrong-key")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeyMissing(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Command: []string{"x"},
		APIKey: "test-key-123",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeyCustomHeader(t *testing.T) {
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Command: []string{"x"},
		APIKey: "mykey", APIKeyHeader: "Authorization",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "mykey")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()
}
