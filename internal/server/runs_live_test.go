// /runs' active-truth guarantees: the merged list ALWAYS carries every
// active (non-terminal) run no matter how small the caller's max is — the
// cap bounds terminal/history rows only — and ?live=1 answers exactly the
// current active set. Together with the stream's hb active-id payload
// (stream_test.go) these are what make the dashboard timeline positively
// recovering: any client can re-derive "what is happening right now" from
// one bounded read, and absence from these reads is an authoritative
// "not running" verdict.
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

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func getRuns(t *testing.T, s *Server, target string) []runs.RunState {
	t.Helper()
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	require.Equal(t, http.StatusOK, rec.Code, "GET %s", target)
	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got, "GET %s must answer a JSON array, never null", target)
	return got
}

// Any requested max must be harmless for live runs: with more active runs
// than max, EVERY active run still comes back and only terminal rows are
// capped. The flood here is SKIPPED records — the production shape: bursts
// of zero-duration terminal instants whose sheer volume used to push
// live-but-waiting runs out of every newest-max window (clients read
// absence as termination).
func TestListRunsActiveSurvivesSkippedFlood(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	activeIDs := set.New[string]()
	for i := 0; i < 5; i++ {
		activeIDs.Add(tr.New("flood").ID())
	}
	for i := 0; i < 6; i++ {
		tr.New("flood").Finish(runs.StatusSkipped, 0, "")
	}

	// max=3 is smaller than either partition alone (5 pending + 6 skipped): ALL 5 pending runs are still present; exactly 3 skipped rows survive.
	got := getRuns(t, s, "/runs?max=3")
	var gotActive, gotTerminal int
	for _, st := range got {
		if st.Status.Terminal() {
			gotTerminal++
			assert.Equal(t, runs.StatusSkipped, st.Status)
		} else {
			gotActive++
			assert.True(t, activeIDs.Contains(st.ID), "unexpected active row %s", st.ID)
		}
	}
	assert.Equal(t, 5, gotActive, "every active run must be returned no matter how small max is")
	assert.Equal(t, 3, gotTerminal, "the max cap applies to terminal rows only")

	// Newest-first ordering still holds over the combined result.
	for i := 1; i < len(got); i++ {
		assert.False(t, got[i-1].Started.Before(got[i].Started), "rows not newest-first at %d", i)
	}

	// The ?hook= view carries the same guarantee.
	byHook := getRuns(t, s, "/runs?hook=flood&max=2")
	var hookActive int
	for _, st := range byHook {
		if !st.Status.Terminal() {
			hookActive++
		}
	}
	assert.Equal(t, 5, hookActive, "?hook= windows must include every active run too")
}

// History pages (?before=) deliberately KEEP the legacy newest-max total
// cap: the paging walk advances its cursor from each page's oldest row, so
// a page must be complete down to that row — an uncapped ancient active
// would make the walk skip terminal history. The active-truth guarantee
// belongs to the cursorless live windows above.
func TestListRunsCursorPagesKeepTotalCap(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	old := tr.New("h") // ancient active run, oldest of all
	for i := 0; i < 4; i++ {
		tr.New("h").Finish(runs.StatusSuccess, 0, "")
	}
	newest := tr.New("h")
	newest.Finish(runs.StatusFailure, 1, "")

	cursor := url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano))
	page := getRuns(t, s, "/runs?before="+cursor+"&max=3")
	require.Len(t, page, 3, "cursor pages stay capped at max TOTAL rows")
	assert.Equal(t, newest.ID(), page[0].ID, "pages are newest-first")
	for _, st := range page {
		assert.NotEqual(t, old.ID(), st.ID, "the ancient active run is outside the newest-max page")
	}
}

// ?live=1 is the one-shot truth fetch: exactly the current non-terminal
// set, [] (never null) when idle, ?hook= narrowing, cap-free by design.
func TestListRunsLiveParam(t *testing.T) {
	s, _, tr, _ := newTestServer(t)

	assert.Empty(t, getRuns(t, s, "/runs?live=1"), "an idle server answers the empty-but-real []")

	a := tr.New("h")
	b := tr.New("h")
	b.SetRunning()
	c := tr.New("other")
	done := tr.New("h")
	done.Finish(runs.StatusFailure, 1, "boom")

	ids := func(states []runs.RunState) set.Set[string] {
		out := set.New[string]()
		for _, st := range states {
			assert.False(t, st.Status.Terminal(), "live view leaked terminal run %s", st.ID)
			assert.Empty(t, st.Output, "list-shaped rows never carry output")
			out.Add(st.ID)
		}
		return out
	}

	got := ids(getRuns(t, s, "/runs?live=1"))
	assert.Equal(t, set.Of(a.ID(), b.ID(), c.ID()), got)

	// ?hook= narrows; max/before don't apply (the active set IS the answer).
	got = ids(getRuns(t, s, "/runs?live=1&hook=h&max=1"))
	assert.Equal(t, set.Of(a.ID(), b.ID()), got)

	// live=0 (and garbage) keep the ordinary merged view.
	all := getRuns(t, s, "/runs?live=0")
	assert.Len(t, all, 4, "live=0 must keep the plain merged view, terminal rows included")
}
