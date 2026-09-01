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

	// The launch chain runs start-to-finish on goroutine, so these marks
	// arrive in a strict sequence.
	ordered := []runs.Phase{
		runs.PhaseImageReady, runs.PhaseSlotAcquired, runs.PhaseSpawned,
	}
	for i := 1; i < len(ordered); i++ {
		prev, cur := snap.Phases[ordered[i-1]], snap.Phases[ordered[i]]
		assert.False(t, cur.Before(prev), "%q must not precede %q", ordered[i], ordered[i-1])
	}
	// first_output and exited are stamped by different goroutines and are
	// NOT ordered against each other. Only their common predecessor,
	// PhaseSpawned, is guaranteed.
	for _, p := range []runs.Phase{runs.PhaseFirstOutput, runs.PhaseExited} {
		assert.False(t, snap.Phases[p].Before(snap.Phases[runs.PhaseSpawned]),
			"%q must not precede %q", p, runs.PhaseSpawned)
	}
	assert.False(t, snap.Phases[runs.PhaseSpawned].Before(snap.Started),
		"the launch handoff cannot precede the run being accepted")

	// No shim in a non-state hook, so the boot figure is only a bound.
	_, exact, ok := snap.BootDuration()
	require.True(t, ok)
	assert.False(t, exact, "a hook with no injected shim can only be bounded from above")
}

// A run that never launches a container must not carry launch marks — a
// zeroed boot sample from a run that never booted would silently drag every
// average toward .
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
