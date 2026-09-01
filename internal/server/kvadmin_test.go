package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

func adminGet(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	admin(s).ServeHTTP(rr, httptest.NewRequest("GET", target, nil))
	return rr
}

// The wire shapes of the inspection endpoints, decoded field-by-field so the tests pin the JSON contract (names, omitempty behavior), not just the Go.
type kvKeyJSON struct {
	Key        string     `json:"key"`
	Size       int        `json:"size"`
	ExpiresAt  *time.Time `json:"expires_at"`
	TTLSeconds *int64     `json:"ttl_seconds"`
}

type kvEntryJSON struct {
	kvKeyJSON
	Namespace   string  `json:"namespace"`
	ValueBase64 string  `json:"value_base64"`
	ValueUTF8   *string `json:"value_utf8"`
}

func TestAdminKVNamespaceList(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	require.NoError(t, store.Set("h", "desc:repo#7", []byte("hash"), 0))
	require.NoError(t, store.Set("h", "pr:repo#1", []byte("sha-one"), time.Hour))
	require.NoError(t, store.Set("h", "pr:repo#2", []byte("gone"), 10*time.Millisecond))
	require.NoError(t, store.Set("other", "k", []byte("elsewhere"), 0))
	time.Sleep(30 * time.Millisecond)

	rr := adminGet(t, s, "/kv/h")
	require.Equal(t, http.StatusOK, rr.Code)
	var got struct {
		Namespace string      `json:"namespace"`
		Keys      []kvKeyJSON `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, "h", got.Namespace)
	// The expired key is invisible (lazy expiry) and other namespaces' keys don't bleed in; results are sorted by key.
	require.Len(t, got.Keys, 2)
	require.Equal(t, "desc:repo#7", got.Keys[0].Key)
	require.Equal(t, 4, got.Keys[0].Size)
	require.Nil(t, got.Keys[0].ExpiresAt, "no-TTL key must omit expires_at")
	require.Nil(t, got.Keys[0].TTLSeconds, "no-TTL key must omit ttl_seconds")
	require.Equal(t, "pr:repo#1", got.Keys[1].Key)
	require.Equal(t, 7, got.Keys[1].Size)
	require.NotNil(t, got.Keys[1].ExpiresAt)
	require.NotNil(t, got.Keys[1].TTLSeconds)
	require.InDelta(t, 3600, float64(*got.Keys[1].TTLSeconds), 5)
	// The listing never ships values.
	require.NotContains(t, rr.Body.String(), "sha-one")

	// ?prefix= narrows the listing.
	rr = adminGet(t, s, "/kv/h?prefix=pr%3A")
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got.Keys, 1)
	require.Equal(t, "pr:repo#1", got.Keys[0].Key)

	// An unknown namespace lists as empty (same semantics as the state API's list), not an error.
	rr = adminGet(t, s, "/kv/nope")
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Empty(t, got.Keys)
}

func TestAdminKVEntryUTF8(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	val := `{"sha":"abc123"}`
	require.NoError(t, store.Set("h", "marker", []byte(val), time.Hour))

	rr := adminGet(t, s, "/kv/h/marker")
	require.Equal(t, http.StatusOK, rr.Code)
	var got kvEntryJSON
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, "h", got.Namespace)
	require.Equal(t, "marker", got.Key)
	require.Equal(t, len(val), got.Size)
	require.NotNil(t, got.ExpiresAt)
	require.NotNil(t, got.TTLSeconds)
	// A valid-UTF- value arrives in both encodings, byte-identical.
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(val)), got.ValueBase64)
	require.NotNil(t, got.ValueUTF8)
	require.Equal(t, val, *got.ValueUTF8)
}

func TestAdminKVEntryBinary(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	val := []byte{0xff, 0x00, 0x80, 'x'} // not valid UTF-
	require.NoError(t, store.Set("h", "blob", val, 0))

	rr := adminGet(t, s, "/kv/h/blob")
	require.Equal(t, http.StatusOK, rr.Code)
	var got kvEntryJSON
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, base64.StdEncoding.EncodeToString(val), got.ValueBase64)
	require.Nil(t, got.ValueUTF8, "invalid UTF-8 must not be shipped as a JSON string")
	require.Nil(t, got.ExpiresAt)
}

func TestAdminKVEntryEscapedKey(t *testing.T) {
	// pr-minder-style keys contain "/", ":" and "#"; the admin URL carries them percent-encoded in a single path segment.
	s, store := newStateServer(t, kv.Config{})
	require.NoError(t, store.Set("h", "pr:wow-look-at-my/webhooks#42", []byte("sha"), 0))

	rr := adminGet(t, s, "/kv/h/pr%3Awow-look-at-my%2Fwebhooks%2342")
	require.Equal(t, http.StatusOK, rr.Code)
	var got kvEntryJSON
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, "pr:wow-look-at-my/webhooks#42", got.Key)
	require.NotNil(t, got.ValueUTF8)
	require.Equal(t, "sha", *got.ValueUTF8)
}

func TestAdminKVEntryMissingOrExpired(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})

	rr := adminGet(t, s, "/kv/h/nope")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "absent or expired")

	// An expired key s exactly like a missing — the same lazy-expiry rule as the state API, so inspection never returns ghosts.
	require.NoError(t, store.Set("h", "fast", []byte("v"), 10*time.Millisecond))
	time.Sleep(30 * time.Millisecond)
	rr = adminGet(t, s, "/kv/h/fast")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "absent or expired")
}

func TestAdminKVInspectionNilStore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Options{Logger: logger}) // no KV configured
	require.Equal(t, http.StatusServiceUnavailable, adminGet(t, s, "/kv/h").Code)
	require.Equal(t, http.StatusServiceUnavailable, adminGet(t, s, "/kv/h/k").Code)
}

// The bare /kv stats endpoint keeps its original shape (a top-level array of
// value-free per-namespace summaries) — the new inspection routes must not
// change it.
func TestAdminKVStatsShapeUnchanged(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	require.NoError(t, store.Set("h", "k", []byte("secret-value"), 0))
	rr := adminGet(t, s, "/kv")
	require.Equal(t, http.StatusOK, rr.Code)
	var stats []kv.NamespaceStat
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &stats))
	require.Equal(t, []kv.NamespaceStat{{Namespace: "h", Keys: 1, Bytes: 12}}, stats)
	require.NotContains(t, rr.Body.String(), "secret-value")
}

// A state-API write whose disk persist fails must be loud end-to-end: the
// store rolls back and returns the error, the hook gets a xx carrying the
// reason, the server logs it, and a kv.write_failed event lands on the
// activity feed.
func TestStateWriteFailureIsLoud(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kv")
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	store, err := kv.New(kv.Config{Dir: dir}, []byte("secret"), logger)
	require.NoError(t, err)
	rec := events.NewRecorder(16)
	s := New(Options{Logger: logger, KV: store, Events: rec})
	tok := store.Token("h", "run1")

	// Sabotage persistence: remove the store's directory out from under it, so the next write's temp-file creation fails (works even as root.
	require.NoError(t, os.RemoveAll(dir))

	rr := stateReq(t, s, "PUT", "/kv/foo", tok, strings.NewReader("v"))
	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Contains(t, rr.Body.String(), "state store error")

	// Rolled back: the failed write never became readable.
	_, ok := store.Get("h", "foo")
	require.False(t, ok, "failed persist left the value readable in memory")

	// Logged...
	require.Contains(t, logBuf.String(), "state write failed")

	// ...and on the activity feed, hook-scoped.
	evs := rec.ListByHook("h", 10)
	require.Len(t, evs, 1)
	require.Equal(t, "kv.write_failed", evs[0].Kind)
	require.Contains(t, evs[0].Msg, "state write failed")
}
