package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// --- Queue drill-down: holders / waiting_runs on /concurrency --------------

func TestConcurrencyEndpointCarriesHoldersAndWaiting(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"model-gateway": {Limit: 1}},
	})
	tr := runs.NewTracker()
	s := New(Options{
		Registry:    hooks.NewRegistry(),
		Tracker:     tr,
		Logger:      logger,
		Concurrency: mgr,
	})

	// A real tracked run holds the slot; a second real run queues behind it.
	holder := tr.New("pr-resolve")
	holder.SetTitle("wow-look-at-my/scratch#117")
	holder.SetRunning()
	relA, ok, err := mgr.Acquire("model-gateway", holder.ID(), nil, nil)
	require.NoError(t, err)
	require.True(t, ok)
	defer relA()

	waiter := tr.New("pr-resolve")
	queued := make(chan struct{})
	got := make(chan bool, 1)
	go func() {
		rel, ok, _ := mgr.Acquire("model-gateway", waiter.ID(), waiter.Cancelled(), func(concurrency.QueueState) {
			select {
			case <-queued:
			default:
				close(queued)
			}
		})
		got <- ok
		if ok {
			rel()
		}
	}()
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never queued")
	}

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
	require.Equal(t, 200, rec.Code)

	var groups []struct {
		Name    string `json:"name"`
		Active  int    `json:"active"`
		Waiting int    `json:"waiting"`
		Holders []struct {
			RunID  string `json:"run_id"`
			HookID string `json:"hook_id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"holders"`
		WaitingRuns []struct {
			RunID  string `json:"run_id"`
			HookID string `json:"hook_id"`
		} `json:"waiting_runs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 1)
	g := groups[0]
	assert.Equal(t, "model-gateway", g.Name)
	require.Len(t, g.Holders, 1)
	assert.Equal(t, holder.ID(), g.Holders[0].RunID)
	assert.Equal(t, "pr-resolve", g.Holders[0].HookID)
	assert.Equal(t, "wow-look-at-my/scratch#117", g.Holders[0].Title)
	assert.Equal(t, "running", g.Holders[0].Status)
	require.Len(t, g.WaitingRuns, 1)
	assert.Equal(t, waiter.ID(), g.WaitingRuns[0].RunID)
	assert.Equal(t, "pr-resolve", g.WaitingRuns[0].HookID)

	// Unblock the waiter so the test goroutine exits cleanly.
	relA()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never finished")
	}
}

// A holder evicted from the tracker window still shows up by run id — the
// operator must see THAT something holds the slot even when the run's
// metadata is gone.
func TestConcurrencyViewUnknownRunKeepsID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 1}},
	})
	s := New(Options{Registry: hooks.NewRegistry(), Tracker: runs.NewTracker(), Logger: logger, Concurrency: mgr})
	rel, ok, _ := mgr.Acquire("g", "gone-run-id", nil, nil)
	require.True(t, ok)
	defer rel()

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
	body := rec.Body.String()
	assert.Contains(t, body, "gone-run-id")
	assert.NotContains(t, body, `"hook_id"`) // no metadata invented
}
