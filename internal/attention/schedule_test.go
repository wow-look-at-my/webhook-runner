package attention

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func TestCheckStaleSchedules_NoTrackedRuns(t *testing.T) {
	// A brand-new schedule with nothing tracked yet is a startup window,
	// never a standing problem.
	entries := CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(string) []*runs.Run { return nil },
		time.Now(),
	)
	assert.Empty(t, entries)
}

func TestCheckStaleSchedules_RecentSuccessIsQuiet(t *testing.T) {
	tracker := runs.NewTracker()
	r := tracker.New("pr-minder")
	r.SetRunning()
	r.Finish(runs.StatusSuccess, 0, "")

	entries := CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		time.Now(), // no time has passed since the success above
	)
	assert.Empty(t, entries)
}

func TestCheckStaleSchedules_StaleSuccessReportsOnce(t *testing.T) {
	tracker := runs.NewTracker()
	r := tracker.New("pr-minder")
	r.SetRunning()
	r.Finish(runs.StatusSuccess, 0, "")

	// The threshold is max(interval*3, 15m); an hourly schedule's threshold is 3h, so simulate "now" 4h later, well past it.
	future := time.Now().Add(4 * time.Hour)
	entries := CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		future,
	)
	require.Len(t, entries, 1)
	assert.Equal(t, "pr-minder", entries[0].Hook)
	assert.Equal(t, KeyStale, entries[0].Key)
	assert.Contains(t, entries[0].Message, "last successful run")
}

func TestCheckStaleSchedules_NeverSucceededButTooYoungToCallStale(t *testing.T) {
	tracker := runs.NewTracker()
	r := tracker.New("pr-minder")
	r.SetRunning()
	r.Finish(runs.StatusFailure, 1, "boom")

	entries := CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		time.Now(), // the one failure just happened -- no history to judge yet
	)
	assert.Empty(t, entries)
}

func TestCheckStaleSchedules_NeverSucceededAndOldEnoughReportsOnce(t *testing.T) {
	tracker := runs.NewTracker()
	r := tracker.New("pr-minder")
	r.SetRunning()
	r.Finish(runs.StatusFailure, 1, "boom")

	future := time.Now().Add(4 * time.Hour)
	entries := CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		future,
	)
	require.Len(t, entries, 1)
	assert.Equal(t, "pr-minder", entries[0].Hook)
	assert.Equal(t, KeyStale, entries[0].Key)
	assert.Contains(t, entries[0].Message, "no successful run is on record")
}

func TestCheckStaleSchedules_IgnoresUnscheduledAndNonPositiveIntervals(t *testing.T) {
	tracker := runs.NewTracker()
	entries := CheckStaleSchedules(
		map[string]time.Duration{"one-shot-hook": 0, "negative": -time.Minute},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		time.Now().Add(24*time.Hour),
	)
	assert.Empty(t, entries)
}

func TestCheckStaleSchedules_ClearsViaReplaceSource(t *testing.T) {
	// End-to-end through the Aggregator: a stale entry reported one minute and cleared the next (a success landed) must actually disappear --.
	agg := New()
	tracker := runs.NewTracker()
	r := tracker.New("pr-minder")
	r.SetRunning()
	r.Finish(runs.StatusFailure, 1, "boom")

	future := time.Now().Add(4 * time.Hour)
	agg.ReplaceSource(SourceSchedule, CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		future,
	))
	require.Equal(t, 1, agg.Count())

	r2 := tracker.New("pr-minder")
	r2.SetRunning()
	r2.Finish(runs.StatusSuccess, 0, "")
	// The success just landed relative to REAL time, not the fixed `future`
	// reference above -- check staleness as of now, not as of 4h from the
	// original test start.
	agg.ReplaceSource(SourceSchedule, CheckStaleSchedules(
		map[string]time.Duration{"pr-minder": time.Hour},
		func(id string) []*runs.Run { return tracker.ListByHook(id, 50) },
		time.Now(),
	))
	assert.Equal(t, 0, agg.Count())
}
