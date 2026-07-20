package server

// The operator kill switch (admin port, behind Zero Trust — same trust
// model as /reload and run cancellation):
//
//	POST   /hooks/{id}/disable        flip a hook off: deliveries 503,
//	                                  scheduled runs skipped
//	POST   /hooks/{id}/enable         flip it back on
//	PUT    /concurrency/{group}/limit override a group's limit live
//	DELETE /concurrency/{group}/limit revert to the declared limit
//
// These write OPERATIONAL state (internal/overrides, persisted under the
// data dir), not hooks-repo config: a hooks-repo reload re-applies every
// override rather than wiping it, and a restart loads it back from disk.
// Every flip lands on the activity feed; a persist failure is loud (500 +
// an override.write_failed event, memory rolled back — the same rule as
// kv writes), never a quiet degrade.
//
// Precedence: hook.json's `enable` field is only the DEFAULT position of
// the switch (absent = enabled). The endpoints above write an EXPLICIT
// per-hook override — tri-state in the store — which persists and wins
// over the default in both directions, so enabling a hook that ships
// `"enable": false` sticks across reloads and restarts.

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// effectiveDisabled is the one place a hook's effective kill-switch state
// is computed: the operator's persisted explicit override when one exists,
// else the hook.json `enable` default. A hook that is not loaded (e.g. it
// failed load/validation — there is no parsed default to read) counts as
// default-enabled, so only an explicit override disables it.
func (s *Server) effectiveDisabled(id string) bool {
	defaultEnabled := true
	if h, ok := s.registry.Get(id); ok {
		defaultEnabled = h.EnabledByDefault()
	} else if m, ok := s.registry.GetManager(id); ok {
		// Managers share the hook default: absent `enable` means enabled,
		// so a declared manager works the moment it deploys. The persisted
		// operator override stays the emergency control.
		defaultEnabled = m.EnabledByDefault()
	}
	return s.overrides.HookDisabled(id, defaultEnabled)
}

func (s *Server) handleHookDisable(w http.ResponseWriter, r *http.Request) {
	s.setHookDisabled(w, r, true)
}

func (s *Server) handleHookEnable(w http.ResponseWriter, r *http.Request) {
	s.setHookDisabled(w, r, false)
}

func (s *Server) setHookDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	id := r.PathValue("id")
	if _, ok := s.registry.Get(id); !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	if s.overrides == nil {
		writeError(w, http.StatusInternalServerError, "override store not configured")
		return
	}
	changed, err := s.overrides.SetHookDisabled(id, disabled)
	if err != nil {
		s.overrideWriteError(w, "hook "+id, err)
		return
	}
	if changed {
		if disabled {
			s.log.Warn("hook disabled by operator", "hook", id)
			s.events.Record("hook.disabled",
				id+": disabled by operator — deliveries will be rejected (503) and scheduled runs skipped",
				map[string]string{"hook": id})
		} else {
			s.log.Info("hook enabled by operator", "hook", id)
			s.events.Record("hook.enabled", id+": re-enabled by operator", map[string]string{"hook": id})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hook": id, "disabled": disabled})
}

// handleConcurrencyOverrideSet overrides a declared group's limit at
// runtime: PUT /concurrency/{group}/limit with body {"limit": N}. N must
// be >= 1 — a 0 limit is rejected because it would leave queued runs
// blocked forever (the right way to stop a group's hooks entirely is to
// disable the hooks). The group must be currently declared, so a typo'd
// name can't create a phantom override.
func (s *Server) handleConcurrencyOverrideSet(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	declared, ok := s.concurrency.Declared(group)
	if !ok {
		writeError(w, http.StatusNotFound, "no such concurrency group")
		return
	}
	var body struct {
		Limit *int `json:"limit"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Limit == nil {
		writeError(w, http.StatusBadRequest, `body must be {"limit": N}`)
		return
	}
	limit := *body.Limit
	if limit < 1 {
		writeError(w, http.StatusBadRequest,
			"limit must be >= 1 — a 0 limit would deadlock queued runs; to stop a group's hooks entirely, disable the hooks instead")
		return
	}
	if s.overrides == nil {
		writeError(w, http.StatusInternalServerError, "override store not configured")
		return
	}
	changed, err := s.overrides.SetConcurrencyLimit(group, limit)
	if err != nil {
		s.overrideWriteError(w, "concurrency group "+group, err)
		return
	}
	// Persisted first, then applied live: if the process dies between the
	// two, the restart re-applies from disk — never the other way around.
	if err := s.concurrency.SetLimitOverride(group, limit); err != nil {
		// Unreachable in practice (limit and group were just validated),
		// but never swallow it.
		writeError(w, http.StatusInternalServerError, "apply override: "+err.Error())
		return
	}
	if changed {
		s.log.Warn("concurrency limit overridden by operator", "group", group, "limit", limit, "declared", declared)
		s.events.Record("concurrency.overridden",
			fmt.Sprintf("concurrency group %q: limit overridden to %d by operator (declared %d)", group, limit, declared),
			map[string]string{"group": group})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group": group, "declared": declared, "limit": limit, "overridden": true,
	})
}

// handleConcurrencyOverrideClear reverts a group to its declared limit:
// DELETE /concurrency/{group}/limit. Idempotent for declared groups; it
// also accepts a group that is no longer declared but still has a stored
// (orphaned) override, so an operator can clean those up. 404 only when
// the name matches neither.
func (s *Server) handleConcurrencyOverrideClear(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	if s.overrides == nil {
		writeError(w, http.StatusInternalServerError, "override store not configured")
		return
	}
	declared, isDeclared := s.concurrency.Declared(group)
	if _, hasOverride := s.overrides.ConcurrencyLimit(group); !isDeclared && !hasOverride {
		writeError(w, http.StatusNotFound, "no such concurrency group")
		return
	}
	changed, err := s.overrides.ClearConcurrencyLimit(group)
	if err != nil {
		s.overrideWriteError(w, "concurrency group "+group, err)
		return
	}
	s.concurrency.ClearLimitOverride(group)
	if changed {
		msg := fmt.Sprintf("concurrency group %q: limit override cleared by operator (declared limit %d back in effect)", group, declared)
		if !isDeclared {
			msg = fmt.Sprintf("concurrency group %q: orphaned limit override cleared by operator (group is not currently declared)", group)
		}
		s.log.Info("concurrency limit override cleared by operator", "group", group, "declared", declared)
		s.events.Record("concurrency.override_cleared", msg, map[string]string{"group": group})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group": group, "declared": declared, "overridden": false,
	})
}

// overrideWriteError reports a failed override persist the same way kv
// write failures are reported: the store already rolled the mutation back,
// the caller gets a 500 with the reason, and the activity feed records it.
func (s *Server) overrideWriteError(w http.ResponseWriter, what string, err error) {
	s.log.Error("persist operator override failed", "target", what, "err", err)
	s.events.Record("override.write_failed",
		"persist operator override for "+what+" failed (change rolled back): "+err.Error(), nil)
	writeError(w, http.StatusInternalServerError, "persist override: "+err.Error())
}
