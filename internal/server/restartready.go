// The docker-updater PRE-CHECK: one GET that answers "is it safe to replace
// this container right now?" — 200 yes, 503 no. Reachable two ways: the
// standard pre-update path above, which needs only
// `docker-updater.well-known.port=9001` on the container so discovery knows
// which port to probe, or the older explicit label
// `docker-updater.pre-check.url=:9001/restart-ready` (a ":"-prefixed URL
// resolves against the container's own bridge IP). Prefer the first: the label
// overrides discovery entirely and marks the container "nonstandard". Either
// way a non-2xx makes docker-updater skip that cycle and retry on the next one.
//
// A restart is not merely lossy, it is DESTRUCTIVE to work in flight. Runs
// alive at shutdown are never recorded, their containers are orphaned on the
// daemon, and the next boot's SweepOrphanContainers kills every labeled
// leftover — so bouncing mid-job reaps the gha-runner container serving a
// live CI job. /health answers "is the process up", which is a different
// question and always yes.
//
// Manager instances deliberately do NOT block: a flat restart is their
// declared contract, the supervisor brings them back, and gating on them
// would mean never updating (an instance is always running).
package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The standard paths docker-updater discovers by itself, served on the admin mux as aliases of /health and /restart-ready.
const (
	wellKnownHealth    = "/.well-known/docker-updater/health"
	wellKnownPreUpdate = "/.well-known/docker-updater/pre-update"
)

// DefaultRestartMaxDefer bounds how long a busy fleet may hold off an update. docker-updater retries forever on a non-2xx and has no.
const DefaultRestartMaxDefer = 6 * time.Hour

// restartGate tracks how long the check has been continuously blocked.
type restartGate struct {
	mu        sync.Mutex
	blockedAt time.Time // zero = not currently blocked
	forced    bool      // the force already fired for this stretch (log once)
}

// observe records a blocked/ready answer and reports whether the max-defer
// force applies, plus when the current blocked stretch began.
func (g *restartGate) observe(blocked bool, maxDefer time.Duration, now time.Time) (force bool, since time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !blocked {
		g.blockedAt, g.forced = time.Time{}, false
		return false, time.Time{}
	}
	if g.blockedAt.IsZero() {
		g.blockedAt = now
	}
	if maxDefer <= 0 {
		return false, g.blockedAt // force disabled: block for as long as it takes
	}
	return now.Sub(g.blockedAt) >= maxDefer, g.blockedAt
}

// firstForce reports whether this is the first forced answer of the current blocked stretch, so the loud line is emitted once rather than.
func (g *restartGate) firstForce() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.forced {
		return false
	}
	g.forced = true
	return true
}

type restartReadyResponse struct {
	Ready       bool     `json:"ready"`
	ActiveRuns  int      `json:"active_runs"`
	RunIDs      []string `json:"run_ids,omitempty"`
	BlockedFor  string   `json:"blocked_for,omitempty"`
	Forced      bool     `json:"forced,omitempty"`
	MaxDefer    string   `json:"max_defer,omitempty"`
	Explanation string   `json:"explanation"`
}

// restartReadyRunIDCap bounds the id list in the response — the count is the decision, the ids are a courtesy for whoever reads the skip reason.
const restartReadyRunIDCap = 20

func (s *Server) handleRestartReady(w http.ResponseWriter, _ *http.Request) {
	active := s.tracker.ActiveIDs()
	now := time.Now()
	force, since := s.restart.observe(len(active) > 0, s.restartMaxDefer, now)

	resp := restartReadyResponse{ActiveRuns: len(active)}
	if s.restartMaxDefer > 0 {
		resp.MaxDefer = s.restartMaxDefer.String()
	}
	switch {
	case len(active) == 0:
		resp.Ready, resp.Explanation = true, "no runs in flight — safe to replace the container"
	case force:
		resp.Ready, resp.Forced = true, true
		resp.BlockedFor = now.Sub(since).Truncate(time.Second).String()
		resp.Explanation = "max defer elapsed — allowing the update despite runs in flight"
		if s.restart.firstForce() {
			msg := "restart-ready: " + resp.BlockedFor + " of continuous in-flight work has exceeded the " +
				s.restartMaxDefer.String() + " max defer — answering READY with " +
				strconv.Itoa(len(active)) + " run(s) still active; they will be lost and their containers reaped at the next boot"
			s.log.Warn("restart pre-check forced", "active", len(active), "blocked_for", resp.BlockedFor)
			s.events.Record("restart.deferred_force", msg, nil)
		}
	default:
		resp.BlockedFor = now.Sub(since).Truncate(time.Second).String()
		resp.Explanation = "runs in flight — replacing the container now would lose them and reap their containers"
	}
	if n := len(active); n > 0 {
		if n > restartReadyRunIDCap {
			active = active[:restartReadyRunIDCap]
		}
		resp.RunIDs = active
	}

	status := http.StatusServiceUnavailable
	if resp.Ready {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}
