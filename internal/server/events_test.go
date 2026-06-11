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

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func TestEventsAndImagesEndpoints(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	reg := hooks.NewRegistry()
	hookDir := filepath.Join(dir, "h")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"}, SourcePath: filepath.Join(hookDir, "hook.json")})
	tr := runs.NewTracker()
	rec := events.NewRecorder(10)
	rec.Record("github.push", "push to repo-x", nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Events: rec})

	evRec := httptest.NewRecorder()
	admin(s).ServeHTTP(evRec, httptest.NewRequest(http.MethodGet, "/events", nil))
	require.Equal(t, http.StatusOK, evRec.Code)
	assert.Contains(t, evRec.Body.String(), "github.push")
	assert.Contains(t, evRec.Body.String(), "push to repo-x")

	// The exit-0 mock docker reports every tag as built and lists no
	// images on disk.
	imRec := httptest.NewRecorder()
	admin(s).ServeHTTP(imRec, httptest.NewRequest(http.MethodGet, "/images", nil))
	require.Equal(t, http.StatusOK, imRec.Code)
	var imgs []runner.ImageStatus
	require.NoError(t, json.Unmarshal(imRec.Body.Bytes(), &imgs))
	require.Len(t, imgs, 1)
	assert.Equal(t, "h", imgs[0].HookID)
	assert.True(t, imgs[0].Built)
	assert.Contains(t, imgs[0].Tag, "whr-hook/h:")

	// Internal state stays off the public hook port.
	for _, p := range []string{"/events", "/images"} {
		r := httptest.NewRecorder()
		hook(s).ServeHTTP(r, httptest.NewRequest(http.MethodGet, p, nil))
		assert.Equal(t, http.StatusNotFound, r.Code, p)
	}
}

func TestDescribePush(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/master","after":"abcdef1234567890","repository":{"full_name":"o/r"},"pusher":{"name":"alice"},"commits":[{"message":"fix things\n\ndetails"}]}`)
	msg := describePush(body)
	assert.Contains(t, msg, "o/r")
	assert.Contains(t, msg, "refs/heads/master")
	assert.Contains(t, msg, "abcdef123456")
	assert.Contains(t, msg, "alice")
	assert.Contains(t, msg, "fix things")
	assert.NotContains(t, msg, "details")
	assert.Equal(t, "push webhook received (unparsed payload)", describePush([]byte("notjson")))
}

func TestReloadWebhookRecordsEvents(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	rec := events.NewRecorder(10)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Events: rec,
		ReloadSecret: "sec", OnReload: func() error { return nil }})

	body := []byte(`{"repository":{"full_name":"o/r"},"ref":"refs/heads/master","after":"abc","pusher":{"name":"p"}}`)
	mac := hmac.New(sha256.New, []byte("sec"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	kinds := []string{}
	for _, e := range rec.List(0) {
		kinds = append(kinds, e.Kind)
	}
	assert.Contains(t, kinds, "github.push")

	// A bad signature is itself an event.
	req = httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader("{}"))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w = httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "reload.denied", rec.List(1)[0].Kind)
}
