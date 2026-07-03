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
