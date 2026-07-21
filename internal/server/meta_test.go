package server

// The plain /health reachability checks on BOTH ports (the health-test
// reorganization that moved them out of server_test.go). The /version
// surface — build identity, hooks_tree states, the dev default — lives in
// version_test.go: this file deliberately does NOT duplicate it (an older
// revision did, against the pre-VersionInfo string API, and the duplicate
// declarations broke the build when both landed on one tree).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
