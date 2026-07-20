package runner

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// StartSpawned dispatches exactly like Start, plus parent attribution: the
// run's RunState carries spawned_by — set BEFORE any terminal path, so the
// persisted snapshot has it — and the run.started/run.finished activity
// messages name the parent inline (the runRef convention, no event-schema
// change).
func TestStartSpawnedAttribution(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	rec := events.NewRecorder(50)
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker, Events: rec})
	hook := diskHook(t, dir, &hooks.Hook{ID: "child", Command: []string{"hi"}})

	run, err := r.StartSpawned(context.Background(), hook, []byte(`{}`), http.Header{}, "", "parent-hook", "parentrunid")
	require.NoError(t, err)
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	require.NotNil(t, snap.SpawnedBy, "the terminal snapshot must carry the attribution")
	assert.Equal(t, "parent-hook", snap.SpawnedBy.HookID)
	assert.Equal(t, "parentrunid", snap.SpawnedBy.RunID)

	var started, finished string
	for _, ev := range rec.ListByHook("child", 20) {
		switch ev.Kind {
		case "run.started":
			started = ev.Msg
		case "run.finished":
			finished = ev.Msg
		}
	}
	assert.Contains(t, started, ", spawned by parent-hook run parentrunid")
	assert.Contains(t, finished, ", spawned by parent-hook run parentrunid")

	// A plain Start stays unattributed — no spawned_by, no note.
	plain, err := r.Start(context.Background(), hook, []byte(`{}`), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()
	assert.Nil(t, plain.Snapshot(0).SpawnedBy)
	for _, ev := range rec.ListByHook("child", 20) {
		if ev.Fields["run"] == plain.ID() {
			assert.NotContains(t, ev.Msg, "spawned by")
		}
	}
}
