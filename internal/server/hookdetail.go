package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
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
	Info HookInfo `json:"info"`
	// Config is the hook's own hook.json, passed through a key whitelist
	// (see hookconfig.go): every non-secret key exactly as authored, with
	// api_key/secret/settings values replaced by presence-only summaries.
	// Info stays the compact summary the list view shares; this is the
	// full document an operator would otherwise have to read off disk.
	Config HookConfig         `json:"config"`
	Image  runner.ImageStatus `json:"image"`
	// Disabled is the operator kill switch (operational state, not
	// hook.json config — which is why it sits beside Info, not in it):
	// true means deliveries are rejected (503) and scheduled runs skipped
	// until the operator re-enables the hook.
	Disabled bool `json:"disabled"`
	// KV is the hook's state-store namespace summary (key count + bytes —
	// this endpoint stays value-free like the bare /kv stats; keys and
	// values live behind /kv/{namespace}[/{key}], see kvadmin.go); absent
	// when the store is off or holds nothing for this hook.
	KV *kv.NamespaceStat `json:"kv,omitempty"`
	// Stats cover the live tracker window merged with the persisted run
	// history when a run store is configured (Stats.Retention names the
	// persisted window then); tracker-only otherwise. Never lifetime.
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
	// Dind reports whether the hook opted into Docker-in-Docker (--privileged
	// + an anonymous /var/lib/docker volume so it can run a nested container
	// daemon). Surfaced so an operator can see this host-root-equivalent
	// capability on the hook's drill-down page.
	Dind bool `json:"dind,omitempty"`
	// Timeout is the absolute run ceiling, when the hook sets one (empty =
	// no absolute ceiling; the run is bounded by idle_timeout, if set, or
	// runs until it exits).
	Timeout string `json:"timeout,omitempty"`
	// IdleTimeout is the no-output kill limit, when the hook sets one
	// (empty = no idle limit; only the total timeout applies, if any).
	IdleTimeout string `json:"idle_timeout,omitempty"`
	APIKey      bool   `json:"api_key"`
	// SettingsKeys are the TOP-LEVEL keys of the hook's own settings object
	// (hook.json `settings`, validated at load against the hook's
	// settings.schema.json). Keys only, never values: this port is
	// operator-only but a settings document can hold credentials.
	SettingsKeys []string `json:"settings_keys,omitempty"`
	// SkipConditions is how many skip_if conditions the hook declares
	// (0 = every authenticated delivery runs). A count, not the conditions:
	// this summary stays compact — the conditions live in hook.json, and
	// each skipped run's output names the exact one that matched.
	SkipConditions int `json:"skip_conditions,omitempty"`
}

func hookInfo(h *hooks.Hook) HookInfo {
	info := HookInfo{
		ID:               h.ID,
		Description:      h.Description,
		Synchronous:      h.Synchronous,
		Schedule:         h.Schedule,
		ConcurrencyGroup: h.ConcurrencyGroup,
		State:            h.State,
		Dind:             h.Dind,
		APIKey:           h.APIKey != "",
		SkipConditions:   len(h.SkipIf),
	}
	if d := h.Timeout(); d > 0 {
		info.Timeout = d.String()
	}
	if d := h.IdleTimeout(); d > 0 {
		info.IdleTimeout = d.String()
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(h.SettingsJSON(), &settings); err == nil {
		for k := range settings {
			info.SettingsKeys = append(info.SettingsKeys, k)
		}
	}
	sort.Strings(info.SettingsKeys)
	return info
}

// handleHookDetail returns one hook's drill-down JSON (admin port): the
// value-free config summary, image state, its KV namespace stats, and run
// stats over the merged window. A pure read — an unknown ID is a plain 404,
// same semantics as /runs/{id}.
func (s *Server) handleHookDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	detail := HookDetail{
		Info:     hookInfo(h),
		Config:   filterHookConfig(h.ManifestJSON()),
		Image:    s.runner.ImageStatus([]*hooks.Hook{h})[0],
		Disabled: s.effectiveDisabled(id),
		Stats:    s.mergedStats(id),
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

// mergedStats aggregates one hook's runs over the live tracker window merged
// with the persisted history, deduped by run ID like mergedRuns. Persisted
// entries come from the store's summary index (status + start/finish only) —
// exactly the fields the aggregation reads — so a full retention window is
// one cursor walk, never a metadata scan.
func (s *Server) mergedStats(hookID string) runs.HookRunStats {
	if s.runstore == nil {
		return s.tracker.StatsByHook(hookID)
	}
	live := s.tracker.ListByHook(hookID, 0)
	states := make([]runs.RunState, 0, len(live))
	seen := set.New[string](len(live))
	for _, r := range live {
		snap := r.Snapshot(0)
		snap.Output = nil
		snap.OutputTimes = nil
		states = append(states, snap)
		seen.Add(snap.ID)
	}
	for _, st := range s.runstore.SummariesByHook(hookID) {
		if seen.Contains(st.ID) {
			continue
		}
		states = append(states, st)
	}
	stats := runs.ComputeStats(states)
	stats.MaxTracked = runs.MaxRunsPerHook
	stats.Retention = compactDuration(s.runstore.Retention())
	return stats
}

// compactDuration renders a duration the way an operator would write it:
// time.Duration.String()'s trailing zero units dropped ("48h0m0s" → "48h"),
// but only when the remainder is still a valid duration tail — "30s" must
// not collapse to "3".
func compactDuration(d time.Duration) string {
	s := d.String()
	for _, suf := range []string{"0s", "0m"} {
		t := strings.TrimSuffix(s, suf)
		if t != s && t != "" && (strings.HasSuffix(t, "h") || strings.HasSuffix(t, "m")) {
			s = t
		}
	}
	return s
}
