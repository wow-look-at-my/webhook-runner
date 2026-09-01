// GET /runs time-cursor paging (?before=) and its companion /config field
// (run_retention — the "history ends here" boundary for paging clients).
// Beside these, TestListRuns/TestListRunsByHook (server_test.go) pin the
// omitted-cursor behavior and TestListRunsBeforePagesMergedSources
// (runstore_merge_test.go) covers paging across the live+persisted merge.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// ?before= pages the run list: a valid RFC(Nano) cursor keeps only
// strictly-older runs (the live tracker is filtered too), garbage is a
// in the standard JSON error shape, and an omitted or empty cursor keeps
// today's unpaged view byte-for-byte.
func TestListRunsBeforeParam(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	r1 := tr.New("a")
	r1.Finish(runs.StatusSuccess, 0, "")
	r2 := tr.New("b")

	body := func(target string, wantCode int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, wantCode, rec.Code, "GET %s", target)
		return rec.Body.String()
	}

	// A cursor at the newer run's queued instant excludes it — strictly before — leaving only the older run.
	var got []runs.RunState
	nanoCursor := url.QueryEscape(r2.Snapshot(0).Started.Format(time.RFC3339Nano))
	require.NoError(t, json.Unmarshal([]byte(body("/runs?before="+nanoCursor, http.StatusOK)), &got))
	require.Len(t, got, 1)
	assert.Equal(t, r1.ID(), got[0].ID)

	// Plain RFC (no fractional seconds) parses too.
	plain := url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	require.NoError(t, json.Unmarshal([]byte(body("/runs?before="+plain, http.StatusOK)), &got))
	assert.Len(t, got, 2)

	// Garbage is a with the handlers' JSON error shape.
	var e map[string]string
	require.NoError(t, json.Unmarshal([]byte(body("/runs?before=yesterday", http.StatusBadRequest)), &e))
	assert.Contains(t, e["error"], `invalid before="yesterday"`)

	// Omitted and empty cursors keep the unpaged view, byte-for-byte — which a far-future cursor (excluding nothing) also matches.
	unpaged := body("/runs", http.StatusOK)
	assert.Equal(t, unpaged, body("/runs?before=", http.StatusOK))
	future := url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano))
	assert.Equal(t, unpaged, body("/runs?before="+future, http.StatusOK))
}

// With a run store configured, /config additionally reports the persisted
// history window as a compact duration — how far back /runs?before= paging
// can ever reach. (TestConfigEndpointEmpty proves it is absent without a
// store.)
func TestConfigRunRetention(t *testing.T) {
	s, _, _, _ := newTestServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var cfg map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&cfg))
	assert.Equal(t, "48h", cfg["run_retention"], "default retention, compacted")
}
