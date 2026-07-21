package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

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

func TestVersionEndpoint(t *testing.T) {
	s := New(Options{Registry: hooks.NewRegistry(), Version: "v9.9.9-test"})
	// /version is served on both the public hook port and the admin port.
	for name, h := range map[string]http.Handler{"hook": hook(s), "admin": admin(s)} {
		req := httptest.NewRequest(http.MethodGet, "/version", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, 200, rec.Code, name)
		assert.Contains(t, rec.Body.String(), "v9.9.9-test", name)
	}

	// /health carries the same version so a single curl confirms a deploy.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"ok"`)
	assert.Contains(t, rec.Body.String(), "v9.9.9-test")
}

func TestVersionDefaultsToDev(t *testing.T) {
	s := New(Options{Registry: hooks.NewRegistry()})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	hook(s).ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"dev"`)
}
