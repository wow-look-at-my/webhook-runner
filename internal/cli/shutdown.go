package cli

import (
	"context"
	"log/slog"
	"time"
)

// shutdownGrace bounds the final close of the three listeners, measured from
// AFTER the drain.
const shutdownGrace = 10 * time.Second

// shutdownDeps is the graceful teardown's world, expressed as behavior rather
// than concrete servers. The ORDER below is the only thing here that can be
// wrong, and a docker-free test can check it exactly (shutdown_test.go).
type shutdownDeps struct {
	refuseNewRuns func()
	stopManagers  func()
	drainRuns     func()
	closeHook     func(context.Context) error
	closeState    func(context.Context) error
	closeStreams  func()
	closeAdmin    func(context.Context) error
	logger        *slog.Logger
}

// gracefulShutdown winds the server down in the one order that loses nothing.
func gracefulShutdown(d shutdownDeps) {
	// Refuse NEW runs immediately: a run launched by this dying process
	// races the state-socket handover (its shim would dial a socket the
	// next server replaces). Deliveries are not rejected though — they are
	// PARKED (internal/spool) and answered 202, because GitHub does not
	// re-send a failed delivery. In-flight runs drain below.
	d.refuseNewRuns()
	// Stop manager instances gracefully (docker stop; SIGTERM + grace)
	// BEFORE anything else winds down: the lease releases only when this
	// process exits, so the successor process's supervisor cannot start
	// replacement instances until ours are provably gone.
	d.stopManagers()
	// ORDER IS THE POINT. EVERY listener stays UP across the drain:
	//   - the hook port, because the drain can take as long as the longest
	//     run (a CI job is minutes). Stopping it first left the process
	//     alive with nothing listening for that entire stretch, and every
	//     delivery arriving in it got connection-refused — silently lost,
	//     since GitHub does not retry. Now they spool and answer 202.
	//   - the state socket, because DRAINING RUNS ARE STILL USING IT: locks,
	//     /wait, /title all ride it. Closing it before the drain pulled the
	//     floor out from under the very runs being drained.
	//   - the admin port, because the drain is precisely when an operator
	//     needs to see what is still running and why the deploy is slow. It
	//     used to close ahead of a wait measured in whole CI jobs, so a
	//     rolling update took the dashboard away for minutes while hooks
	//     went on executing: connection-refused through the tunnel, with no
	//     way to watch the drain it was waiting on. Nothing on this port
	//     starts work — new runs are already refused above and the
	//     supervisor will not restart an instance once shut down — so
	//     serving it to the end costs nothing and tells the operator
	//     everything.
	d.drainRuns()
	// The grace window starts HERE, not before the drain — a deadline armed
	// pre-drain would already be blown and turn a graceful close into an
	// abrupt one.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := d.closeHook(ctx); err != nil {
		d.logger.Warn("hook server shutdown", "err", err)
	}
	if err := d.closeState(ctx); err != nil {
		d.logger.Warn("state server shutdown", "err", err)
	}
	// Disconnect /runs/stream clients before closing the admin port: that
	// close waits for in-flight handlers, and a stream handler holds its
	// response open until its subscription closes (or its client goes away).
	d.closeStreams()
	if err := d.closeAdmin(ctx); err != nil {
		d.logger.Warn("admin server shutdown", "err", err)
	}
}
