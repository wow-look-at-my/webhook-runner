package server

import (
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// handleContainerEntry implements POST /phase/container-entry on the state
// API: the injected shim reports that it is running INSIDE the container,
// before it execs the hook's real command.
//
// This is the only mark a run cannot observe from the host, and it is the
// one that makes container overhead measurable rather than arguable: the
// host stamps PhaseSpawned when `docker run` is handed off, this stamps the
// far side, and the difference is Docker's own create/namespace/overlay
// cost with no hook runtime in it. Without it the only available bound is
// spawned→first-output, which silently includes the interpreter's cold
// start — exactly the conflation that turns a measurement into a guess.
//
// The server stamps its own receive time rather than trusting a timestamp
// in the body: the socket is local (sub-millisecond), and a client-supplied
// instant would be both unverifiable and a needless clock-agreement
// question. The cost is that the mark includes the shim binary's own
// process start — which is honest, since the shim IS the container's
// entrypoint and its start is part of what launching this container costs.
//
// Best-effort by contract: every failure mode answers 204 or a plain error
// the shim ignores. Instrumentation must never be able to fail a run.
func (s *Server) handleContainerEntry(w http.ResponseWriter, _ *http.Request, ns, runID string) {
	if s.tracker == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run := s.tracker.Get(runID)
	// A manager instance is not a run and has nothing to stamp; so is a run
	// already evicted or finished (Mark ignores terminal runs anyway). Both
	// are 204: the shim has no recourse and must not treat this as fatal.
	if run == nil || run.HookID() != ns {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run.Mark(runs.PhaseContainerEntry)
	w.WriteHeader(http.StatusNoContent)
}
