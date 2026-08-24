package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The teardown order is the whole contract, and every step of it was learned
// from a delivery or an operator losing something mid-deploy. These pin it.

// recordingShutdown returns deps that append each step's name to *order, plus
// a hook to run something extra during the drain.
func recordingShutdown(order *[]string, duringDrain func()) shutdownDeps {
	add := func(name string) func() { return func() { *order = append(*order, name) } }
	addCtx := func(name string) func(context.Context) error {
		return func(context.Context) error { *order = append(*order, name); return nil }
	}
	return shutdownDeps{
		refuseNewRuns: add("refuse"),
		stopManagers:  add("managers"),
		drainRuns: func() {
			*order = append(*order, "drain")
			if duringDrain != nil {
				duringDrain()
			}
		},
		closeHook:    addCtx("hook"),
		closeState:   addCtx("state"),
		closeStreams: add("streams"),
		closeAdmin:   addCtx("admin"),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The regression this file exists for: the admin port used to close BEFORE
// the drain, so a rolling update took the dashboard away for as long as the
// longest run (a CI job) while hooks kept executing — the one stretch an
// operator most needs to see what is still running.
func TestAdminPortServesUntilAfterTheDrain(t *testing.T) {
	var order []string
	gracefulShutdown(recordingShutdown(&order, nil))

	require.Contains(t, order, "admin")
	assert.Greater(t, indexOf(order, "admin"), indexOf(order, "drain"),
		"the admin port must outlive the drain: it is how an operator watches it")
}

func TestEveryListenerOutlivesTheDrain(t *testing.T) {
	var order []string
	gracefulShutdown(recordingShutdown(&order, nil))

	drain := indexOf(order, "drain")
	for _, listener := range []string{"hook", "state", "admin"} {
		assert.Greater(t, indexOf(order, listener), drain,
			listener+" closed before in-flight runs finished draining")
	}
}

func TestShutdownRunsTheFullOrder(t *testing.T) {
	var order []string
	gracefulShutdown(recordingShutdown(&order, nil))

	assert.Equal(t, []string{
		"refuse",   // no new runs races the state-socket handover
		"managers", // the lease frees only on exit, so stop instances early
		"drain",    // in-flight runs finish, still holding every listener
		"hook", "state",
		"streams", // SSE handlers must let go before the admin close waits on them
		"admin",
	}, order)
}

// A stream handler holds its response open until its subscription closes, and the admin close waits for in-flight handlers — so closing streams after it would hang until the grace deadline instead of being.
func TestStreamsCloseBeforeTheAdminPort(t *testing.T) {
	var order []string
	gracefulShutdown(recordingShutdown(&order, nil))

	assert.Less(t, indexOf(order, "streams"), indexOf(order, "admin"))
}

// The grace window is armed AFTER the drain. Armed before, a drain longer than
// the window hands every closer an already-expired context, turning a graceful
// close into an abrupt one.
func TestGraceWindowStartsAfterTheDrain(t *testing.T) {
	var order []string
	var deadline time.Time
	d := recordingShutdown(&order, func() { time.Sleep(20 * time.Millisecond) })
	d.closeHook = func(ctx context.Context) error {
		order = append(order, "hook")
		deadline, _ = ctx.Deadline()
		return nil
	}
	start := time.Now()
	gracefulShutdown(d)

	require.False(t, deadline.IsZero(), "the closers get a bounded context")
	assert.Greater(t, deadline.Sub(start), shutdownGrace,
		"the deadline was armed before the drain, so it is already burning")
}

// A closer that fails must not stop the ones after it: a hung hook port cannot
// be allowed to strand the admin port or the state socket.
func TestAFailingCloserDoesNotStopTheRest(t *testing.T) {
	var order []string
	d := recordingShutdown(&order, nil)
	d.closeHook = func(context.Context) error {
		order = append(order, "hook")
		return context.DeadlineExceeded
	}
	gracefulShutdown(d)

	assert.Equal(t, []string{"refuse", "managers", "drain", "hook", "state", "streams", "admin"}, order)
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
