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

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"

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
	})
	return s, reg, tr, rn
}

func TestHealth(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"ok"`)
}

func TestListHooks(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "a", Description: "alpha", Image: "alpine", Command: []string{"x"},
		Secret: "supersecret",
	})
	reg.Set(&hooks.Hook{ID: "b", Description: "beta", Image: "alpine", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodGet, "/hooks", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)

	var got []hooks.Summary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ID)
	assert.Equal(t, "b", got[1].ID)
	assert.NotContains(t, rec.Body.String(), "supersecret")
}

func TestTriggerInvalidSignature(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID:      "secret-hook",
		Image:   "alpine",
		Command: []string{"echo"},
		Secret:  "abc",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/secret-hook", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerValidSignature(t *testing.T) {
	s, reg, tr, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID:      "ok",
		Image:   "alpine",
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
	s.ServeHTTP(rec, req)
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
	reg.Set(&hooks.Hook{
		ID:      "sync",
		Image:   "alpine",
		Command: []string{"x"},
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/sync?wait=true", strings.NewReader(``))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var snap runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	rn.Wait()
}

func TestTriggerInvalidWaitParam(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodPost, "/hook/h?wait=banana", strings.NewReader(``))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTriggerInvalidTimeoutParam(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodPost, "/hook/h?wait=true&timeout=banana", strings.NewReader(``))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTriggerNotFound(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/hook/missing", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetRunNotFound(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/runs/zzz", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
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
	s.ServeHTTP(rec, req)
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
	s.ServeHTTP(rec, req)
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
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
	// list view drops output for compactness
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
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

func TestDashboardServesIndex(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<html")
}

func TestDashboardServesAssets(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	for _, p := range []string{"/dashboard.css", "/dashboard.js"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "path %s", p)
	}
}

func TestDashboardUnknownPath404(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/typo", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTriggerBodyTooLarge(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}})

	big := strings.Repeat("x", MaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/hook/h", strings.NewReader(big))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestTriggerAPIKeyValid(t *testing.T) {
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Image: "alpine", Command: []string{"x"},
		APIKey: "test-key-123",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "test-key-123")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()
}

func TestTriggerAPIKeyInvalid(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Image: "alpine", Command: []string{"x"},
		APIKey: "test-key-123",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("X-API-Key", "wrong-key")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeyMissing(t *testing.T) {
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Image: "alpine", Command: []string{"x"},
		APIKey: "test-key-123",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerAPIKeyCustomHeader(t *testing.T) {
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ak", Image: "alpine", Command: []string{"x"},
		APIKey: "mykey", APIKeyHeader: "Authorization",
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ak", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "mykey")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()
}

func TestTriggerEd25519Valid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ed", Image: "alpine", Command: []string{"x"},
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	})

	body := []byte(`{"signed":"payload"}`)
	sig := ed25519.Sign(priv, body)

	req := httptest.NewRequest(http.MethodPost, "/hook/ed", strings.NewReader(string(body)))
	req.Header.Set("X-Signature-Ed25519", base64.StdEncoding.EncodeToString(sig))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rn.Wait()
}

func TestTriggerEd25519Invalid(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ed", Image: "alpine", Command: []string{"x"},
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	})

	req := httptest.NewRequest(http.MethodPost, "/hook/ed", strings.NewReader(`{}`))
	req.Header.Set("X-Signature-Ed25519", base64.StdEncoding.EncodeToString(make([]byte, 64)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTriggerEd25519WrongKey(t *testing.T) {
	pub1, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, priv2, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{
		ID: "ed", Image: "alpine", Command: []string{"x"},
		PublicKey: base64.StdEncoding.EncodeToString(pub1),
	})

	body := []byte(`{"wrong":"key"}`)
	sig := ed25519.Sign(priv2, body)

	req := httptest.NewRequest(http.MethodPost, "/hook/ed", strings.NewReader(string(body)))
	req.Header.Set("X-Signature-Ed25519", base64.StdEncoding.EncodeToString(sig))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
