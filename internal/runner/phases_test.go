package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// A real dispatch through the mock docker must leave the lifecycle marks
// the overhead measurement is computed from — in order, and covering the
// launch handoff. Without this the instrumentation could silently stop
// being stamped and every derived figure would quietly become "no data".
func TestExecuteRecordsPhaseMarks(t *testing.T) {
	dir := t.TempDir()
	dockerBin := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  dockerBin,
	})

	hook := diskHook(t, dir, &hooks.Hook{ID: "phased", Command: []string{"hello"}})
	run, err := r.Start(context.Background(), hook, []byte(`{}`), nil, "")
	require.NoError(t, err)

	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}

	snap := run.Snapshot(-1)
	for _, p := range []runs.Phase{
		runs.PhaseImageReady, runs.PhaseSlotAcquired, runs.PhaseSpawned,
		runs.PhaseFirstOutput, runs.PhaseExited,
	} {
		assert.Contains(t, snap.Phases, p, "missing mark %q", p)
	}

	// Ordering is the whole basis of the derived spans: a boot figure
	// computed from marks that can arrive out of order measures nothing.
	// The launch chain runs start-to-finish on the execute goroutine, so it
	// is a strict sequence.
	ordered := []runs.Phase{
		runs.PhaseImageReady, runs.PhaseSlotAcquired, runs.PhaseSpawned,
	}
	for i := 1; i < len(ordered); i++ {
		prev, cur := snap.Phases[ordered[i-1]], snap.Phases[ordered[i]]
		assert.False(t, cur.Before(prev), "%q must not precede %q", ordered[i], ordered[i-1])
	}
	// first_output and exited are NOT ordered against each other: different
	// goroutines stamp them. first_output comes from the streamPipe reader
	// (via AppendOutput); exited from this goroutine the moment cmd.Wait
	// returns — which is before streamWG.Wait. A container that prints one
	// line and exits leaves it buffered in the pipe, so cmd.Wait can return
	// before the reader is ever scheduled. That is not a bug to assert away:
	// first_output is defined as when output reached the SERVER, not when
	// the container emitted it. Only their common predecessor is guaranteed,
	// and that is the one the boot span is actually computed from.
	for _, p := range []runs.Phase{runs.PhaseFirstOutput, runs.PhaseExited} {
		assert.False(t, snap.Phases[p].Before(snap.Phases[runs.PhaseSpawned]),
			"%q must not precede %q", p, runs.PhaseSpawned)
	}
	assert.False(t, snap.Phases[runs.PhaseSpawned].Before(snap.Started),
		"the launch handoff cannot precede the run being accepted")

	// No shim is injected into a non-state hook, so there is no in-container
	// vantage point: the boot figure must announce itself as a bound.
	_, exact, ok := snap.BootDuration()
	require.True(t, ok)
	assert.False(t, exact, "a hook with no injected shim can only be bounded from above")
}

// A run that never launches a container must not carry launch marks — a
// zeroed boot sample from a run that never booted would silently drag every
// average toward zero.
func TestSkippedRunCarriesNoLaunchMarks(t *testing.T) {
	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  t.TempDir(),
	})

	run := r.Skip(&hooks.Hook{ID: "skipper"}, "event not interesting", "")
	snap := run.Snapshot(-1)

	assert.NotContains(t, snap.Phases, runs.PhaseSpawned)
	_, _, ok := snap.BootDuration()
	assert.False(t, ok, "a skipped delivery contributes no boot sample")
}
