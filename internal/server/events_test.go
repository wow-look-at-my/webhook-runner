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

// Rejected requests must be visible on the dashboard: a caller hitting a
// wrong URL, a stale key, or a hook whose api_key reference doesn't resolve
// is exactly the misconfiguration the activity feed exists to answer
// "did you receive anything?" about.
func TestTriggerRejectionsRecordEvents(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	reg := hooks.NewRegistry()
	reg.Set(&hooks.Hook{ID: "locked", APIKey: "right-key"})
	reg.Set(&hooks.Hook{ID: "broken", APIKey: "${WHR_TEST_UNSET_REF_X9}"})
	tr := runs.NewTracker()
	rec := events.NewRecorder(20)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Events: rec})

	post := func(path, key string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		hook(s).ServeHTTP(w, req)
		return w.Code
	}
	latest := func() events.Event { return rec.List(1)[0] }

	// Unknown hook id: 404 + hook.unknown, on both trigger and cancel.
	require.Equal(t, http.StatusNotFound, post("/hook/nope", ""))
	assert.Equal(t, "hook.unknown", latest().Kind)
	assert.Contains(t, latest().Msg, "nope")
	require.Equal(t, http.StatusNotFound, post("/hook/gone/cancel/abc123", ""))
	assert.Equal(t, "hook.unknown", latest().Kind)
	assert.Contains(t, latest().Msg, "cancel")

	// Wrong key: 401 + hook.denied — and the presented key must never appear
	// in the feed.
	require.Equal(t, http.StatusUnauthorized, post("/hook/locked", "wrong-key"))
	assert.Equal(t, "hook.denied", latest().Kind)
	assert.Contains(t, latest().Msg, "locked")
	assert.NotContains(t, latest().Msg, "wrong-key")
	require.Equal(t, http.StatusUnauthorized, post("/hook/locked/cancel/abc123", "wrong-key"))
	assert.Equal(t, "hook.denied", latest().Kind)
	assert.Contains(t, latest().Msg, "cancel")

	// Unresolvable api_key reference: the caller sees a generic 401 (no
	// config detail for anonymous callers), the operator sees
	// hook.misconfigured naming the exact broken reference.
	require.Equal(t, http.StatusUnauthorized, post("/hook/broken", "anything"))
	kinds := []string{}
	misconfigured := ""
	for _, e := range rec.List(0) {
		kinds = append(kinds, e.Kind)
		if e.Kind == "hook.misconfigured" {
			misconfigured = e.Msg
		}
	}
	assert.Contains(t, kinds, "hook.misconfigured")
	assert.Contains(t, kinds, "hook.denied")
	assert.Contains(t, misconfigured, "${WHR_TEST_UNSET_REF_X9}")
}

// An env value whose ${NAME} reference resolves to nothing makes the
// container run with an empty value — invisible from outside while every
// run fails downstream. The runner must surface it in the activity feed.
func TestRunRecordsEnvUnresolved(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	reg := hooks.NewRegistry()
	hookDir := filepath.Join(dir, "e")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	reg.Set(&hooks.Hook{
		ID:          "e",
		Command:     []string{"x"},
		Env:         map[string]string{"X": "${WHR_TEST_UNSET_ENV_X9}"},
		Synchronous: true,
		SourcePath:  filepath.Join(hookDir, "hook.json"),
	})
	tr := runs.NewTracker()
	rec := events.NewRecorder(20)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker, Events: rec})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, Events: rec})

	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hook/e", strings.NewReader("{}")))
	require.Equal(t, http.StatusOK, w.Code)

	unresolved := ""
	for _, e := range rec.List(0) {
		if e.Kind == "env.unresolved" {
			unresolved = e.Msg
		}
	}
	require.NotEmpty(t, unresolved, "expected an env.unresolved event")
	assert.Contains(t, unresolved, "${WHR_TEST_UNSET_ENV_X9}")
	assert.Contains(t, unresolved, "env X")
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
