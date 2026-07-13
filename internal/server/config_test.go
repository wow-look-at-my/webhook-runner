package server

// /config secret hygiene: the admin surface returns secret METADATA,
// never secret material.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// The reload secret's value must NEVER appear on /config — only the fact
// that one is configured (the admin surface returns secret metadata, never
// secret material).
func TestConfigNeverLeaksReloadSecret(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Options{
		Registry:     hooks.NewRegistry(),
		Tracker:      runs.NewTracker(),
		Logger:       logger,
		ReloadSecret: "super-secret-value-1234",
	})
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "super-secret-value-1234")
	assert.Contains(t, body, `"reload_secret_configured": "true"`)

	// Unset: the flag is absent entirely.
	s2 := New(Options{Registry: hooks.NewRegistry(), Tracker: runs.NewTracker(), Logger: logger})
	rec2 := httptest.NewRecorder()
	admin(s2).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/config", nil))
	assert.NotContains(t, rec2.Body.String(), "reload_secret")
}
