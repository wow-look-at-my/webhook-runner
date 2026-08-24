package server

import (
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// handleContainerEntry implements POST /phase/container-entry on the state API: the injected shim reports that it is running INSIDE the container, before it execs the hook's real command.
func (s *Server) handleContainerEntry(w http.ResponseWriter, _ *http.Request, ns, runID string) {
	if s.tracker == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run := s.tracker.Get(runID)
	// A manager instance is not a run and has nothing to stamp; so is a run already evicted or finished (Mark ignores terminal runs anyway).
	if run == nil || run.HookID() != ns {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run.Mark(runs.PhaseContainerEntry)
	w.WriteHeader(http.StatusNoContent)
}
