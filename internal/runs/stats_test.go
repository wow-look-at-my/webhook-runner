package runs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatsByHookEmpty(t *testing.T) {
	tr := NewTracker()
	got := tr.StatsByHook("nothing-yet")
	assert.Equal(t, 0, got.Tracked)
	assert.Equal(t, MaxRunsPerHook, got.MaxTracked)
	assert.Equal(t, 0, got.Completed)
	assert.Zero(t, got.SuccessRate)
	assert.Nil(t, got.ByStatus)
	assert.Nil(t, got.LastRun)
}

// backdate rewrites a run's start/finish times so duration aggregation can be
// asserted deterministically (New/Finish stamp time.Now).
func backdate(r *Run, started time.Time, dur time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Started = started
	if !r.state.Finished.IsZero() {
		r.state.Finished = started.Add(dur)
	}
}

// Skipped runs land in their own bucket: counted (Tracked, ByStatus,
// Skipped) but excluded from Completed and every rate/duration/wait figure
// — a skip did no work, so it must not read as a fast success or a failure.
func TestStatsSkippedBucket(t *testing.T) {
	tr := NewTracker()

	ok := tr.New("h")
	ok.SetRunning()
	ok.Finish(StatusSuccess, 0, "")

	bad := tr.New("h")
	bad.SetRunning()
	bad.Finish(StatusFailure, 1, "")

	for range 3 {
		sk := tr.New("h")
		sk.AppendOutput(`skipped: skip_if[0]: header x-github-event == "workflow_run"`)
		sk.Finish(StatusSkipped, 0, "")
	}

	got := tr.StatsByHook("h")
	assert.Equal(t, 5, got.Tracked)
	assert.Equal(t, 2, got.Completed, "skips are not completions")
	assert.Equal(t, 3, got.Skipped)
	assert.Equal(t, 0.5, got.SuccessRate, "the rate covers real work only")
	assert.Equal(t, 3, got.ByStatus[StatusSkipped])
	assert.Equal(t, 2, got.WaitSampled, "skips contribute no wait samples")
	require.NotNil(t, got.LastRun)
	assert.Equal(t, StatusSkipped, got.LastRun.Status, "a skip is still the latest run")
}

func TestStatsByHookAggregates(t *testing.T) {
	tr := NewTracker()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	ok1 := tr.New("h")
	ok1.Finish(StatusSuccess, 0, "")
	backdate(ok1, base, 2*time.Second)

	ok2 := tr.New("h")
	ok2.Finish(StatusSuccess, 0, "")
	backdate(ok2, base.Add(time.Minute), 4*time.Second)

	bad := tr.New("h")
	bad.Finish(StatusFailure, 1, "")
	backdate(bad, base.Add(2*time.Minute), 6*time.Second)

	// Active: counted as tracked but excluded from completed/rate/durations.
	active := tr.New("h")
	active.SetRunning()
	backdate(active, base.Add(3*time.Minute), 0)

	// A different hook's runs must not leak in.
	other := tr.New("other")
	other.Finish(StatusSuccess, 0, "")

	got := tr.StatsByHook("h")
	assert.Equal(t, 4, got.Tracked)
	assert.Equal(t, MaxRunsPerHook, got.MaxTracked)
	assert.Equal(t, map[Status]int{StatusSuccess: 2, StatusFailure: 1, StatusRunning: 1}, got.ByStatus)
	assert.Equal(t, 3, got.Completed)
	assert.InDelta(t, 2.0/3.0, got.SuccessRate, 1e-9)
	assert.Equal(t, int64(4000), got.AvgDurationMS) // (2s+4s+6s)/3
	assert.Equal(t, int64(6000), got.MaxDurationMS)

	// Last run is the newest by start time — here the still-running one.
	require.NotNil(t, got.LastRun)
	assert.Equal(t, active.ID(), got.LastRun.ID)
	assert.Equal(t, StatusRunning, got.LastRun.Status)
	assert.True(t, got.LastRun.Finished.IsZero())
}

// The queue-wait/processing split: duration covers only StartedAt→Finished,
// wait covers Started→StartedAt, legacy rows (no StartedAt) fall back to the
// old queued-inclusive duration and are excluded from wait, and never-started
// runs contribute to neither.
func TestComputeStatsSplitsWaitFromProcessing(t *testing.T) {
	base := time.Date(2026, 7, 9, 3, 36, 11, 0, time.UTC)
	states := []RunState{
		// Accepted, queued 26m09s behind a busy concurrency group, then
		// processed for 23s. Duration must read 23s, not the 26m32s of the
		// queued-inclusive span.
		{ID: "incident", HookID: "h", Status: StatusSuccess,
			Started:   base,
			StartedAt: base.Add(26*time.Minute + 9*time.Second),
			Finished:  base.Add(26*time.Minute + 32*time.Second)},
		// Barely queued: launched after 1s, ran 3s.
		{ID: "quick", HookID: "h", Status: StatusFailure,
			Started:   base.Add(time.Minute),
			StartedAt: base.Add(time.Minute + time.Second),
			Finished:  base.Add(time.Minute + 4*time.Second)},
		// Legacy pre-upgrade row: terminal success with no StartedAt —
		// duration falls back to the queued-inclusive span (10s), and the
		// row is excluded from the wait figures.
		{ID: "legacy", HookID: "h", Status: StatusSuccess,
			Started:  base.Add(2 * time.Minute),
			Finished: base.Add(2*time.Minute + 10*time.Second)},
		// Never started: cancelled while queued. Its 20m in the queue is
		// not processing time — it must not pollute the duration figures.
		{ID: "neverstarted", HookID: "h", Status: StatusCancelled,
			Started:  base.Add(3 * time.Minute),
			Finished: base.Add(23 * time.Minute)},
	}
	got := ComputeStats(states)
	assert.Equal(t, 4, got.Completed)
	// Durations: 23s + 3s + 10s (legacy fallback) over 3 samples.
	assert.Equal(t, int64(12000), got.AvgDurationMS)
	assert.Equal(t, int64(23000), got.MaxDurationMS)
	// Waits: 26m09s + 1s over the 2 runs that recorded a StartedAt.
	assert.Equal(t, 2, got.WaitSampled)
	assert.Equal(t, int64(785000), got.AvgWaitMS) // (1569s+1s)/2
	assert.Equal(t, int64(1569000), got.MaxWaitMS)
}

func TestStatsByHookAllActive(t *testing.T) {
	tr := NewTracker()
	tr.New("h")
	tr.New("h").SetRunning()

	got := tr.StatsByHook("h")
	assert.Equal(t, 2, got.Tracked)
	assert.Equal(t, 0, got.Completed)
	assert.Zero(t, got.SuccessRate)
	assert.Zero(t, got.AvgDurationMS)
	assert.Zero(t, got.MaxDurationMS)
	require.NotNil(t, got.LastRun)
}

func TestStatsByHookRespectsEviction(t *testing.T) {
	tr := NewTracker()
	tr.maxByHook = 2
	tr.New("h").Finish(StatusFailure, 1, "")
	tr.New("h").Finish(StatusSuccess, 0, "")
	tr.New("h").Finish(StatusSuccess, 0, "")

	// The failure fell out of the bounded window, so the rate reflects only
	// what is retained.
	got := tr.StatsByHook("h")
	assert.Equal(t, 2, got.Tracked)
	assert.Equal(t, 2, got.MaxTracked)
	assert.Equal(t, 2, got.Completed)
	assert.Equal(t, 1.0, got.SuccessRate)
}
