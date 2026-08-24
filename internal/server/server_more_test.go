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
