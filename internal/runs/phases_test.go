package runs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkStampsOnceAndIgnoresUnknownPhases(t *testing.T) {
	tr := NewTracker()
	run := tr.New("h")

	run.Mark(PhaseSpawned)
	first := run.Phase(PhaseSpawned)
	require.False(t, first.IsZero(), "spawned should be stamped")

	// First write wins: a phase is a point the run passed through once, and
	// both output streams race to stamp first_output.
	time.Sleep(2 * time.Millisecond)
	run.Mark(PhaseSpawned)
	assert.Equal(t, first, run.Phase(PhaseSpawned), "a second mark must not move the stamp")

	run.Mark(Phase("not-a-phase"))
	assert.Empty(t, run.Snapshot(0).Phases[Phase("not-a-phase")], "unknown phases are dropped, never stored")
}

func TestMarkIgnoresFinishedRuns(t *testing.T) {
	tr := NewTracker()
	run := tr.New("h")
	run.Finish(StatusSuccess, 0, "")

	run.Mark(PhaseExited)
	assert.NotContains(t, run.Snapshot(0).Phases, PhaseExited,
		"history is immutable once the terminal snapshot has flowed through OnFinish")
}

func TestFirstOutputMarkedFromTheFirstLineOnly(t *testing.T) {
	tr := NewTracker()
	run := tr.New("h")

	run.AppendOutput("first")
	stamp := run.Phase(PhaseFirstOutput)
	require.False(t, stamp.IsZero(), "the first line stamps first_output")

	snap := run.Snapshot(-1)
	require.Len(t, snap.OutputTimes, 1)
	assert.Equal(t, snap.OutputTimes[0], stamp,
		"the mark and the line's timestamp come from one instant under one lock")

	time.Sleep(2 * time.Millisecond)
	run.AppendOutput("second")
	assert.Equal(t, stamp, run.Phase(PhaseFirstOutput), "later lines must not move the mark")
}

func TestSnapshotCopiesPhases(t *testing.T) {
	tr := NewTracker()
	run := tr.New("h")
	run.Mark(PhaseSpawned)

	snap := run.Snapshot(0)
	snap.Phases[PhaseExited] = time.Now().UTC()

	assert.NotContains(t, run.Snapshot(0).Phases, PhaseExited,
		"a snapshot must never alias live state")
}

func TestSpanRequiresBothMarks(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := RunState{Phases: map[Phase]time.Time{
		PhaseSpawned:        base,
		PhaseContainerEntry: base.Add(180 * time.Millisecond),
	}}

	d, ok := s.Span(PhaseSpawned, PhaseContainerEntry)
	require.True(t, ok)
	assert.Equal(t, 180*time.Millisecond, d)

	// A missing mark yields no duration — never one measured against the
	// zero time, which would report a span of ~2000 years.
	_, ok = s.Span(PhaseSpawned, PhaseFirstOutput)
	assert.False(t, ok)

	// Out-of-order marks are refused rather than reported negative.
	_, ok = s.Span(PhaseContainerEntry, PhaseSpawned)
	assert.False(t, ok)
}

func TestBootDurationSplitsExactFromBound(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// With the in-container mark: exact container startup, no runtime in it.
	withEntry := RunState{Phases: map[Phase]time.Time{
		PhaseSpawned:        base,
		PhaseContainerEntry: base.Add(200 * time.Millisecond),
		PhaseFirstOutput:    base.Add(500 * time.Millisecond),
	}}
	d, exact, ok := withEntry.BootDuration()
	require.True(t, ok)
	assert.True(t, exact)
	assert.Equal(t, 200*time.Millisecond, d, "boot excludes the runtime cold start")

	rt, ok := withEntry.RuntimeStartDuration()
	require.True(t, ok)
	assert.Equal(t, 300*time.Millisecond, rt, "runtime start is entry to first output")

	// Without it: only an upper bound, and it must announce itself as one —
	// quoting this number as docker's cost is the conflation the split exists
	// to prevent.
	noEntry := RunState{Phases: map[Phase]time.Time{
		PhaseSpawned:     base,
		PhaseFirstOutput: base.Add(500 * time.Millisecond),
	}}
	d, exact, ok = noEntry.BootDuration()
	require.True(t, ok)
	assert.False(t, exact)
	assert.Equal(t, 500*time.Millisecond, d)

	_, _, ok = RunState{}.BootDuration()
	assert.False(t, ok, "pre-upgrade history reports nothing, not zero")
}

func TestComputeOverheadKeepsExactAndBoundApart(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	exactRun := func(boot, rt time.Duration) RunState {
		return RunState{Status: StatusSuccess, Phases: map[Phase]time.Time{
			PhaseSpawned:        base,
			PhaseContainerEntry: base.Add(boot),
			PhaseFirstOutput:    base.Add(boot + rt),
		}}
	}
	boundRun := func(d time.Duration) RunState {
		return RunState{Status: StatusSuccess, Phases: map[Phase]time.Time{
			PhaseSpawned:     base,
			PhaseFirstOutput: base.Add(d),
		}}
	}

	o := computeOverhead([]RunState{
		exactRun(100*time.Millisecond, 300*time.Millisecond),
		exactRun(300*time.Millisecond, 500*time.Millisecond),
		boundRun(900 * time.Millisecond),
		{Status: StatusSuccess}, // pre-upgrade history contributes nothing
	})
	require.NotNil(t, o)
	assert.Equal(t, 2, o.BootSampled)
	assert.Equal(t, int64(200), o.BootAvgMS)
	assert.Equal(t, int64(300), o.BootMaxMS)
	assert.Equal(t, int64(400), o.RuntimeStartAvgMS)
	// The 900ms bound must never be averaged into the exact boot figure.
	assert.Equal(t, 1, o.BoundSampled)
	assert.Equal(t, int64(900), o.BoundAvgMS)

	assert.Nil(t, computeOverhead([]RunState{{Status: StatusSuccess}}),
		"no marks in the window means no data, not a zero overhead claim")
}

func TestComputeOverheadIncludesRunsThatDidNotSucceed(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// A container that booted and then timed out measured a real boot;
	// dropping it would bias the figure toward whatever finishes cleanly.
	o := computeOverhead([]RunState{{
		Status: StatusTimeout,
		Phases: map[Phase]time.Time{
			PhaseSpawned:        base,
			PhaseContainerEntry: base.Add(250 * time.Millisecond),
		},
	}})
	require.NotNil(t, o)
	assert.Equal(t, 1, o.BootSampled)
	assert.Equal(t, int64(250), o.BootAvgMS)
}

func TestInspectDurationMeasuresTheExtraRoundTrip(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := RunState{Phases: map[Phase]time.Time{
		PhaseSlotAcquired: base,
		PhaseInspected:    base.Add(40 * time.Millisecond),
	}}
	d, ok := s.InspectDuration()
	require.True(t, ok)
	assert.Equal(t, 40*time.Millisecond, d)

	// Non-state hooks never inspect, so they report nothing.
	_, ok = RunState{Phases: map[Phase]time.Time{PhaseSlotAcquired: base}}.InspectDuration()
	assert.False(t, ok)
}
