// GET /hooks/{id}/diagnostics (admin port): downloadable bundle of
// everything an operator would otherwise gather by hand across /hooks/{id},
// /runs?hook=, /events?hook=, /attention and /concurrency — built so an
// operator hitting an incident (a wedged webhook, a stuck downstream gate,
// a run that will not finish) can pull file and hand it over for
// debugging instead of pasting screenshots of different panels.
//
// Kept to the SAME value-free contract as the rest of the admin surface,
// with deliberate widening: run OUTPUT is included, because /runs/{id}
// already serves it in full to anyone with admin access, and a diagnostic
// bundle without the run logs would be missing the thing incident
// debugging is usually about. Settings values, env values, and KV values
// are never included here — HookInfo.SettingsKeys stays names-only, exactly
// like the rest of the admin API, because this file is meant to be pasted
// into a bug report or a chat, which is a much wider audience than the
// -Trust-gated dashboard itself.
package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// DiagnosticsBundle is the GET /hooks/{id}/diagnostics document.
type DiagnosticsBundle struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Runner      VersionInfo `json:"runner_version"`
	// HooksTree is the reload gate's served-tree state (same shape as /version's hooks_tree) — a hook misbehaving because the fleet is running.
	HooksTree *reloadgate.TreeState `json:"hooks_tree,omitempty"`

	Hook HookDetail `json:"hook"`
	// ConcurrencyGroup is the hook's declared group's live state (holders, waiting, limit) — present only when the hook sets.
	ConcurrencyGroup *concurrencyGroupView `json:"concurrency_group,omitempty"`

	// Runs are this hook's most recent runs, newest , WITH output (tailed per RunsTail — see handleHookDiagnostics), merged live +.
	Runs []runs.RunState `json:"runs"`
	// RunsTruncated is set when more runs exist than were included, so a short bundle never reads as "this hook has only ever run N times".
	RunsTruncated bool `json:"runs_truncated,omitempty"`

	// Events are this hook's activity-feed entries, newest — every kind, unlike the dashboard's exclude=run (a diagnostic bundle wants.
	Events []events.Event `json:"events"`

	// Attention holds every CURRENTLY ACTIVE needs-attention entry scoped to this hook, plus any server-wide entry (empty Hook) that could.
	Attention []attention.Entry `json:"attention"`
}

// defaultDiagnosticsRuns/Tail/Events are the un-parameterized bundle sizes: generous enough to cover a real incident (a stuck gate, a wedged lock) in.
const (
	defaultDiagnosticsRuns   = 50
	maxDiagnosticsRuns       = 500
	defaultDiagnosticsTail   = 500
	defaultDiagnosticsEvents = 300
	maxDiagnosticsEvents     = 2000
)

// handleHookDiagnostics answers with a single JSON bundle covering
// hook's config, image/KV/stats summary, concurrency-group state, recent
// runs (with output), activity events, and any active needs-attention
// entries — everything an operator would otherwise gather by hand across
// several dashboard panels. Content-Disposition makes a browser click save
// it straight to a file instead of navigating to raw JSON.
//
// ?runs= (default , capped at ) bounds how many recent runs to
// include. ?tail= (default , - = full transcript) bounds how many
// output lines each included run carries — the same knob /runs/{id}?tail=
// already exposes, so a caller who wants full logs for every run can ask
// for them. ?events= (default , capped at ) bounds the activity feed.
func (s *Server) handleHookDiagnostics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}

	runsMax := intParam(r, "runs", defaultDiagnosticsRuns, 1, maxDiagnosticsRuns)
	tail := defaultDiagnosticsTail
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			tail = n
		}
	}
	eventsMax := intParam(r, "events", defaultDiagnosticsEvents, 1, maxDiagnosticsEvents)

	info := hookInfo(h)
	bundle := DiagnosticsBundle{
		GeneratedAt: time.Now(),
		Runner:      s.version,
		Hook: HookDetail{
			Info:     info,
			Image:    s.runner.ImageStatus([]*hooks.Hook{h})[0],
			Disabled: s.effectiveDisabled(id),
			Stats:    s.mergedStats(id),
		},
		Events:    s.events.ListFiltered(events.Filter{Hook: id}, eventsMax),
		Attention: s.attentionFor(id),
	}
	if s.kv != nil {
		for _, ns := range s.kv.Stats() {
			if ns.Namespace == id {
				stat := ns
				bundle.Hook.KV = &stat
				break
			}
		}
	}
	if s.treeState != nil {
		ts := s.treeState()
		bundle.HooksTree = &ts
	}
	if info.ConcurrencyGroup != "" && s.concurrency != nil {
		for _, gv := range s.groupViews() {
			if gv.Name == info.ConcurrencyGroup {
				v := gv
				bundle.ConcurrencyGroup = &v
				break
			}
		}
	}

	all := s.diagnosticRuns(id, runsMax+1, tail)
	if len(all) > runsMax {
		bundle.RunsTruncated = true
		all = all[:runsMax]
	}
	bundle.Runs = all

	filename := fmt.Sprintf("%s-diagnostics-%s.json", id, time.Now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	writeJSON(w, http.StatusOK, bundle)
}

// intParam reads a positive-int query parameter, clamped to [min, max];
// missing or unparseable falls back to def.
func intParam(r *http.Request, name string, def, min, max int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// attentionFor is the needs-attention entries relevant to hook: its own scoped entries plus every server-wide entry (empty Hook — -hooks, reload-held, the containerized-TMPDIR hazard), oldest .
func (s *Server) attentionFor(hookID string) []attention.Entry {
	all := s.attention.Snapshot() // nil-aggregator safe: empty, never nil
	out := make([]attention.Entry, 0, len(all))
	for _, e := range all {
		if e.Hook == "" || e.Hook == hookID {
			out = append(out, e)
		}
	}
	return out
}

// diagnosticRuns is /runs?hook=<id>'s merge (live tracker + persisted
// history, deduped by run ID, newest-) with difference: output is
// kept (tailed to `tail` lines; - = full), because a diagnostic bundle
// without the logs is missing the point. max bounds the returned count;
// pass max+ from the caller to detect truncation without a query.
func (s *Server) diagnosticRuns(hookID string, max, tail int) []runs.RunState {
	live := s.tracker.ListByHook(hookID, 0)
	out := make([]runs.RunState, 0, len(live))
	seen := set.New[string](len(live))
	for _, r := range live {
		snap := r.Snapshot(tail)
		out = append(out, snap)
		seen.Add(snap.ID)
	}
	if s.runstore != nil {
		for _, summary := range s.runstore.ListByHookBeforeFiltered(hookID, time.Time{}, max, nil) {
			if seen.Contains(summary.ID) {
				continue
			}
			if full, ok := s.runstore.Get(summary.ID); ok {
				tailOutput(&full, tail)
				out = append(out, full)
			} else {
				out = append(out, summary)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	s.attachWaiters(out)
	return out
}
