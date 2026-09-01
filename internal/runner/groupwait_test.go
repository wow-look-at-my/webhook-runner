package runner

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// TestRunnerQueuedRunExposesGroupWait: while queued behind a saturated
// concurrency group, a run's waiting_on mirrors its place in the line —
// kind "group", the group name, the current holders, and its -based
// position — and the wait clears the moment it acquires the slot (and is
// absent from the terminal snapshot).
func TestRunnerQueuedRunExposesGroupWait(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 1}},
	})
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Groups:  mgr,
	})

	hookA := diskHook(t, dir, &hooks.Hook{ID: "a", Command: []string{"SLEEP_1"}, ConcurrencyGroup: "g"})
	runA, err := r.Start(context.Background(), hookA, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	waitStatus(t, runA, runs.StatusRunning, 2*time.Second)

	hookB := diskHook(t, dir, &hooks.Hook{ID: "b", Command: []string{"echo", "b"}, ConcurrencyGroup: "g"})
	runB, err := r.Start(context.Background(), hookB, []byte("p"), http.Header{}, "")
	require.NoError(t, err)

	// B is pending and its waiting_on names the group, the holder (A), and position (next in line).
	deadline := time.After(2 * time.Second)
	for {
		snap := runB.Snapshot(0)
		w := snap.WaitingOn
		if w != nil && w.Kind == runs.WaitingOnGroup {
			assert.Equal(t, "g", w.Key)
			assert.Equal(t, []string{runA.ID()}, w.HolderRunIDs)
			assert.Equal(t, 1, w.Position)
			break
		}
		select {
		case <-deadline:
			t.Fatalf("queued run never exposed a group wait (waiting_on=%+v)", w)
		case <-time.After(10 * time.Millisecond):
		}
	}

	r.Wait()
	assert.Equal(t, runs.StatusSuccess, runB.Status())
	// The terminal snapshot must not read as waiting (acquire cleared it, and Finish clears any leftover as a backstop).
	assert.Nil(t, runB.Snapshot(0).WaitingOn)
}
