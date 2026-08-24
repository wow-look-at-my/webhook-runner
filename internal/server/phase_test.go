package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// --- POST /phase/container-entry (state API): the in-container mark -------

func TestContainerEntryStampsTheRun(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	run.Mark(runs.PhaseSpawned)

	rr := stateReq(t, s, "POST", "/phase/container-entry", store.Token("h", run.ID()), nil)
	require.Equal(t, 204, rr.Code)

	snap := run.Snapshot(0)
	require.Contains(t, snap.Phases, runs.PhaseContainerEntry)

	// The mark closes the span the host opened at launch — this pair IS the container-overhead measurement.
	d, exact, ok := snap.BootDuration()
	require.True(t, ok)
	assert.True(t, exact, "the in-container mark makes the boot figure exact")
	assert.GreaterOrEqual(t, d.Nanoseconds(), int64(0))
}

func TestContainerEntryRequiresAuthButNeverFailsARun(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()

	// Auth is the same gate as every other state route: an unauthenticated caller must not be able to stamp somebody else's run.
	require.Equal(t, 401, stateReq(t, s, "POST", "/phase/container-entry", "", nil).Code)
	require.Equal(t, 401, stateReq(t, s, "POST", "/phase/container-entry", "h.bogus", nil).Code)

	// Past the gate, everything the shim cannot control answers 204 rather than an error: instrumentation must never be able to fail a run. An unknown run, a cross-hook token, and a finished run all no-op.
	for _, tok := range []string{
		store.Token("h", "nosuchrun"),
		store.Token("other", run.ID()),
	} {
		rr := stateReq(t, s, "POST", "/phase/container-entry", tok, nil)
		assert.Equal(t, 204, rr.Code)
	}
	assert.NotContains(t, run.Snapshot(0).Phases, runs.PhaseContainerEntry,
		"a token that does not name this run must not stamp it")

	done := tr.New("h")
	done.Finish(runs.StatusSuccess, 0, "")
	require.Equal(t, 204,
		stateReq(t, s, "POST", "/phase/container-entry", store.Token("h", done.ID()), nil).Code)
	assert.NotContains(t, done.Snapshot(0).Phases, runs.PhaseContainerEntry)
}

func TestContainerEntryIsStampedOnce(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	require.Equal(t, 204, stateReq(t, s, "POST", "/phase/container-entry", tok, nil).Code)
	first := run.Phase(runs.PhaseContainerEntry)
	require.Equal(t, 204, stateReq(t, s, "POST", "/phase/container-entry", tok, strings.NewReader("")).Code)

	assert.Equal(t, first, run.Phase(runs.PhaseContainerEntry),
		"a retrying shim must not move the container's entry instant")
}
