package runstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func filteredIDs(list []runs.RunState) []string {
	out := make([]string, 0, len(list))
	for _, st := range list {
		out = append(out, st.ID)
	}
	return out
}

// FILTER FIRST, THEN LIMIT: max must count only the runs the predicate
// keeps. Listing max rows and filtering afterwards is the bug — a hook whose
// newest runs are all skips answers "nothing" at every max.
func TestListFilteredCountsOnlyKeptRuns(t *testing.T) {
	s := newStore(t, Config{})
	base := time.Now().UTC().Add(-time.Hour)

	// 20 skips on top of 5 successes, interleaved so the newest 20 rows in both indexes are skips.
	var wantSuccess []string
	for i := range 5 {
		id := fmt.Sprintf("succ%022d", i)
		require.NoError(t, s.Record(state(id, "h", runs.StatusSuccess, base.Add(time.Duration(i)*time.Minute))))
		wantSuccess = append([]string{id}, wantSuccess...) // newest-first
	}
	for i := range 20 {
		id := fmt.Sprintf("skip%022d", i)
		require.NoError(t, s.Record(state(id, "h", runs.StatusSkipped, base.Add(time.Duration(10+i)*time.Minute))))
	}

	notSkipped := func(st runs.Status) bool { return st != runs.StatusSkipped }

	// Unfiltered, the newest 5 are all skips — the pre-fix answer.
	for _, st := range s.ListByHook("h", 5) {
		require.Equal(t, runs.StatusSkipped, st.Status)
	}

	// Filtered at the SAME max, every success is reachable.
	assert.Equal(t, wantSuccess, filteredIDs(s.ListByHookBeforeFiltered("h", time.Time{}, 5, notSkipped)))
	assert.Equal(t, wantSuccess, filteredIDs(s.ListAllBeforeFiltered(time.Time{}, 5, notSkipped)))

	// The cap still caps what survives.
	assert.Equal(t, wantSuccess[:2], filteredIDs(s.ListByHookBeforeFiltered("h", time.Time{}, 2, notSkipped)))
	assert.Equal(t, wantSuccess[:2], filteredIDs(s.ListAllBeforeFiltered(time.Time{}, 2, notSkipped)))

	// A predicate matching nothing answers empty, not a partial page.
	none := func(runs.Status) bool { return false }
	assert.Empty(t, s.ListByHookBeforeFiltered("h", time.Time{}, 5, none))
	assert.Empty(t, s.ListAllBeforeFiltered(time.Time{}, 5, none))

	// nil keep is exactly the unfiltered contract.
	assert.Equal(t,
		filteredIDs(s.ListByHook("h", 0)),
		filteredIDs(s.ListByHookBeforeFiltered("h", time.Time{}, 0, nil)))
	assert.Equal(t,
		filteredIDs(s.ListAll(0)),
		filteredIDs(s.ListAllBeforeFiltered(time.Time{}, 0, nil)))
}

// The filter composes with the ?before= page cursor: paging stays complete
// down to each page's oldest kept row.
func TestListFilteredComposesWithBefore(t *testing.T) {
	s := newStore(t, Config{})
	base := time.Now().UTC().Add(-time.Hour)

	// success, skip, success, skip, ... newest last.
	for i := range 10 {
		status := runs.StatusSuccess
		if i%2 == 1 {
			status = runs.StatusSkipped
		}
		require.NoError(t, s.Record(state(fmt.Sprintf("run%023d", i), "h", status, base.Add(time.Duration(i)*time.Minute))))
	}
	notSkipped := func(st runs.Status) bool { return st != runs.StatusSkipped }

	page1 := s.ListByHookBeforeFiltered("h", time.Time{}, 3, notSkipped)
	require.Len(t, page1, 3)
	page2 := s.ListByHookBeforeFiltered("h", page1[len(page1)-1].Started, 3, notSkipped)
	require.Len(t, page2, 2, "5 successes total across the two pages")
	for _, st := range append(append([]runs.RunState{}, page1...), page2...) {
		assert.NotEqual(t, runs.StatusSkipped, st.Status)
	}
	assert.Empty(t, s.ListByHookBeforeFiltered("h", page2[len(page2)-1].Started, 3, notSkipped))
}
