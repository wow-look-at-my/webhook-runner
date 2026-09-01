package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// fakeGate records what the server hands it and answers a scripted verdict.
type fakeGate struct {
	calls  int
	event  string
	body   []byte
	status string
	err    error
}

func (f *fakeGate) HandleEvent(event string, body []byte) (string, error) {
	f.calls++
	f.event = event
	f.body = body
	return f.status, f.err
}

func signReload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newGateTestServer(t *testing.T, gate ReloadGate, onReload func() error, rec *events.Recorder) *Server {
	t.Helper()
	return New(Options{
		Registry:     hooks.NewRegistry(),
		Tracker:      runs.NewTracker(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events:       rec,
		OnReload:     onReload,
		ReloadSecret: "gate-secret",
		Gate:         gate,
	})
}

func recordedKinds(rec *events.Recorder) []string {
	evs := rec.List(0)
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func TestReloadWebhookGateWired(t *testing.T) {
	gate := &fakeGate{status: "held"}
	reloads := 0
	rec := events.NewRecorder(50)
	s := newGateTestServer(t, gate, func() error { reloads++; return nil }, rec)

	body := []byte(`{"sha":"abc","state":"success","context":"all-builds"}`)
	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", signReload("gate-secret", body))
	req.Header.Set("X-GitHub-Event", "status")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"held"`)
	assert.Equal(t, 1, gate.calls)
	assert.Equal(t, "status", gate.event)
	assert.Equal(t, body, gate.body)
	assert.Zero(t, reloads, "the gate replaces the legacy OnReload path on /_reload")
	// A status delivery is not a push: no github.push feed entry.
	assert.NotContains(t, recordedKinds(rec), "github.push")
}

func TestReloadWebhookGatePushKeepsFeedEntry(t *testing.T) {
	gate := &fakeGate{status: "held"}
	rec := events.NewRecorder(50)
	s := newGateTestServer(t, gate, nil, rec)

	body := []byte(`{"ref":"refs/heads/master","after":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", signReload("gate-secret", body))
	req.Header.Set("X-GitHub-Event", "push")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "push", gate.event)
	assert.Contains(t, recordedKinds(rec), "github.push", "push feed continuity")
}

func TestReloadWebhookGateError(t *testing.T) {
	gate := &fakeGate{err: errors.New("fetch hooks repo: boom")}
	rec := events.NewRecorder(50)
	s := newGateTestServer(t, gate, nil, rec)

	body := []byte(`{"sha":"abc","state":"success","context":"all-builds"}`)
	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", signReload("gate-secret", body))
	req.Header.Set("X-GitHub-Event", "status")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusInternalServerError, w.Code, "a gate error 500s so GitHub records a redeliverable delivery")
	assert.Contains(t, recordedKinds(rec), "reload.failed")
}

func TestReloadWebhookGatePing(t *testing.T) {
	gate := &fakeGate{status: "ignored"}
	reloads := 0
	s := newGateTestServer(t, gate, func() error { reloads++; return nil }, events.NewRecorder(50))

	body := []byte(`{"zen":"Keep it logically awesome."}`)
	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", signReload("gate-secret", body))
	req.Header.Set("X-GitHub-Event", "ping")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "ignored")
	assert.Equal(t, "ping", gate.event)
	assert.Zero(t, reloads)
}

func TestReloadWebhookGateBadSignature(t *testing.T) {
	gate := &fakeGate{status: "held"}
	rec := events.NewRecorder(50)
	s := newGateTestServer(t, gate, nil, rec)

	req := httptest.NewRequest(http.MethodPost, "/_reload", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=0000000000000000000000000000000000000000000000000000000000000000")
	req.Header.Set("X-GitHub-Event", "status")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Zero(t, gate.calls, "an unauthenticated delivery must never reach the gate")
	assert.Contains(t, recordedKinds(rec), "reload.denied")
}

// Admin POST /reload is deliberately untouched by the gate: it runs
// OnReload (wired to gate.Force in gate mode) and records reload.requested
// — the shape the ee test asserts.
func TestReloadAdminPortWithGateForceWiring(t *testing.T) {
	gate := &fakeGate{status: "held"}
	forced := 0
	rec := events.NewRecorder(50)
	s := newGateTestServer(t, gate, func() error { forced++; return nil }, rec)

	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "reloaded")
	assert.Equal(t, 1, forced, "admin /reload runs OnReload (the gate bypass), not HandleEvent")
	assert.Zero(t, gate.calls)
	assert.Contains(t, recordedKinds(rec), "reload.requested")
}
