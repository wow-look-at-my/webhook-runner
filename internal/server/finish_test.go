package server

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func newFinishFixture(t *testing.T) (*kv.Store, *runs.Tracker) {
	t.Helper()
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("s"), nil)
	require.NoError(t, err)
	return store, runs.NewTracker()
}

// The finish seam is the PRIMARY lock-release mechanism: Tracker.Finish fires
// the OnFinish observer exactly on every terminal path, and the callback
// must free everything the run still holds — regardless of how it ended and
// regardless of whether the history write works.
func TestRunFinishReleasesLocks(t *testing.T) {
	store, tracker := newFinishFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := events.NewRecorder(10)
	var recorded []runs.RunState
	tracker.SetOnFinish(RunFinishCallback(store, func(st runs.RunState) error {
		recorded = append(recorded, st)
		return nil
	}, rec, logger))

	// Simulate terminal paths a run can take; each must drop its locks.
	terminals := []func(r *runs.Run){
		func(r *runs.Run) { r.Finish(runs.StatusSuccess, 0, "") },
		func(r *runs.Run) { r.Finish(runs.StatusError, 1, "boom") },
		func(r *runs.Run) { r.Finish(runs.StatusTimeout, -1, "timed out") },
		func(r *runs.Run) { r.Finish(runs.StatusCancelled, -1, "cancelled") },
	}
	for i, terminal := range terminals {
		run := tracker.New("my-hook")
		_, err := store.AcquireLock("my-hook", "lease", run.ID(), 0)
		require.NoError(t, err)

		terminal(run)

		// The lock is gone: a different run can take it immediately.
		other := tracker.New("my-hook")
		_, err = store.AcquireLock("my-hook", "lease", other.ID(), 0)
		require.NoErrorf(t, err, "terminal path %d left the lock held", i)
		require.Equal(t, 1, store.ReleaseRunLocks(other.ID()))
	}
	require.Len(t, recorded, len(terminals)) // the history write still happened, after the release

	// The sweep is visible: lock.released_on_finish event per leftover.
	var swept int
	for _, e := range rec.List(100) {
		if e.Kind == "lock.released_on_finish" {
			swept++
		}
	}
	require.Equal(t, len(terminals), swept)
}

// Lock release must come : a runstore write error cannot leave a dead
// run's locks held.
func TestRunFinishReleasesLocksEvenWhenHistoryWriteFails(t *testing.T) {
	store, tracker := newFinishFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tracker.SetOnFinish(RunFinishCallback(store, func(runs.RunState) error {
		return errors.New("disk full")
	}, nil, logger)) // nil recorder: the events ring is nil-safe by contract

	run := tracker.New("my-hook")
	_, err := store.AcquireLock("my-hook", "lease", run.ID(), 0)
	require.NoError(t, err)
	run.Finish(runs.StatusError, 1, "boom")

	other := tracker.New("my-hook")
	_, err = store.AcquireLock("my-hook", "lease", other.ID(), 0)
	require.NoError(t, err, "locks must release even when the history write fails")
}

// A run that released everything itself sweeps nothing — and emits no event.
func TestRunFinishQuietWhenNoLeftovers(t *testing.T) {
	store, tracker := newFinishFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := events.NewRecorder(10)
	tracker.SetOnFinish(RunFinishCallback(store, func(runs.RunState) error { return nil }, rec, logger))

	run := tracker.New("my-hook")
	_, err := store.AcquireLock("my-hook", "lease", run.ID(), 0)
	require.NoError(t, err)
	require.NoError(t, store.ReleaseLock("my-hook", "lease", run.ID())) // explicit early release
	run.Finish(runs.StatusSuccess, 0, "")

	for _, e := range rec.List(100) {
		require.NotEqual(t, "lock.released_on_finish", e.Kind, "no leftovers -> no sweep event")
	}
}
