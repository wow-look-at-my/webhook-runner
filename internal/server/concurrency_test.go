package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func TestConcurrencyEndpointNilSafe(t *testing.T) {
	// The default test server has no manager; the endpoint must still 200.
	s, _, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
	require.Equal(t, 200, rec.Code)
}

func TestConcurrencyEndpointReportsGroups(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"ollama-local": {Limit: 1}},
	})
	s := New(Options{
		Registry:    hooks.NewRegistry(),
		Tracker:     runs.NewTracker(),
		Logger:      logger,
		Concurrency: mgr,
	})
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "ollama-local")
	assert.Contains(t, rec.Body.String(), `"limit"`)
}
