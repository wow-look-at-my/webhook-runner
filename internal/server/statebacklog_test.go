package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/backlog"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
)

func newBacklogServer(t *testing.T) (*Server, *kv.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	store, err := kv.New(kv.Config{Dir: filepath.Join(dir, "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	qs, err := backlog.New(backlog.Config{Dir: filepath.Join(dir, "queues")}, logger)
	require.NoError(t, err)
	return New(Options{Logger: logger, KV: store, Backlogs: qs}), store
}

// The whole point of the primitive, over HTTP: a run pushes the work it knows
// about, a later run takes what it can, and nothing in between had to invent a
// cursor.
func TestStateBacklogPushTakeAcrossRuns(t *testing.T) {
	s, store := newBacklogServer(t)
	runA := store.Token("h", "run-a")
	runB := store.Token("h", "run-b")

	rr := stateReq(t, s, "POST", "/backlog/backlog/push", runA, strings.NewReader(`{"items":["o/r#1","o/r#2","o/r#3"]}`))
	require.Equal(t, http.StatusOK, rr.Code)
	var push struct{ Queued, Duplicates, Dropped, Depth int }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &push))
	assert.Equal(t, 3, push.Queued)
	assert.Equal(t, 3, push.Depth)

	// A DIFFERENT run drains it — the backlog belongs to the hook, not to the
	// run that filled it.
	rr = stateReq(t, s, "POST", "/backlog/backlog/take", runB, strings.NewReader(`{"count":2}`))
	require.Equal(t, http.StatusOK, rr.Code)
	var take struct {
		Items []string
		Depth int
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &take))
	assert.Equal(t, []string{"o/r#1", "o/r#2"}, take.Items)
	assert.Equal(t, 1, take.Depth)

	// Re-pushing the whole set adds only what is missing; the taken ones come
	// back (the caller still lists them — that is the stateless usage).
	rr = stateReq(t, s, "POST", "/backlog/backlog/push", runB, strings.NewReader(`{"items":["o/r#1","o/r#2","o/r#3"]}`))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &push))
	assert.Equal(t, 2, push.Queued)
	assert.Equal(t, 1, push.Duplicates, "the un-taken item keeps its place")
	assert.Equal(t, 3, push.Depth)

	// Depth is readable on its own, contents are not exposed by it.
	rr = stateReq(t, s, "GET", "/backlog/backlog", runA, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var stat backlog.Stat
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &stat))
	assert.Equal(t, backlog.Stat{Name: "backlog", Depth: 3}, stat)
	assert.NotContains(t, rr.Body.String(), "o/r#", "a depth read never exposes the contents")

	rr = stateReq(t, s, "GET", "/backlogs", runA, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var stats []backlog.Stat
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &stats))
	assert.Equal(t, []backlog.Stat{{Name: "backlog", Depth: 3}}, stats)
}

// Namespaces come from the token, never the URL — a hook cannot reach another
// hook's backlog.
func TestStateBacklogNamespaceIsolation(t *testing.T) {
	s, store := newBacklogServer(t)

	require.Equal(t, http.StatusOK,
		stateReq(t, s, "POST", "/backlog/q/push", store.Token("h1", "run-a"), strings.NewReader(`{"items":["x"]}`)).Code)

	rr := stateReq(t, s, "POST", "/backlog/q/take", store.Token("h2", "run-b"), strings.NewReader(`{"count":5}`))
	require.Equal(t, http.StatusOK, rr.Code)
	var take struct{ Items []string }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &take))
	assert.Empty(t, take.Items, "h2 sees its own empty queue, not h1's work")

	require.Equal(t, http.StatusUnauthorized, stateReq(t, s, "POST", "/backlog/q/take", "", nil).Code)
}

func TestStateBacklogValidation(t *testing.T) {
	s, store := newBacklogServer(t)
	tok := store.Token("h", "run-a")

	for _, body := range []string{`{"items":[]}`, `{}`, `not json`, `{"items":[""]}`} {
		rr := stateReq(t, s, "POST", "/backlog/q/push", tok, strings.NewReader(body))
		assert.Equalf(t, http.StatusBadRequest, rr.Code, "push body=%q", body)
	}
	for _, body := range []string{`{"count":0}`, `{"count":-1}`, `{"count":1001}`, `nope`} {
		rr := stateReq(t, s, "POST", "/backlog/q/take", tok, strings.NewReader(body))
		assert.Equalf(t, http.StatusBadRequest, rr.Code, "take body=%q", body)
	}
	// An empty take body is the documented default of one item.
	require.Equal(t, http.StatusOK, stateReq(t, s, "POST", "/backlog/q/push", tok, strings.NewReader(`{"items":["a","b"]}`)).Code)
	rr := stateReq(t, s, "POST", "/backlog/q/take", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var take struct{ Items []string }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &take))
	assert.Equal(t, []string{"a"}, take.Items)

	// A name outside the alphabet is refused by the read too, not only by the
	// verbs that would create it.
	assert.Equal(t, http.StatusBadRequest, stateReq(t, s, "GET", "/backlog/Bad_Name", tok, nil).Code)
}

// Without a queue store the routes are honestly unavailable rather than
// silently pretending an empty backlog.
func TestStateBacklogUnconfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	s := New(Options{Logger: logger, KV: store})

	tok := store.Token("h", "run-a")
	assert.Equal(t, http.StatusServiceUnavailable, stateReq(t, s, "POST", "/backlog/q/push", tok, strings.NewReader(`{"items":["a"]}`)).Code)
	assert.Equal(t, http.StatusServiceUnavailable, stateReq(t, s, "POST", "/backlog/q/take", tok, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, stateReq(t, s, "GET", "/backlog/q", tok, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, stateReq(t, s, "GET", "/backlogs", tok, nil).Code)
}
