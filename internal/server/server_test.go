package server

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
	return s, reg, tr, rn
}

func hook(s *Server) http.Handler  { return s.HookHandler() }
func admin(s *Server) http.Handler { return s.AdminHandler() }

func TestHealthHookPort(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"ok"`)
}

func TestHealthAdminPort(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"ok"`)
}

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
	for _, p := range []string{"/dashboard.css", "/dashboard.js"} {
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

func TestTriggerEd25519Valid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ed", Command: []string{"x"},
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	})

	body := []byte(`{"signed":"payload"}`)
	sig := ed25519.Sign(priv, body)

	req := httptest.NewRequest(http.MethodPost, "/hook/ed", strings.NewReader(string(body)))
	req.Header.Set("X-Signature-Ed25519", base64.StdEncoding.EncodeToString(sig))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()
}

func TestTriggerEd25519Invalid(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ed", Command: []string{"x"},
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ed", strings.NewReader(`{}`))
	req.Header.Set("X-Signature-Ed25519", base64.StdEncoding.EncodeToString(make([]byte, 64)))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerEd25519WrongKey(t *testing.T) {
	pub1, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, priv2, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ed", Command: []string{"x"},
		PublicKey: base64.StdEncoding.EncodeToString(pub1),
	})

	body := []byte(`{"wrong":"key"}`)
	sig := ed25519.Sign(priv2, body)

	req := httptest.NewRequest(http.MethodPost, "/hook/ed", strings.NewReader(string(body)))
	req.Header.Set("X-Signature-Ed25519", base64.StdEncoding.EncodeToString(sig))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestReloadAdminPort(t *testing.T) {
	reloaded := false
	s := New(Options{
		Registry: hooks.NewRegistry(),
		Tracker:  runs.NewTracker(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnReload: func() error { reloaded = true; return nil },
	})

	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, reloaded)
	assert.Contains(t, rec.Body.String(), "reloaded")
}

func TestReloadWebhookValid(t *testing.T) {
	reloaded := false
	secret := "test-reload-secret"
	s := New(Options{
		Registry:     hooks.NewRegistry(),
		Tracker:      runs.NewTracker(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnReload:     func() error { reloaded = true; return nil },
		ReloadSecret: secret,
	})

	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, reloaded)
}

func TestReloadWebhookBadSignature(t *testing.T) {
	s := New(Options{
		Registry:     hooks.NewRegistry(),
		Tracker:      runs.NewTracker(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnReload:     func() error { return nil },
		ReloadSecret: "real-secret",
	})

	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=0000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestReloadWebhookNotRegisteredWithoutSecret(t *testing.T) {
	s := New(Options{
		Registry: hooks.NewRegistry(),
		Tracker:  runs.NewTracker(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestConfigEndpoint(t *testing.T) {
	s := New(Options{
		Registry:     hooks.NewRegistry(),
		Tracker:      runs.NewTracker(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HooksRepo:    "git@github.com:wow-look-at-my/webhooks.git",
		HookBaseURL:  "https://hooks.example.com",
		ReloadSecret: "my-secret",
	})

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var cfg map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&cfg))
	assert.Equal(t, "git@github.com:wow-look-at-my/webhooks.git", cfg["hooks_repo"])
	assert.Equal(t, "https://hooks.example.com", cfg["hook_base_url"])
	assert.Equal(t, "my-secret", cfg["reload_secret"])
}

func TestConfigEndpointEmpty(t *testing.T) {
	s := New(Options{
		Registry: hooks.NewRegistry(),
		Tracker:  runs.NewTracker(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var cfg map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&cfg))
	assert.Empty(t, cfg)
}

func TestCancelRunHookPort(t *testing.T) {
	s, reg, tr, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}, APIKey: "k"})
	run := tr.New("h") // still pending — never handed to the runner

	// Wrong key is rejected before the run is even looked up.
	req := httptest.NewRequest(http.MethodPost, "/hook/h/cancel/"+run.ID(), nil)
	req.Header.Set("X-API-Key", "wrong")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/hook/h/cancel/"+run.ID(), nil)
	req.Header.Set("X-API-Key", "k")
	rec = httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), "cancelling")

	select {
	case <-run.Cancelled():
	default:
		t.Error("run not flagged for cancel")
	}
}

func TestCancelRunCrossHookIsNotFound(t *testing.T) {
	// Hook a's valid key must not cancel (or detect) hook b's runs.
	s, reg, tr, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "a", Command: []string{"x"}, APIKey: "ka"})
	reg.Set(&hooks.Hook{ID: "b", Command: []string{"x"}, APIKey: "kb"})
	run := tr.New("b")

	req := httptest.NewRequest(http.MethodPost, "/hook/a/cancel/"+run.ID(), nil)
	req.Header.Set("X-API-Key", "ka")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	select {
	case <-run.Cancelled():
		t.Error("cross-hook cancel must not flag the run")
	default:
	}
}

func TestCancelRunUnknownHookOrRun(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}, APIKey: "k"})

	req := httptest.NewRequest(http.MethodPost, "/hook/nope/cancel/xyz", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/hook/h/cancel/doesnotexist", nil)
	req.Header.Set("X-API-Key", "k")
	rec = httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCancelFinishedRunConflict(t *testing.T) {
	s, reg, tr, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}, APIKey: "k"})
	run := tr.New("h")
	run.Finish(runs.StatusSuccess, 0, "")

	req := httptest.NewRequest(http.MethodPost, "/hook/h/cancel/"+run.ID(), nil)
	req.Header.Set("X-API-Key", "k")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "already finished")
}

func TestCancelRunAdminPort(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	run := tr.New("h")

	req := httptest.NewRequest(http.MethodPost, "/runs/"+run.ID()+"/cancel", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	select {
	case <-run.Cancelled():
	default:
		t.Error("run not flagged for cancel")
	}
}

func TestTriggerAPIKeyFromHostEnv(t *testing.T) {
	t.Setenv("WHR_TEST_API_KEY", "sesame")
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Command: []string{"x"},
		APIKey: "${WHR_TEST_API_KEY}",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "sesame")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()

	// The literal reference string is not a valid key.
	req = httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "${WHR_TEST_API_KEY}")
	rec = httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeyFromHostEnvUnsetFailsClosed(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Command: []string{"x"},
		APIKey: "${WHR_TEST_DEFINITELY_UNSET_KEY}",
	})

	// An unset reference must reject everything — even an empty key header.
	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeyFromSopsSecrets(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	mockSops := filepath.Join(dir, "sops")
	require.NoError(t, os.WriteFile(mockSops, []byte("#!/bin/sh\nfor a; do f=\"$a\"; done\ncat \"$f\"\n"), 0o755))

	hookDir := filepath.Join(dir, "ak")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, hooks.SecretsFileName), []byte("AK_FROM_SOPS=open-sesame\n"), 0o600))

	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	secrets := hooks.NewSecretsLoader(mockSops)
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker, Secrets: secrets})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Secrets: secrets})

	reg.Set(&hooks.Hook{
		ID: "ak", SourcePath: filepath.Join(hookDir, "hook.json"),
		Command: []string{"x"},
		APIKey:  "${AK_FROM_SOPS}",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "open-sesame")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()

	req = httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "wrong")
	rec = httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeySecretsDecryptFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	failingSops := filepath.Join(dir, "sops")
	require.NoError(t, os.WriteFile(failingSops, []byte("#!/bin/sh\nexit 1\n"), 0o755))

	hookDir := filepath.Join(dir, "ak")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, hooks.SecretsFileName), []byte("K=v\n"), 0o600))

	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	secrets := hooks.NewSecretsLoader(failingSops)
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker, Secrets: secrets})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Secrets: secrets})

	reg.Set(&hooks.Hook{
		ID: "ak", SourcePath: filepath.Join(hookDir, "hook.json"),
		Command: []string{"x"},
		APIKey:  "${K}",
	})

	// Even a request that would match the (undecryptable) key fails closed.
	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "v")
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
