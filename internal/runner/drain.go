// The shutdown drain gate. A run LAUNCHED by a dying runner process races
// the state-socket handover (its kvproxy shim would dial a socket the next
// process unlinks and re-binds) and the process's own teardown — observed
// in production as a fresh container's lock call dying with a
// connection-level "fetch failed" inside a deploy window, which the hook
// then reported as a real failure with external side effects.
// shutdown begins, new launches are refused with a retryable error;
// in-flight runs are untouched (Wait drains them), and the kvproxy shim's
// flat dial retry covers THEIR calls across the brief handover.
package runner

import "errors"

// ErrDraining is returned by Start BeginShutdown has been called.
var ErrDraining = errors.New("server is restarting; not accepting new runs")

// BeginShutdown flips the runner into drain mode: every later Start is refused with ErrDraining (recorded as an error run so the caller's.
func (r *Runner) BeginShutdown() { r.draining.Store(true) }

// Draining reports whether BeginShutdown has been called (handlers use it to refuse deliveries cheaply before creating any run state).
func (r *Runner) Draining() bool { return r.draining.Load() }
