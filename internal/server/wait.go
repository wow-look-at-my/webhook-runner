package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Declared-wait bounds. One call blocks for at most maxWaitSeconds (10
// minutes) — a hook that needs to sleep longer loops, so a single forgotten
// request can't pin a connection for hours. The reason is mandatory and
// bounded: waits must be explained (the dashboard and the activity feed show
// it verbatim), never silent.
const (
	minWaitSeconds   = 1
	maxWaitSeconds   = 600
	maxWaitReasonLen = 200
	maxWaitBody      = 4096
)

// Watchdog-touch cadence for an in-flight wait. The default 5s is plenty for
// any realistic idle `timeout` (minutes); a hook with a shorter limit gets a
// proportionally faster cadence (at least ~3 touches per idle window) so a
// declared wait can never lose the race against its own watchdog. The floor
// keeps a pathologically tiny timeout from turning the ticker into a hot loop.
const (
	maxWaitTouchInterval = 5 * time.Second
	minWaitTouchInterval = 50 * time.Millisecond
)

// waitResult is the POST /wait response body. A full wait is
// {"waited": N}; an interrupted one adds "interrupted": true plus the cause,
// so a hook can tell "my time is up" from "my run is being torn down".
type waitResult struct {
	// Waited is the whole seconds actually spent waiting — the requested
	// amount after a full wait, less when interrupted.
	Waited      int    `json:"waited"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Cause       string `json:"cause,omitempty"`
}

func interruptedResult(started time.Time, cause string) waitResult {
	return waitResult{
		Waited:      int(time.Since(started).Seconds()),
		Interrupted: true,
		Cause:       cause,
	}
}

// handleWait implements POST /wait on the state API: a first-class declared
// sleep. The hook says how long it wants to pause and why; the server blocks
// the request for that long and returns {"waited": N}. While the wait is in
// flight the run is visibly "waiting <reason>" on the dashboard AND the wait
// counts as activity for the hook's idle `timeout` (the handler keeps
// touching the run's watchdog), so an in-process sleep is safe — no
// defer-to-next-tick contortions needed. The wait ends early — with a
// distinguishable {"interrupted": true} body — when the run finishes, a
// cancel is requested, or the client hangs up.
//
// Only state: true hooks can wait: the endpoint rides the same socket +
// bearer token as /kv, which only those hooks have.
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request, ns, runID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWaitBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		Seconds int    `json:"seconds"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Seconds < minWaitSeconds || req.Seconds > maxWaitSeconds {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"invalid seconds: must be %d..%d (loop for longer waits)", minWaitSeconds, maxWaitSeconds))
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required: say what the run is waiting for")
		return
	}
	if len(reason) > maxWaitReasonLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("reason too long: max %d characters", maxWaitReasonLen))
		return
	}

	if s.tracker == nil {
		writeError(w, http.StatusServiceUnavailable, "run tracking not configured")
		return
	}
	run := s.tracker.Get(runID)
	// A token always names a real (namespace, run) pair, but the run can be
	// over — or, pathologically, evicted from the bounded tracker. There is
	// nothing to attribute or protect then, so refuse rather than blocking a
	// connection nobody owns. The HookID check mirrors handleCancelRun's
	// cross-hook guard; the HMAC already binds the pair, so it's belt-only.
	if run == nil || run.HookID() != ns || run.Status().Terminal() {
		writeError(w, http.StatusConflict, "run is not active")
		return
	}

	d := time.Duration(req.Seconds) * time.Second
	seq := run.BeginWait(reason, time.Now().UTC().Add(d))
	defer run.EndWait(seq)
	run.TouchActivity()
	s.events.Record("run.wait",
		fmt.Sprintf("%s run %s waiting %ds: %s", ns, runID, req.Seconds, reason),
		map[string]string{"hook": ns, "run": runID})

	started := time.Now()
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	touch := time.NewTicker(s.waitTouchInterval(ns))
	defer touch.Stop()
	for {
		select {
		case <-deadline.C:
			// One last touch so the hook starts its next step with a full
			// idle window, not one partially burned by the final tick gap.
			run.TouchActivity()
			writeJSON(w, http.StatusOK, waitResult{Waited: req.Seconds})
			return
		case <-touch.C:
			run.TouchActivity()
		case <-run.Done():
			writeJSON(w, http.StatusOK, interruptedResult(started, "run finished"))
			return
		case <-run.Cancelled():
			writeJSON(w, http.StatusOK, interruptedResult(started, "run cancelled"))
			return
		case <-r.Context().Done():
			// Client hung up (the container is going away); nothing left to
			// tell it. The deferred EndWait clears the dashboard state.
			return
		}
	}
}

// waitTouchInterval picks how often an in-flight wait resets the caller's
// idle watchdog: at least ~3 touches per idle window (see the constants
// above). The namespace is the hook ID, so the hook's own timeout is a
// registry lookup away; an unknown hook (or no registry, as in tests) gets
// the default cadence, which suits DefaultTimeout and anything longer.
func (s *Server) waitTouchInterval(ns string) time.Duration {
	interval := maxWaitTouchInterval
	if s.registry != nil {
		if h, ok := s.registry.Get(ns); ok {
			if d := h.Timeout() / 3; d < interval {
				interval = d
			}
		}
	}
	if interval < minWaitTouchInterval {
		interval = minWaitTouchInterval
	}
	return interval
}
