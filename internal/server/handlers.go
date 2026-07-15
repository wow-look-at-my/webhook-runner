package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// MaxBodyBytes caps the request body size accepted on POST /hook/{id}.
// 25 MiB matches GitHub's documented webhook payload ceiling and is
// generous for everything else.
const MaxBodyBytes = 25 * 1024 * 1024

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// The version rides along so a single probe answers both "is it up?"
	// and "which build is this?" — status stays the first field for
	// backward compatibility with anything matching on the raw body.
	writeJSON(w, http.StatusOK, struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}{Status: "ok", Version: s.version.Version})
}

// handleVersion identifies the running build (same string the `version`
// command prints, plus the VCS revision/time when the build has them).
// Registered on both ports so the deployed build is checkable from either
// side of the tunnel.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.version)
}

// hookListEntry is one row of GET /hooks: the registry summary plus the
// operator kill-switch state (disabled hooks stay loaded and listed —
// only their dispatch is gated).
type hookListEntry struct {
	hooks.Summary
	Disabled bool `json:"disabled"`
}

func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	list := s.registry.List()
	out := make([]hookListEntry, 0, len(list))
	for _, sum := range list {
		out = append(out, hookListEntry{Summary: sum, Disabled: s.overrides.HookDisabled(sum.ID)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hook, ok := s.registry.Get(id)
	if !ok {
		// Rejected requests are activity too: a caller hitting a wrong URL or
		// a stale key is exactly the misconfiguration the dashboard must be
		// able to answer "did you receive anything?" about.
		s.events.Record("hook.unknown", "trigger for unknown hook "+id+" from "+r.RemoteAddr,
			map[string]string{"hook": id})
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}

	// The operator kill switch gates DISPATCH only: the hook stays loaded
	// (image state, config, runs all intact) but no new run starts — not
	// even from the admin port (re-enable it to run it). Checked before the
	// body/auth so a runaway caller is cut off at minimal cost, and
	// answered with a deliberately distinct, loud 503 (a 404/401 would read
	// as a routing or key problem).
	if s.overrides.HookDisabled(id) {
		s.events.Record("hook.disabled_rejected",
			hook.ID+": delivery rejected — hook is disabled by operator (from "+r.RemoteAddr+")",
			map[string]string{"hook": hook.ID})
		writeError(w, http.StatusServiceUnavailable, "hook disabled by operator")
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

	if err := s.authenticate(hook, r, body); err != nil {
		s.log.Warn("auth failed", "hook", hook.ID, "remote", r.RemoteAddr, "err", err)
		// Auth errors never contain presented credentials (see auth.go), so
		// the reason is safe to surface on the dashboard.
		s.events.Record("hook.denied", hook.ID+": trigger denied from "+r.RemoteAddr+": "+err.Error(),
			map[string]string{"hook": hook.ID})
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// Friendly run title — resolved exactly ONCE per delivery, here, BEFORE
	// skip evaluation, so whichever pipeline the delivery takes (skip or
	// run) carries the same title: a skipped run should still say which PR
	// it was about. Resolution is total and never fails ("" = untitled, the
	// dashboard falls back to the run id), so it cannot reject a delivery.
	title := hook.RenderRunTitle(body, r.Header)

	// Declarative skip conditions — evaluated strictly AFTER authentication
	// (an unauthenticated caller must never probe the conditions; it gets
	// the 401 above with nothing recorded) and BEFORE any work: no image
	// build, no container, no concurrency slot. A match answers the request
	// immediately — sync hooks included, there is nothing to hold for — and
	// records a real, terminal `skipped` run naming the matched condition,
	// so "no work was done" is first-class on the runs table.
	if reason, skip := hook.EvaluateSkip(body, r.Header); skip {
		run := s.runner.Skip(hook, reason, title)
		writeJSON(w, http.StatusOK, map[string]string{
			"run_id": run.ID(),
			"status": string(runs.StatusSkipped),
			"reason": reason,
		})
		return
	}

	wantSync, syncTimeout, err := parseWaitParams(r, hook)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	run, err := s.runner.Start(s.runRequestContext(), hook, body, r.Header, title)
	if err != nil {
		// A draining server is a RETRYABLE condition, not a hook failure:
		// answer 503 so the sender (GitHub redelivers webhooks) tries the
		// restarted server instead of recording a permanent failure.
		code := http.StatusInternalServerError
		if errors.Is(err, runner.ErrDraining) {
			code = http.StatusServiceUnavailable
		}
		// The runner has already recorded the failure; return the run
		// ID anyway so the client can fetch details.
		writeJSON(w, code, map[string]string{
			"run_id": run.ID(),
			"error":  err.Error(),
		})
		return
	}

	if !wantSync {
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": run.ID()})
		return
	}

	// Synchronous: hold the connection until done or sync timeout. The hold
	// is a RESPONSE bound (wall-clock), not a run bound: the run's own
	// `timeout` is activity-based, so a run that keeps producing output can
	// legitimately outlive the syncTimeout value — when that happens the
	// response degrades to the async 202 below and the run continues
	// untouched in the background.
	select {
	case <-run.Done():
	case <-time.After(syncTimeout):
		// Don't kill the run — it can finish in the background.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"run_id": run.ID(),
			"status": "running",
			"note":   "sync timeout exceeded; run continues in background",
		})
		return
	case <-r.Context().Done():
		// Client gave up. The run continues; just stop blocking.
		return
	}

	snap := run.Snapshot(20)
	code := http.StatusOK
	if snap.Status != runs.StatusSuccess {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, snap)
}

// handleCancelRun cancels an in-flight run from the public hook port:
// POST /hook/{id}/cancel/{run}, authenticated exactly like triggering the
// hook (for api_key hooks the body is irrelevant; for signature hooks the
// signature covers whatever body the caller sent). The response is 202 —
// cancellation is a request: the run reaches "cancelled" once the runner
// has actually killed the container. Deliberately NOT gated by the
// operator kill switch: cancelling a disabled hook's in-flight runs is
// stopping work, which is what disabling is for.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hook, ok := s.registry.Get(id)
	if !ok {
		s.events.Record("hook.unknown", "cancel for unknown hook "+id+" from "+r.RemoteAddr,
			map[string]string{"hook": id})
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := s.authenticate(hook, r, body); err != nil {
		s.log.Warn("cancel auth failed", "hook", hook.ID, "remote", r.RemoteAddr, "err", err)
		s.events.Record("hook.denied", hook.ID+": cancel denied from "+r.RemoteAddr+": "+err.Error(),
			map[string]string{"hook": hook.ID})
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	run := s.tracker.Get(r.PathValue("run"))
	// A run belonging to a different hook is reported as absent — one
	// hook's key must not act on (or probe for) another hook's runs.
	if run == nil || run.HookID() != hook.ID {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	s.cancelRun(w, run)
}

// handleAdminCancelRun cancels any run from the admin port (no auth — the
// admin port is behind zero trust, same trust model as POST /reload).
func (s *Server) handleAdminCancelRun(w http.ResponseWriter, r *http.Request) {
	run := s.tracker.Get(r.PathValue("id"))
	if run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	s.cancelRun(w, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, run *runs.Run) {
	if st := run.Status(); st.Terminal() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"run_id": run.ID(),
			"status": string(st),
			"error":  "run already finished",
		})
		return
	}
	run.RequestCancel()
	s.log.Info("run cancel requested", "hook", run.HookID(), "run", run.ID())
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id": run.ID(),
		"status": "cancelling",
	})
}

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
	max := 100
	if m := r.URL.Query().Get("max"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			max = n
		}
	}
	// ?before= pages into history: only runs queued STRICTLY before the
	// instant (RFC3339, fractional seconds optional). Clients page by
	// passing the oldest `started` they already hold. Omitted = no bound.
	var before time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid before=%q: want an RFC3339 timestamp", b))
			return
		}
		before = t
	}
	writeJSON(w, http.StatusOK, s.mergedRuns(r.URL.Query().Get("hook"), before, max))
}

// mergedRuns is the /runs read path: live tracker runs (active + recent)
// merged with the persisted completed history, deduped by run ID (the live
// copy wins — for the same run it can never be older than the persisted
// one), newest-first, capped at max. A non-zero before keeps only runs
// queued strictly before it (the page cursor); zero means unbounded. Output
// is never shipped in the list view; clients fetch /runs/{id} for that.
func (s *Server) mergedRuns(hookID string, before time.Time, max int) []runs.RunState {
	// With a cursor the newest-max live window may sit entirely at-or-after
	// it, hiding older live runs behind the cap — list uncapped (the tracker
	// is bounded anyway) and let the filter plus the final cap do the work.
	liveMax := max
	if !before.IsZero() {
		liveMax = 0
	}
	var live []*runs.Run
	if hookID != "" {
		live = s.tracker.ListByHook(hookID, liveMax)
	} else {
		live = s.tracker.ListAll(liveMax)
	}
	out := make([]runs.RunState, 0, len(live))
	seen := make(map[string]struct{}, len(live))
	for _, r := range live {
		snap := r.Snapshot(0)
		if !before.IsZero() && !snap.Started.Before(before) {
			continue
		}
		snap.Output = nil
		snap.OutputTimes = nil
		out = append(out, snap)
		seen[snap.ID] = struct{}{}
	}
	if s.runstore != nil {
		var persisted []runs.RunState
		if hookID != "" {
			persisted = s.runstore.ListByHookBefore(hookID, before, max)
		} else {
			persisted = s.runstore.ListAllBefore(before, max)
		}
		for _, st := range persisted {
			if _, dup := seen[st.ID]; dup {
				continue
			}
			out = append(out, st)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Started.After(out[j].Started)
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	s.attachWaiters(out)
	return out
}

// attachWaiters decorates run snapshots with the runs currently blocked on
// resources each of them holds — the holder-side view the dashboard shows
// ("N runs waiting on this run"). DERIVED, never stored: a blocked acquire
// stamps its own run's WaitingOn with the holder(s) it is waiting on
// (re-stamped as holders change), so the live tracker already contains the
// whole graph and one pass inverts it. Two kinds contribute: a lock wait
// names its single holder (Key = the lock key), and a concurrency-group
// wait names every current slot holder (Key = "group:<name>", so renderers
// can tell the two apart). Waiter lists are sorted for stable JSON.
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

// parseWaitParams reads the optional ?wait=true and ?timeout=<go-duration>
// query parameters and merges them with the hook's Synchronous setting.
//
// Returns the desired sync mode and the maximum time we'll hold the HTTP
// response open before degrading to a background-running 202. The default
// hold is the hook's Timeout() value, but reinterpreted as WALL CLOCK: a
// held response can't wait on "activity", so while the run's timeout bounds
// inactivity, the hold bounds the response itself — a chatty run may outlive
// it, in which case the caller gets the 202 and polls /runs/{id}.
func parseWaitParams(r *http.Request, hook *hooks.Hook) (sync bool, syncTimeout time.Duration, err error) {
	q := r.URL.Query()
	sync = hook.Synchronous

	if v := q.Get("wait"); v != "" {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			return false, 0, fmt.Errorf("invalid wait=%q", v)
		}
		sync = b
	}

	syncTimeout = hook.Timeout()
	if v := q.Get("timeout"); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil {
			return false, 0, fmt.Errorf("invalid timeout=%q", v)
		}
		if d <= 0 {
			return false, 0, fmt.Errorf("timeout must be positive")
		}
		syncTimeout = d
	}
	return sync, syncTimeout, nil
}

// handleReload triggers a reload on the admin port (no auth — the admin
// port is behind zero trust). With a reload gate configured, OnReload is
// wired to the gate's Force: admin /reload DELIBERATELY bypasses the CI
// gate (jump to the remote tip, recorded verified — the operator vouched).
func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	s.events.Record("reload.requested", "reload requested via admin port", map[string]string{"source": "admin"})
	s.runReload(w)
}

// handleReloadWebhook accepts the hooks repo's GitHub webhook on the hook
// port, authenticated with the HMAC-SHA256 secret in
// WEBHOOK_RUNNER_HOOKS_REPO_SECRET. With a reload gate configured the flow
// is event-aware: a push only records the pending tip, and a green gating
// commit status is what switches the tree (internal/reloadgate). Without a
// gate, any verified POST pulls + reloads (legacy behavior).
func (s *Server) handleReloadWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !verifyLegacyHMAC(body, sig, s.reloadSecret) {
		s.log.Warn("reload auth failed", "remote", r.RemoteAddr)
		s.events.Record("reload.denied", "reload webhook with invalid signature from "+r.RemoteAddr, nil)
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	if s.gate == nil {
		s.events.Record("github.push", describePush(body), map[string]string{"source": "github"})
		s.runReload(w)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "push" {
		// Feed continuity: pushes stay announced exactly as before. The
		// gate records its own held/red/switched events; the server only
		// maps its verdict onto HTTP.
		s.events.Record("github.push", describePush(body), map[string]string{"source": "github"})
	}
	status, err := s.gate.HandleEvent(event, body)
	if err != nil {
		s.log.Error("reload failed", "err", err)
		s.events.Record("reload.failed", "reload failed: "+err.Error(), nil)
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// runReload executes the configured reload and reports the outcome.
func (s *Server) runReload(w http.ResponseWriter) {
	if s.onReload == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-op"})
		return
	}
	if err := s.onReload(); err != nil {
		s.log.Error("reload failed", "err", err)
		s.events.Record("reload.failed", "reload failed: "+err.Error(), nil)
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

// describePush summarizes a GitHub push webhook payload for the activity
// feed. Unparseable payloads still produce a generic entry — the event is
// about what arrived, not about being pretty.
func describePush(body []byte) string {
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pusher struct {
			Name string `json:"name"`
		} `json:"pusher"`
		Commits []struct {
			Message string `json:"message"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Repository.FullName == "" {
		return "push webhook received (unparsed payload)"
	}
	sha := p.After
	if len(sha) > 12 {
		sha = sha[:12]
	}
	msg := fmt.Sprintf("push to %s %s @ %s by %s (%d commit(s))",
		p.Repository.FullName, p.Ref, sha, p.Pusher.Name, len(p.Commits))
	if len(p.Commits) > 0 {
		first, _, _ := strings.Cut(p.Commits[len(p.Commits)-1].Message, "\n")
		msg += ": " + first
	}
	return msg
}

// handleEvents returns the activity feed, newest first (admin port).
// ?hook={id} narrows it to one hook's slice, same convention as /runs.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	max := 200
	if m := r.URL.Query().Get("max"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			max = n
		}
	}
	if hookID := r.URL.Query().Get("hook"); hookID != "" {
		writeJSON(w, http.StatusOK, s.events.ListByHook(hookID, max))
		return
	}
	writeJSON(w, http.StatusOK, s.events.List(max))
}

// handleImages reports per-hook image state — the tag the hook's current
// content resolves to, whether it's built (false = the next run builds
// it), and every whr-hook image on disk (admin port).
func (s *Server) handleImages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.runner.ImageStatus(s.registry.All()))
}

// handleKVStats reports per-namespace key counts and byte totals for the
// state store (admin port). This level stays value-free (and its shape is
// stable for existing consumers); keys and values are inspectable one level
// down via /kv/{namespace} and /kv/{namespace}/{key} (see kvadmin.go).
func (s *Server) handleKVStats(w http.ResponseWriter, _ *http.Request) {
	if s.kv == nil {
		writeJSON(w, http.StatusOK, []kv.NamespaceStat{})
		return
	}
	writeJSON(w, http.StatusOK, s.kv.Stats())
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := map[string]string{}
	if s.hooksRepo != "" {
		cfg["hooks_repo"] = s.hooksRepo
	}
	if s.hookBaseURL != "" {
		cfg["hook_base_url"] = s.hookBaseURL
	}
	if s.reloadSecret != "" {
		cfg["reload_secret"] = s.reloadSecret
	}
	// The persisted-history window, compacted like stats.retention ("48h") —
	// how far back /runs?before= paging can ever reach, so a client can mark
	// "history ends here". Absent when no run store is configured.
	if s.runstore != nil {
		cfg["run_retention"] = compactDuration(s.runstore.Retention())
	}
	writeJSON(w, http.StatusOK, cfg)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
