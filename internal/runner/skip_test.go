package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Skip is the entire pipeline for a skip_if match: a real, tracked, terminal
// run — and zero side effects. No docker invocation, no per-run temp files,
// no onStart/onFinish (GitHub status) callbacks; but the tracker's finish
// seam fires (so the skip persists to history) and a run.skipped event lands
// on the activity feed.
func TestRunnerSkipBootsNoContainer(t *testing.T) {
	dir := t.TempDir()
	invoked := filepath.Join(dir, "docker-invocations.log")
	docker := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(docker,
		[]byte("#!/bin/sh\necho \"$@\" >> \""+invoked+"\"\nexit 0\n"), 0o755))

	tracker := runs.NewTracker()
	var finished []runs.RunState
	tracker.SetOnFinish(func(st runs.RunState) { finished = append(finished, st) })
	rec := events.NewRecorder(10)
	callbacks := 0
	r := New(Options{
		Tracker:  tracker,
		Logger:   newSilentLogger(),
		TmpDir:   dir,
		Docker:   docker,
		Events:   rec,
		OnStart:  func(*hooks.Hook, *runs.Run, []byte) { callbacks++ },
		OnFinish: func(*hooks.Hook, *runs.Run, []byte) { callbacks++ },
	})

	run := r.Skip(&hooks.Hook{ID: "h"}, `skip_if[0]: header x-github-event == "workflow_run"`, "")
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSkipped, snap.Status)
	assert.True(t, snap.Status.Terminal())
	assert.Equal(t, 0, snap.ExitCode)
	assert.Empty(t, snap.Error)
	assert.True(t, snap.StartedAt.IsZero(), "nothing ever launched, so no processing start")
	assert.Equal(t, []string{`skipped: skip_if[0]: header x-github-event == "workflow_run"`}, snap.Output)

	// No container, no temp files, no container-work callbacks.
	assert.NoFileExists(t, invoked, "docker must never be invoked for a skip")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "wh-"), "unexpected per-run temp dir %s", e.Name())
	}
	assert.Zero(t, callbacks, "onStart/onFinish report container work; none happened")

	// The finish seam fired exactly once — the same write-once path that persists every other terminal run.
	require.Len(t, finished, 1)
	assert.Equal(t, runs.StatusSkipped, finished[0].Status)
	assert.Equal(t, "h", finished[0].HookID)

	evs := rec.List(0)
	require.Len(t, evs, 1)
	assert.Equal(t, "run.skipped", evs[0].Kind)
	assert.Contains(t, evs[0].Msg, `header x-github-event == "workflow_run"`)
	assert.Equal(t, "h", evs[0].Fields["hook"])
	assert.Equal(t, run.ID(), evs[0].Fields["run"])
}
