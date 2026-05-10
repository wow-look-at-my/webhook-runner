package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func newTestServer(t *testing.T) (*Server, *hooks.Registry, *runs.Tracker) {
	t.Helper()
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	s := New(Options{
		Registry: reg,
		Tracker:  tr,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return s, reg, tr
}

func TestHealth(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestListHooks(t *testing.T) {
	s, reg, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "a", Description: "alpha", Image: "alpine", Command: []string{"x"}})
	reg.Set(&hooks.Hook{ID: "b", Description: "beta", Image: "alpine", Command: []string{"x"}})

	req := httptest.NewRequest(http.MethodGet, "/hooks", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var got []hooks.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("got %+v", got)
	}
	// secrets are not exposed
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("secret leaked: %s", rec.Body.String())
	}
}

func TestTriggerInvalidSignature(t *testing.T) {
	s, reg, _ := newTestServer(t)
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
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTriggerNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/hook/missing", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestGetRunNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/runs/zzz", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}
