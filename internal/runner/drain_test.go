package runner

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// shutdown begins, new launches are refused loudly (ErrDraining, an
// error run in history) while nothing blocks — the launch-during-restart
// race is closed at the gate.
func TestStartRefusedWhileDraining(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker})

	assert.False(t, r.Draining())
	r.BeginShutdown()
	assert.True(t, r.Draining())

	hook := diskHook(t, dir, &hooks.Hook{ID: "h", Command: []string{"echo", "x"}})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.ErrorIs(t, err, ErrDraining)
	require.NotNil(t, run)
	assert.Equal(t, runs.StatusError, run.Status())
	assert.Contains(t, run.Error(), "restarting")
	r.Wait() // nothing in flight; must not hang
}
