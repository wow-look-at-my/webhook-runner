package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// ?exclude= filters BEFORE the cap. The bug it fixes: the dashboard fetched
// the newest runs and hid statuses client-side, so a hook whose recent
// history is nothing but skips (every gha-runner delivery that is not a
// queued job) rendered an empty table reading "All recent run(s) are
// hidden by the status filter above" — while the runs the operator actually
// wanted sat just past the window, unreachable at any max.
func TestListRunsExcludeFiltersBeforeTheCap(t *testing.T) {
	s, _, _, st := newTestServerWithStore(t)

	// The shape that broke: consecutive skips on top of the real runs.
	// Anything that filters after limiting can only ever answer "nothing".
	want := []string{
		"successsuccesssuccesssucce",
		"failurefailurefailurefailu",
	}
	persistOld(t, st, want[1], "h", runs.StatusFailure, 3*time.Hour)
	persistOld(t, st, want[0], "h", runs.StatusSuccess, 2*time.Hour)
	for i := range 60 {
		persistOld(t, st, fmt.Sprintf("skip%022d", i), "h", runs.StatusSkipped,
			time.Hour-time.Duration(i)*time.Minute)
	}

	get := func(query string) []runs.RunState {
		t.Helper()
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, query, nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var got []runs.RunState
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}

	// The pre-fix behavior, still the default: the newest are all skips.
	plain := get("/runs?hook=h&max=50")
	require.Len(t, plain, 50)
	for _, r := range plain {
		require.Equal(t, runs.StatusSkipped, r.Status)
	}

	// Filtered: the limit now counts only what survives the filter, so the runs behind the wall of skips are reachable at the SAME max.
	got := get("/runs?hook=h&max=50&exclude=skipped")
	var ids []string
	for _, r := range got {
		assert.NotEqual(t, runs.StatusSkipped, r.Status, "an excluded status must never appear")
		ids = append(ids, r.ID)
	}
	assert.Equal(t, want, ids, "both non-skipped runs, newest-first, despite 60 newer skips")

	// Multiple statuses, and the cap still applies to what remains.
	assert.Empty(t, get("/runs?hook=h&max=50&exclude=skipped,success,failure"))
	assert.Len(t, get("/runs?hook=h&max=1&exclude=skipped"), 1)

	// The unfiltered all-hooks view is unchanged.
	assert.Len(t, get("/runs?max=50"), 50)
}

// The live tracker partition is filtered too — including the rule that a
// non-terminal run normally rides above the cap. An explicit operator
// filter is a request, not a window size.
func TestListRunsExcludeCoversLiveRuns(t *testing.T) {
	s, _, tr, _ := newTestServerWithStore(t)

	// Ordering is by Started, so the finished run is created to make the active unambiguously newest.
	fin := tr.New("h")
	fin.Finish(runs.StatusSuccess, 0, "")
	act := tr.New("h")
	act.SetRunning()

	get := func(query string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, query, nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var got []runs.RunState
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		ids := make([]string, 0, len(got))
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		return ids
	}

	assert.Equal(t, []string{act.ID(), fin.ID()}, get("/runs?hook=h"))
	assert.Equal(t, []string{fin.ID()}, get("/runs?hook=h&exclude=running"),
		"hiding running hides the active run: the always-include-active rule guards the CAP, not an explicit filter")
	assert.Equal(t, []string{act.ID()}, get("/runs?hook=h&exclude=success"))

	// ?live= honors it as well, so a client cannot get an unfiltered answer by asking for the active set.
	assert.Equal(t, []string{act.ID()}, get("/runs?hook=h&live=1"))
	assert.Empty(t, get("/runs?hook=h&live=1&exclude=running"))
}

// An unrecognized status is a . Silently matching nothing would look
// exactly like "this hook has no runs" — the failure mode the parameter
// exists to remove.
func TestListRunsExcludeRejectsUnknownStatus(t *testing.T) {
	s, _, _, _ := newTestServerWithStore(t)

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs?exclude=skipped,bogus", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "bogus")

	// Empty and whitespace-only are "no filter", not an error.
	for _, q := range []string{"/runs?exclude=", "/runs?exclude=%20", "/runs?exclude=,"} {
		rec = httptest.NewRecorder()
		admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, q, nil))
		assert.Equal(t, http.StatusOK, rec.Code, q)
	}
}
