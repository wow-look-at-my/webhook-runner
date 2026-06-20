package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

func state(s *Server) http.Handler { return s.StateHandler() }

func newStateServer(t *testing.T, cfg kv.Config) (*Server, *kv.Store) {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "kv")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(cfg, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	s := New(Options{Logger: logger, KV: store})
	return s, store
}

func stateReq(t *testing.T, s *Server, method, target, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	state(s).ServeHTTP(rr, req)
	return rr
}

func TestStateRoundTrip(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	tok := store.Token("my-hook")

	require.Equal(t, http.StatusNotFound, stateReq(t, s, "GET", "/kv/foo", tok, nil).Code)

	require.Equal(t, http.StatusNoContent,
		stateReq(t, s, "PUT", "/kv/foo", tok, strings.NewReader("bar")).Code)

	got := stateReq(t, s, "GET", "/kv/foo", tok, nil)
	require.Equal(t, http.StatusOK, got.Code)
	require.Equal(t, "bar", got.Body.String())

	list := stateReq(t, s, "GET", "/kv", tok, nil)
	require.Equal(t, http.StatusOK, list.Code)
	require.Contains(t, list.Body.String(), "foo")

	require.Equal(t, http.StatusNoContent, stateReq(t, s, "DELETE", "/kv/foo", tok, nil).Code)
	require.Equal(t, http.StatusNotFound, stateReq(t, s, "GET", "/kv/foo", tok, nil).Code)
}

func TestStateIncr(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	tok := store.Token("h")

	first := stateReq(t, s, "POST", "/kv/c/incr", tok, nil)
	require.Equal(t, http.StatusOK, first.Code)
	require.JSONEq(t, `{"value":1}`, first.Body.String())

	withDelta := stateReq(t, s, "POST", "/kv/c/incr", tok, strings.NewReader(`{"delta":5}`))
	require.Equal(t, http.StatusOK, withDelta.Code)
	require.JSONEq(t, `{"value":6}`, withDelta.Body.String())

	// A non-integer value yields 409, not a silent reset.
	require.NoError(t, store.Set("h", "word", []byte("x"), 0))
	require.Equal(t, http.StatusConflict, stateReq(t, s, "POST", "/kv/word/incr", tok, nil).Code)
}

func TestStateAuth(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	tok := store.Token("h")

	require.Equal(t, http.StatusUnauthorized, stateReq(t, s, "GET", "/kv/foo", "", nil).Code)
	require.Equal(t, http.StatusUnauthorized, stateReq(t, s, "GET", "/kv/foo", "h.deadbeef", nil).Code)

	// Namespaces are isolated: h's own token reads its data, a token for
	// "other" gets 404 for the same key.
	require.NoError(t, store.Set("h", "secret", []byte("v"), 0))
	require.Equal(t, http.StatusOK, stateReq(t, s, "GET", "/kv/secret", tok, nil).Code)
	require.Equal(t, http.StatusNotFound,
		stateReq(t, s, "GET", "/kv/secret", store.Token("other"), nil).Code)
}

func TestStateTTL(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	tok := store.Token("h")

	require.Equal(t, http.StatusNoContent,
		stateReq(t, s, "PUT", "/kv/temp?ttl=60", tok, strings.NewReader("v")).Code)
	v, ok := store.Get("h", "temp")
	require.True(t, ok)
	require.Equal(t, "v", string(v))

	// A negative TTL (header form) is a client error.
	req := httptest.NewRequest("PUT", "/kv/temp", strings.NewReader("v"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-KV-TTL", "-5")
	rr := httptest.NewRecorder()
	state(s).ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestStateValueTooLarge(t *testing.T) {
	s, store := newStateServer(t, kv.Config{MaxValueBytes: 4})
	tok := store.Token("h")
	rr := stateReq(t, s, "PUT", "/kv/big", tok, strings.NewReader("123456789"))
	require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
}

func TestStateStoreDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Options{Logger: logger}) // no KV configured
	req := httptest.NewRequest("GET", "/kv/foo", nil)
	req.Header.Set("Authorization", "Bearer x.y")
	rr := httptest.NewRecorder()
	state(s).ServeHTTP(rr, req)
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestAdminKVStats(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	require.NoError(t, store.Set("h", "k", []byte("vv"), 0))
	req := httptest.NewRequest("GET", "/kv", nil)
	rr := httptest.NewRecorder()
	admin(s).ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"namespace": "h"`)
	require.Contains(t, rr.Body.String(), `"keys": 1`)
}

func TestAdminKVStatsNilStore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Options{Logger: logger})
	req := httptest.NewRequest("GET", "/kv", nil)
	rr := httptest.NewRecorder()
	admin(s).ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "[]", strings.TrimSpace(rr.Body.String()))
}
