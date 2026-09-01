package cli

import (
	"context"
	"log/slog"
	"time"
)

// shutdownGrace bounds the final close of the listeners, measured from AFTER the drain.
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

// gracefulShutdown winds the server down in the order that loses nothing.
func gracefulShutdown(d shutdownDeps) {
	// Refuse NEW runs immediately: a run launched by this dying process races the state-socket handover (its shim would dial a socket the next.
	d.refuseNewRuns()
	// Stop manager instances gracefully (docker stop; SIGTERM + grace) BEFORE anything else winds down: the lease releases only when this.
	d.stopManagers()
	// ORDER IS THE POINT.
	d.drainRuns()
	// The grace window starts HERE, not before the drain — a deadline armed pre-drain would already be blown and turn a graceful close into an.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := d.closeHook(ctx); err != nil {
		d.logger.Warn("hook server shutdown", "err", err)
	}
	if err := d.closeState(ctx); err != nil {
		d.logger.Warn("state server shutdown", "err", err)
	}
	// Disconnect /runs/stream clients before closing the admin port: that close waits for in-flight handlers, and a stream handler holds its.
	d.closeStreams()
	if err := d.closeAdmin(ctx); err != nil {
		d.logger.Warn("admin server shutdown", "err", err)
	}
}
