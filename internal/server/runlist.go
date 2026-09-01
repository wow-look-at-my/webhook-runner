// The /runs read surface: the single-run read (tracker -> persisted-history
// fallback), the merged list with its active-truth partition, the ?live=
// active-set view, and the derived holder-side waiter decoration. Split
// from handlers.go, which keeps the trigger/cancel/reload handlers.
package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tail := -1
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			tail = n
		}
	}
	if run := s.tracker.Get(id); run != nil {
		st := []runs.RunState{run.Snapshot(tail)}
		s.attachWaiters(st)
		writeJSON(w, http.StatusOK, st[0])
		return
	}
	// The tracker window is bounded; fall back to the persisted history for
	// runs it has evicted (or that finished before a restart).
	if s.runstore != nil {
		if snap, ok := s.runstore.Get(id); ok {
			tailOutput(&snap, tail)
			writeJSON(w, http.StatusOK, snap)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such run")
}

// tailOutput trims a persisted state's output to the requested tail length
// (negative = full), the same contract as Run.Snapshot.
func tailOutput(st *runs.RunState, tail int) {
	if tail < 0 || tail >= len(st.Output) {
		return
	}
	st.Output = st.Output[len(st.Output)-tail:]
	if tail < len(st.OutputTimes) {
		st.OutputTimes = st.OutputTimes[len(st.OutputTimes)-tail:]
	}
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	// ?live=: exactly the current ACTIVE (non-terminal) set — the -shot truth fetch for clients reconciling against the stream's hb.
	exclude, err := excludedStatuses(r.URL.Query().Get("exclude"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if v := r.URL.Query().Get("live"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil && b {
			writeJSON(w, http.StatusOK, s.liveRuns(r.URL.Query().Get("hook"), exclude))
			return
		}
	}
	max := 100
	if m := r.URL.Query().Get("max"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			max = n
		}
	}
	// ?before= pages into history: only runs queued STRICTLY before the instant (RFC, fractional seconds optional).
	var before time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid before=%q: want an RFC3339 timestamp", b))
			return
		}
		before = t
	}
	writeJSON(w, http.StatusOK, s.mergedRuns(r.URL.Query().Get("hook"), before, max, exclude))
}

// excludedStatuses parses ?exclude= into a set. Empty/absent yields nil (no
// filtering). An unrecognized status is a , matching ?before=: a filter
// silently matching nothing would look exactly like "this hook has no runs",
// which is the failure mode this whole parameter exists to remove.
func excludedStatuses(raw string) (map[runs.Status]bool, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	out := map[runs.Status]bool{}
	for _, part := range strings.Split(raw, ",") {
		st := runs.Status(strings.TrimSpace(part))
		if st == "" {
			continue
		}
		if !runs.KnownStatus(st) {
			return nil, fmt.Errorf("invalid exclude=%q: %q is not a run status", raw, st)
		}
		out[st] = true
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// keeper turns the exclusion set into the predicate the run store's walk
// takes (nil for "keep everything", which skips the filtering entirely).
func keeper(exclude map[runs.Status]bool) func(runs.Status) bool {
	if len(exclude) == 0 {
		return nil
	}
	return func(st runs.Status) bool { return !exclude[st] }
}

// liveRuns is GET /runs?live=: every non-terminal tracked run, output
// stripped, newest-, waiters attached — the same row shape as /runs.
// Always non-nil so an idle server answers [] (a real "nothing is active"
// verdict), never null.
func (s *Server) liveRuns(hookID string, exclude map[runs.Status]bool) []runs.RunState {
	var live []*runs.Run
	if hookID != "" {
		live = s.tracker.ListByHook(hookID, 0)
	} else {
		live = s.tracker.ListAll(0)
	}
	out := make([]runs.RunState, 0, len(live))
	for _, r := range live {
		snap := r.Snapshot(0)
		if snap.Status.Terminal() || exclude[snap.Status] {
			continue
		}
		snap.Output = nil
		snap.OutputTimes = nil
		out = append(out, snap)
	}
	s.attachWaiters(out)
	return out
}

// mergedRuns is the /runs read path: live tracker runs (active + recent)
// merged with the persisted completed history, deduped by run ID (the live
// copy wins — for the same run it can never be older than the persisted
// ), newest-. On the CURSORLESS live windows (the plain /runs
// list, the SSE connect snapshot) the cap applies to TERMINAL rows only:
// every active (non-terminal) run is ALWAYS included, however small max is
// — a live window must never hide work that is happening right now (the
// old total cap cut still-running runs out of flood-time snapshots, and
// clients read absence as termination). A non- before keeps only runs
// queued strictly before it (the page cursor) and KEEPS the legacy
// newest-max total cap: history pages must be complete down to their
// oldest row — the paging walk advances its cursor from it, and an
// uncapped ancient active row would make the walk skip terminal history.
// Output is never shipped in the list view; clients fetch /runs/{id}.
// A non-empty exclude drops those statuses from BOTH partitions before the
// cap is applied. It is an explicit operator request, so it also overrides
// the always-include-active rule above: hiding "running" hides running runs.
func (s *Server) mergedRuns(hookID string, before time.Time, max int, exclude map[runs.Status]bool) []runs.RunState {
	// List the tracker uncapped: the active partition must be COMPLETE (a newest-max pre-cut could hide older active runs behind newer terminal.
	var live []*runs.Run
	if hookID != "" {
		live = s.tracker.ListByHook(hookID, 0)
	} else {
		live = s.tracker.ListAll(0)
	}
	paged := !before.IsZero()
	// active stays non-nil so an empty merge still serializes as [].
	active := make([]runs.RunState, 0, len(live))
	var capped []runs.RunState
	seen := set.New[string](len(live))
	for _, r := range live {
		snap := r.Snapshot(0)
		if paged && !snap.Started.Before(before) {
			continue
		}
		if exclude[snap.Status] {
			continue
		}
		snap.Output = nil
		snap.OutputTimes = nil
		seen.Add(snap.ID)
		if !paged && !snap.Status.Terminal() {
			active = append(active, snap)
		} else {
			capped = append(capped, snap)
		}
	}
	if s.runstore != nil {
		// Persisted history is terminal by construction (write- at terminal status), so it always lands in the capped partition.
		var persisted []runs.RunState
		if hookID != "" {
			persisted = s.runstore.ListByHookBeforeFiltered(hookID, before, max, keeper(exclude))
		} else {
			persisted = s.runstore.ListAllBeforeFiltered(before, max, keeper(exclude))
		}
		for _, st := range persisted {
			if seen.Contains(st.ID) {
				continue
			}
			capped = append(capped, st)
		}
	}
	sort.SliceStable(capped, func(i, j int) bool {
		return capped[i].Started.After(capped[j].Started)
	})
	if max > 0 && len(capped) > max {
		capped = capped[:max]
	}
	out := append(active, capped...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Started.After(out[j].Started)
	})
	s.attachWaiters(out)
	return out
}

// attachWaiters decorates run snapshots with the runs currently blocked on
// resources each of them holds — the holder-side view the dashboard shows
// ("N runs waiting on this run"). DERIVED, never stored: a blocked acquire
// stamps its own run's WaitingOn with the holder(s) it is waiting on
// (re-stamped as holders change), so the live tracker already contains the
// whole graph and pass inverts it. kinds contribute: a lock wait
// names its single holder (Key = the lock key), and a concurrency-group
// wait names every current slot holder (Key = "group:<name>", so renderers
// can tell the apart). Waiter lists are sorted for stable JSON.
// Terminal/persisted runs never hold locks or slots, so they simply never
// match.
func (s *Server) attachWaiters(states []runs.RunState) {
	if s.tracker == nil || len(states) == 0 {
		return
	}
	var byHolder map[string][]runs.Waiter
	add := func(holderID string, waiter runs.Waiter) {
		if holderID == "" {
			return
		}
		if byHolder == nil {
			byHolder = make(map[string][]runs.Waiter)
		}
		byHolder[holderID] = append(byHolder[holderID], waiter)
	}
	for _, r := range s.tracker.ListAll(0) {
		snap := r.Snapshot(0)
		w := snap.WaitingOn
		if w == nil {
			continue
		}
		switch w.Kind {
		case runs.WaitingOnLock:
			add(w.HolderRunID, runs.Waiter{RunID: snap.ID, HookID: snap.HookID, Key: w.Key})
		case runs.WaitingOnGroup:
			for _, h := range w.HolderRunIDs {
				add(h, runs.Waiter{RunID: snap.ID, HookID: snap.HookID, Key: groupWaiterKey(w.Key)})
			}
		}
	}
	if byHolder == nil {
		return
	}
	for _, ws := range byHolder {
		sort.Slice(ws, func(i, j int) bool { return ws[i].RunID < ws[j].RunID })
	}
	for i := range states {
		states[i].Waiters = byHolder[states[i].ID]
	}
}
