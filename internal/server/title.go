package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// maxTitleBody caps the tiny JSON body of a title update — same bound as a
// /wait request, its sibling on the state API.
const maxTitleBody = 4096

// handleRunTitle implements POST /title on the state API: the running hook
// names its own run — {"title": "wow-look-at-my/go-toolchain#47"} — and the
// live RunState (dashboard rows, the timeline chip, /runs, and the terminal
// snapshot the run store will persist) picks it up immediately.
//
// This is the mid-run override for subjects only known once the run reaches
// them: a fleet sweep doesn't know which repo matters until it gets there,
// so no hook.json template could have said. It REPLACES any template title;
// the last write wins. Bounds mirror the template side (trimmed, max
// hooks.MaxRunTitleLen) — but where the renderer clamps silently, a hook
// speaking for itself can be told no: empty and overlong titles are 400s,
// mirroring /wait's reason validation.
//
// Only state: true hooks can title themselves: the endpoint rides the same
// socket + bearer token as /kv and /wait, which only those hooks have.
func (s *Server) handleRunTitle(w http.ResponseWriter, r *http.Request, ns, runID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTitleBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required: say what the run is about")
		return
	}
	if len(title) > hooks.MaxRunTitleLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("title too long: max %d characters", hooks.MaxRunTitleLen))
		return
	}

	if s.tracker == nil {
		writeError(w, http.StatusServiceUnavailable, "run tracking not configured")
		return
	}
	run := s.tracker.Get(runID)
	// Same rule as /wait: a token always names a real (namespace, run)
	// pair, but the run can be over — or evicted from the bounded tracker.
	// A terminal run's snapshot already persisted, so a late title would
	// diverge history from the live view; refuse instead. The HookID check
	// mirrors handleCancelRun's cross-hook guard (belt-only — the HMAC
	// already binds the pair).
	if run == nil || run.HookID() != ns || run.Status().Terminal() {
		// Manager instances are not runs: /title names the INSTANCE on the
		// Managers panel (the run_title template's mid-flight override —
		// e.g. the coordinator titling itself with its reconcile summary).
		if s.managerCaller(ns, runID) && s.managers.SetInstanceTitle(ns, runID, title) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusConflict, "run is not active")
		return
	}
	run.SetTitle(title)
	w.WriteHeader(http.StatusNoContent)
}
