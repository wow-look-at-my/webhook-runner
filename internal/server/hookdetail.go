package server

import (
	"net/http"
	"sort"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// HookDetail is the admin drill-down view of one hook — the data source for
// the dashboard's per-app page. An "app" is exactly one hook for now; the
// shape stays hook-scoped so a later grouping concept can aggregate several
// of these without reshaping the fields.
type HookDetail struct {
	Info  HookInfo           `json:"info"`
	Image runner.ImageStatus `json:"image"`
	// KV is the hook's state-store namespace summary (key count + bytes,
	// never values — same rule as /kv); absent when the store is off or
	// holds nothing for this hook.
	KV *kv.NamespaceStat `json:"kv,omitempty"`
	// Stats cover the tracker's bounded in-memory window, not lifetime.
	Stats runs.HookRunStats `json:"stats"`
}

// HookInfo is the value-free config summary of a hook: which features are
// configured, never the configuration's secrets. api_key is a boolean and
// env lists variable NAMES only — key material and env values must never
// appear in any admin response.
type HookInfo struct {
	ID               string `json:"id"`
	Description      string `json:"description,omitempty"`
	Synchronous      bool   `json:"synchronous,omitempty"`
	Schedule         string `json:"schedule,omitempty"`
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`
	State            bool   `json:"state,omitempty"`
	// Timeout is the effective run timeout (hook.json's or the default).
	Timeout string   `json:"timeout"`
	APIKey  bool     `json:"api_key"`
	EnvKeys []string `json:"env_keys,omitempty"`
}

func hookInfo(h *hooks.Hook) HookInfo {
	info := HookInfo{
		ID:               h.ID,
		Description:      h.Description,
		Synchronous:      h.Synchronous,
		Schedule:         h.Schedule,
		ConcurrencyGroup: h.ConcurrencyGroup,
		State:            h.State,
		Timeout:          h.Timeout().String(),
		APIKey:           h.APIKey != "",
	}
	for k := range h.Env {
		info.EnvKeys = append(info.EnvKeys, k)
	}
	sort.Strings(info.EnvKeys)
	return info
}

// handleHookDetail returns one hook's drill-down JSON (admin port): the
// value-free config summary, image state, its KV namespace stats, and run
// stats over the bounded recent window. A pure read — an unknown ID is a
// plain 404, same semantics as /runs/{id}.
func (s *Server) handleHookDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	detail := HookDetail{
		Info:  hookInfo(h),
		Image: s.runner.ImageStatus([]*hooks.Hook{h})[0],
		Stats: s.tracker.StatsByHook(id),
	}
	if s.kv != nil {
		for _, ns := range s.kv.Stats() {
			if ns.Namespace == id {
				stat := ns
				detail.KV = &stat
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, detail)
}
