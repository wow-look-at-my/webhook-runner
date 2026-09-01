// Manager entity server surface: delivery dispatch into the inbox (with
// synchronous holds and per-delivery github_status), the state API's
// POST /inbox/next, and the admin roster/kill-switch/restart endpoints.
// Managers are -CLASS — never runs: nothing here touches the tracker,
// the run store, or the timeline.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/managers"
)

// ManagerControl is what the server needs from the manager supervisor.
// Implemented by *managers.Supervisor; nil means no manager support is
// wired (tests, or a serve without the supervisor).
type ManagerControl interface {
	Deliver(id string, headers http.Header, payload []byte) *managers.Delivered
	InboxNext(ctx context.Context, id, instanceID string, wait time.Duration) (managers.Event, bool, error)
	IsCurrentInstance(id, instanceID string) bool
	TouchInstance(id, instanceID string) bool
	SetInstanceTitle(id, instanceID, title string) bool
	Statuses() []managers.Status
	StatusFor(id string) (managers.Status, bool)
	OutputTail(id string) []string
	RequestStop(id, reason string)
	Poke(id string)
	// SetOnChange is the manager surface's push seam: instance output, inbox depth/stamps, and state transitions move the Managers panel.
	SetOnChange(fn func())
}

// defaultInboxNextWait is POST /inbox/next's hold when the body names no wait_seconds — long enough that a healthy manager's poll loop is.
const defaultInboxNextWaitSeconds = 60

// handleManagerTrigger is handleTrigger's manager branch: same kill-switch
// -> body -> AUTH -> skip_if ordering as hooks (auth strictly before the
// conditions), then the delivery lands in the manager's bounded inbox
// instead of booting a container. synchronous (or ?wait=true) holds the
// response until the manager finishes processing that inbox event — its
// next /inbox/next call — degrading to the async on the sync timeout,
// exactly the hook rule. github_status, when enabled, posts pending now
// and success/error when the event settles.
func (s *Server) handleManagerTrigger(w http.ResponseWriter, r *http.Request, mgr *hooks.Manager) {
	id := mgr.ID
	if s.effectiveDisabled(id) {
		s.events.Record("hook.disabled_rejected",
			id+": delivery rejected — manager is disabled by operator (from "+r.RemoteAddr+")",
			map[string]string{"hook": id})
		writeError(w, http.StatusServiceUnavailable, "manager disabled by operator")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	if err := s.authenticate(mgr.Hook, r, body); err != nil {
		s.log.Warn("manager auth failed", "manager", id, "remote", r.RemoteAddr, "err", err)
		s.events.Record("hook.denied", id+": trigger denied from "+r.RemoteAddr+": "+err.Error(),
			map[string]string{"hook": id})
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// skip_if for managers is pure noise filtering in front of the inbox —
	// there is no per-delivery run to record, so a match answers and is
	// only an event on the feed. Strictly after auth, like hooks.
	if reason, skip := mgr.EvaluateSkip(body, r.Header); skip {
		s.events.Record("manager.skipped",
			fmt.Sprintf("%s: delivery skipped: %s", id, reason),
			map[string]string{"hook": id})
		writeJSON(w, http.StatusOK, map[string]string{
			"manager": id,
			"status":  "skipped",
			"reason":  reason,
		})
		return
	}

	wantSync, syncTimeout, err := parseWaitParams(r, mgr.Hook)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.managers == nil {
		writeError(w, http.StatusServiceUnavailable, "manager supervision not configured")
		return
	}
	d := s.managers.Deliver(id, r.Header, body)
	if d == nil {
		// Mid-reload race: the registry knows the manager but the supervisor's set hasn't caught up. Retryable — GitHub redelivers.
		writeError(w, http.StatusServiceUnavailable, "manager not supervised yet; retry")
		return
	}

	// Per-delivery github_status, manager-shaped: pending on acceptance,
	// success when the manager finishes processing the event, error when it
	// is abandoned (dropped on overflow, or the instance died mid-event).
	if s.gh != nil && mgr.GitHubStatus != nil && mgr.GitHubStatus.Enabled {
		s.gh.PostManagerEventStart(context.Background(), mgr.Hook, body)
		go func() {
			<-d.Done()
			s.gh.PostManagerEventResult(context.Background(), mgr.Hook, body, d.Completed())
		}()
	}

	if !wantSync {
		writeJSON(w, http.StatusAccepted, map[string]string{
			"manager": id,
			"event":   d.Event.ID,
			"status":  "queued",
		})
		return
	}

	// Synchronous: hold until the manager finishes THAT event. The hold is
	// a response bound, never an event bound — on timeout the response
	// degrades to the async and the event stays queued/processing.
	select {
	case <-d.Done():
		if d.Completed() {
			writeJSON(w, http.StatusOK, map[string]string{
				"manager": id,
				"event":   d.Event.ID,
				"status":  "processed",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"manager": id,
			"event":   d.Event.ID,
			"status":  "abandoned",
			"error":   "the manager instance ended (or the event was dropped) before processing finished",
		})
		return
	case <-time.After(syncTimeout):
		writeJSON(w, http.StatusAccepted, map[string]any{
			"manager": id,
			"event":   d.Event.ID,
			"status":  "queued",
			"note":    "sync timeout exceeded; the event remains queued for the manager",
		})
		return
	case <-r.Context().Done():
		return
	}
}

// handleInboxNext implements POST /inbox/next on the state API: the
// manager's long-poll event pop. Body {"wait_seconds": ..} (default
// ). = an event (JSON, see managers.Event); = the wait elapsed
// with nothing queued (re-poll, flat); = the caller is not the
// manager's CURRENT instance (a stale token from a dead instance — the
// API-level single-instance guard); = the namespace is not a manager.
func (s *Server) handleInboxNext(w http.ResponseWriter, r *http.Request, ns, runID string) {
	if s.managers == nil || s.registry == nil {
		writeError(w, http.StatusNotFound, "not a manager")
		return
	}
	if _, ok := s.registry.GetManager(ns); !ok {
		writeError(w, http.StatusNotFound, "not a manager")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWaitBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	waitSecs := defaultInboxNextWaitSeconds
	if len(body) > 0 {
		var req struct {
			WaitSeconds *int `json:"wait_seconds"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if req.WaitSeconds != nil {
			if *req.WaitSeconds < minWaitSeconds || *req.WaitSeconds > maxWaitSeconds {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"invalid wait_seconds: must be %d..%d", minWaitSeconds, maxWaitSeconds))
				return
			}
			waitSecs = *req.WaitSeconds
		}
	}

	ev, ok, err := s.managers.InboxNext(r.Context(), ns, runID, time.Duration(waitSecs)*time.Second)
	if err != nil {
		writeError(w, http.StatusConflict, "not the current manager instance")
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// managerCaller reports whether (ns, id) names a manager's CURRENT live instance — the manager-flavored caller-liveness check /wait.
func (s *Server) managerCaller(ns, id string) bool {
	return s.managers != nil && s.managers.IsCurrentInstance(ns, id)
}

// handleListManagers implements GET /managers (admin): the roster.
func (s *Server) handleListManagers(w http.ResponseWriter, _ *http.Request) {
	if s.managers == nil {
		writeJSON(w, http.StatusOK, []managers.Status{})
		return
	}
	writeJSON(w, http.StatusOK, s.managers.Statuses())
}

// managerDetail is GET /managers/{id}: the roster row plus the live instance's recent output tail (managers are not runs — their logs live.
type managerDetail struct {
	managers.Status
	Output []string `json:"output,omitempty"`
}

func (s *Server) handleManagerDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.managers == nil {
		writeError(w, http.StatusNotFound, "no such manager")
		return
	}
	st, ok := s.managers.StatusFor(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such manager")
		return
	}
	writeJSON(w, http.StatusOK, managerDetail{Status: st, Output: s.managers.OutputTail(id)})
}

// handleManagerDisable / handleManagerEnable are the manager kill switch — the same persisted overrides store as hooks (ids share .
func (s *Server) handleManagerDisable(w http.ResponseWriter, r *http.Request) {
	s.setManagerDisabled(w, r, true)
}

func (s *Server) handleManagerEnable(w http.ResponseWriter, r *http.Request) {
	s.setManagerDisabled(w, r, false)
}

func (s *Server) setManagerDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	id := r.PathValue("id")
	if s.registry == nil {
		writeError(w, http.StatusNotFound, "no such manager")
		return
	}
	if _, ok := s.registry.GetManager(id); !ok {
		writeError(w, http.StatusNotFound, "no such manager")
		return
	}
	if s.overrides == nil {
		writeError(w, http.StatusInternalServerError, "override store not configured")
		return
	}
	changed, err := s.overrides.SetHookDisabled(id, disabled)
	if err != nil {
		s.overrideWriteError(w, "manager "+id, err)
		return
	}
	if changed {
		if disabled {
			s.log.Warn("manager disabled by operator", "manager", id)
			s.events.Record("manager.disabled",
				id+": disabled by operator — the instance stops and deliveries are rejected (503)",
				map[string]string{"hook": id})
		} else {
			s.log.Info("manager enabled by operator", "manager", id)
			s.events.Record("manager.enabled", id+": enabled by operator — the supervisor starts an instance",
				map[string]string{"hook": id})
		}
	}
	if s.managers != nil {
		if disabled {
			s.managers.RequestStop(id, "disabled by operator")
		} else {
			s.managers.Poke(id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"manager": id, "disabled": disabled})
}

// handleManagerRestart bounces the live instance (graceful stop; the
// supervisor starts a fresh immediately) — the operator's "kick it"
// button, replacing the run-cancel a session-as-run would have had.
func (s *Server) handleManagerRestart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.registry == nil {
		writeError(w, http.StatusNotFound, "no such manager")
		return
	}
	if _, ok := s.registry.GetManager(id); !ok {
		writeError(w, http.StatusNotFound, "no such manager")
		return
	}
	if s.managers == nil {
		writeError(w, http.StatusServiceUnavailable, "manager supervision not configured")
		return
	}
	s.events.Record("manager.restart_requested", id+": instance restart requested by operator",
		map[string]string{"hook": id})
	s.managers.RequestStop(id, "restarted by operator")
	writeJSON(w, http.StatusAccepted, map[string]string{"manager": id, "status": "restarting"})
}
