package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bothPorts runs a subtest against the hook and the admin handler — the
// build-identity endpoints are deliberately registered on both.
func bothPorts(t *testing.T, s *Server, fn func(t *testing.T, h http.Handler)) {
	t.Helper()
	for name, h := range map[string]http.Handler{"hook": hook(s), "admin": admin(s)} {
		t.Run(name, func(t *testing.T) { fn(t, h) })
	}
}

func TestVersionEndpoint(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	bothPorts(t, s, func(t *testing.T, h http.Handler) {
		req := httptest.NewRequest(http.MethodGet, "/version", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		var got VersionInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, testVersion, got)
	})
}

func TestHealthIncludesVersion(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	bothPorts(t, s, func(t *testing.T, h http.Handler) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var got struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, "ok", got.Status, "existing health field must be unchanged")
		assert.Equal(t, testVersion.Version, got.Version)
	})
}

// An embedding caller that sets no Version still reports something ("dev",
// the version command's own fallback) rather than an empty string.
func TestVersionDefaultsToDev(t *testing.T) {
	s := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got VersionInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "dev", got.Version)
	assert.Empty(t, got.Revision)
}
