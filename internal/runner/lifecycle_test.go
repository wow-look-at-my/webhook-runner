package runner

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// A hook with no timeout must not get a deadline at all: runContext is the
// seam execute() bounds container processing through, and for 0 it returns a
// plain cancellable context — the run is uncapped (bounded only by
// idle_timeout, an explicit cancel, or the container exiting).
func TestRunContextZeroTimeoutHasNoDeadline(t *testing.T) {
	ctx, cancel := runContext(context.Background(), 0)
	defer cancel()

	_, hasDeadline := ctx.Deadline()
	assert.False(t, hasDeadline, "no timeout must mean no deadline — an uncapped run has no absolute ceiling")
	select {
	case <-ctx.Done():
		t.Fatal("uncapped run context must not be done")
	default:
	}
}

func TestRunContextPositiveTimeoutArmsDeadline(t *testing.T) {
	ctx, cancel := runContext(context.Background(), time.Minute)
	defer cancel()

	_, hasDeadline := ctx.Deadline()
	assert.True(t, hasDeadline, "a set timeout must arm the total-timeout deadline")
}

// Parent cancellation still propagates to an uncapped run's context, so
// shutdown paths tied to the parent keep killing the container.
func TestRunContextZeroTimeoutFollowsParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := runContext(parent, 0)
	defer cancel()

	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not propagate to the uncapped run context")
	}
}

// End-to-end through execute(): a hook that omits timeout runs and finishes
// normally. An absent timeout means NO absolute ceiling, never a default one.
func TestRunnerNoTimeoutRunsToCompletion(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := diskHook(t, dir, &hooks.Hook{
		ID:      "uncapped",
		Command: []string{"hello"},
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	assert.Equal(t, 0, snap.ExitCode)
	assert.Contains(t, snap.Output, "hello")
}
