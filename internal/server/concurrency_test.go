package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
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

	var doc struct {
		Groups []struct {
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
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	groups := doc.Groups
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

// --- The GLOBAL run cap on /concurrency + its override endpoints -----------

func TestConcurrencyEndpointCarriesGlobalCap(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := concurrency.NewGlobal(64)
	s := New(Options{
		Registry:  hooks.NewRegistry(),
		Tracker:   runs.NewTracker(),
		Logger:    logger,
		GlobalCap: g,
	})

	rel, acquired := g.Acquire("held-run", nil, nil)
	require.True(t, acquired)
	defer rel()

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
	require.Equal(t, 200, rec.Code)

	var doc struct {
		Global *struct {
			Limit      int  `json:"limit"`
			Default    int  `json:"default"`
			Overridden bool `json:"overridden"`
			Active     int  `json:"active"`
			Waiting    int  `json:"waiting"`
			Holders    []struct {
				RunID string `json:"run_id"`
			} `json:"holders"`
		} `json:"global"`
		Groups []any `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	require.NotNil(t, doc.Global, "the global cap must be reported")
	assert.Equal(t, 64, doc.Global.Limit)
	assert.Equal(t, 64, doc.Global.Default)
	assert.False(t, doc.Global.Overridden)
	assert.Equal(t, 1, doc.Global.Active)
	assert.Zero(t, doc.Global.Waiting)
	require.Len(t, doc.Global.Holders, 1)
	assert.Equal(t, "held-run", doc.Global.Holders[0].RunID)
	assert.NotNil(t, doc.Groups, "groups stays present alongside the cap")
}

// Without a configured cap (older wiring, bare test servers) the view
// simply omits it — never a synthesized zero-limit entry.
func TestConcurrencyEndpointOmitsGlobalWhenUnconfigured(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/concurrency", nil))
	require.Equal(t, 200, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"global"`)
}

func newGlobalCapServer(t *testing.T) (*Server, *concurrency.Global, *overrides.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := concurrency.NewGlobal(64)
	ov, err := overrides.Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	s := New(Options{
		Registry:  hooks.NewRegistry(),
		Tracker:   runs.NewTracker(),
		Logger:    logger,
		GlobalCap: g,
		Overrides: ov,
	})
	return s, g, ov
}

// PUT persists + applies the cap override; DELETE reverts to the default.
// The endpoints mirror the group pair: validation (< 1 rejected, malformed
// body rejected), persist-then-apply, loud events.
func TestGlobalCapOverrideEndpoints(t *testing.T) {
	s, g, ov := newGlobalCapServer(t)

	parseResp := func(rec *httptest.ResponseRecorder) (limit, def int, overridden bool) {
		var body struct {
			Limit      int  `json:"limit"`
			Default    int  `json:"default"`
			Overridden bool `json:"overridden"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Limit, body.Default, body.Overridden
	}

	// Set: applied live AND persisted.
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/concurrency-global/limit",
		strings.NewReader(`{"limit": 100}`)))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	limit, def, overridden := parseResp(rec)
	assert.Equal(t, 100, limit)
	assert.Equal(t, 64, def)
	assert.True(t, overridden)
	st := g.Status()
	assert.Equal(t, 100, st.Limit)
	assert.True(t, st.Overridden)
	n, ok := ov.GlobalRunLimit()
	require.True(t, ok, "the override must persist")
	assert.Equal(t, 100, n)

	// Clear: back to the default, override removed from the store.
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/concurrency-global/limit", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	limit, def, overridden = parseResp(rec)
	assert.Equal(t, 64, limit)
	assert.Equal(t, 64, def)
	assert.False(t, overridden)
	st = g.Status()
	assert.Equal(t, 64, st.Limit)
	assert.False(t, st.Overridden)
	_, ok = ov.GlobalRunLimit()
	assert.False(t, ok)
}

func TestGlobalCapOverrideValidation(t *testing.T) {
	s, g, _ := newGlobalCapServer(t)

	for name, body := range map[string]string{
		"zero":      `{"limit": 0}`,
		"negative":  `{"limit": -3}`,
		"missing":   `{}`,
		"malformed": `{"limit": "ten"}`,
		"unknown":   `{"limit": 2, "bogus": true}`,
	} {
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/concurrency-global/limit",
			strings.NewReader(body)))
		assert.Equal(t, 400, rec.Code, "case %s: %s", name, rec.Body.String())
	}
	st := g.Status()
	assert.Equal(t, 64, st.Limit, "rejected requests must not change the cap")
	assert.False(t, st.Overridden)
}

// Without a configured cap the override endpoints refuse loudly instead of
// pretending to apply something.
func TestGlobalCapOverrideUnconfigured(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/concurrency-global/limit",
		strings.NewReader(`{"limit": 5}`)))
	assert.Equal(t, 500, rec.Code)
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/concurrency-global/limit", nil))
	assert.Equal(t, 500, rec.Code)
}
