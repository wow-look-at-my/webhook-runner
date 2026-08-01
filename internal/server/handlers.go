package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
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
// command prints, plus the VCS revision/time when the build has them) AND
// which hooks tree it is serving: the reload gate's state rides along as
// hooks_tree, so "which hooks commit is deployed?" is answerable without
// probing hook 404s. Registered on both ports so the deployed build is
// checkable from either side of the tunnel — which deliberately exposes
// the served hooks-tree commit sha on the public hook port (an explicit
// operator request; the hooks repo itself stays private).
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		VersionInfo
		HooksTree hooksTreeState `json:"hooks_tree"`
	}{VersionInfo: s.version, HooksTree: s.hooksTreeState()})
}

// hooksTreeState is /version's hooks_tree object: which hooks tree this
// runner is serving, per the reload gate. state discriminates:
//
//   - "serving": the tree is at serving_sha; nothing newer is held.
//   - "held": pending_sha is fetched but not switched to — pending_state
//     carries the gating context's last known CI state for it, and reason
//     spells the hold out ("awaiting all-builds" / "all-builds failure").
//   - "unknown": a gate is tracking but has no serving commit recorded
//     (the boot HEAD read failed and nothing has settled since).
//   - "untracked": no gate tracks the tree — mode names why (no hooks
//     repo, or the CI-gate-disabled legacy reload flow).
//
// serving_sha is omitted rather than sent as an ambiguous empty string
// when it is unknown; verified is present exactly when a gate is
// tracking (states other than "untracked").
type hooksTreeState struct {
	State        string `json:"state"`
	ServingSHA   string `json:"serving_sha,omitempty"`
	Verified     *bool  `json:"verified,omitempty"`
	PendingSHA   string `json:"pending_sha,omitempty"`
	PendingState string `json:"pending_state,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Mode         string `json:"mode,omitempty"`
}

// hooksTreeState renders the reload gate's snapshot (a pure in-memory
// read — no git, no GitHub) into the /version hooks_tree shape.
func (s *Server) hooksTreeState() hooksTreeState {
	if s.treeState == nil {
		mode := "local hooks dir (no hooks repo)"
		if s.hooksRepo != "" {
			mode = "ci gate disabled (legacy reload)"
		}
		return hooksTreeState{State: "untracked", Mode: mode}
	}
	ts := s.treeState()
	out := hooksTreeState{ServingSHA: ts.ServingSHA, Verified: &ts.Verified}
	switch {
	case ts.PendingSHA != "":
		out.State = "held"
		out.PendingSHA = ts.PendingSHA
		out.PendingState = ts.PendingState
		if ts.PendingState == "failure" || ts.PendingState == "error" {
			out.Reason = ts.Context + " " + ts.PendingState
		} else {
			out.Reason = "awaiting " + ts.Context
		}
	case ts.ServingSHA == "":
		out.State = "unknown"
	default:
		out.State = "serving"
	}
	return out
}

// hookListEntry is one row of GET /hooks: the registry summary plus the
// EFFECTIVE kill-switch state — the operator's persisted override when one
// exists, else the hook.json `enable` default (disabled hooks stay loaded
// and listed — only their dispatch is gated).
type hookListEntry struct {
	hooks.Summary
	Disabled bool `json:"disabled"`
}

func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	list := s.registry.List()
	out := make([]hookListEntry, 0, len(list))
	for _, sum := range list {
		out = append(out, hookListEntry{Summary: sum, Disabled: s.effectiveDisabled(sum.ID)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hook, ok := s.registry.Get(id)
	if !ok {
		// Managers share the endpoint (and the id namespace): a delivery
		// for a manager id lands in its inbox instead of booting a
		// container — same auth, same skip_if, same kill switch. See
		// managers.go.
		if mgr, isManager := s.registry.GetManager(id); isManager {
			s.handleManagerTrigger(w, r, mgr)
			return
		}
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
	// even from the admin port (re-enable it to run it). Effective state:
	// explicit override first, else the hook.json `enable` default — so a
	// hook shipping `"enable": false` is born gated. Checked before the
	// body/auth so a runaway caller is cut off at minimal cost, and
	// answered with a deliberately distinct, loud 503 (a 404/401 would read
	// as a routing or key problem).
	if s.effectiveDisabled(id) {
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

	// A draining server must not LOSE the delivery. GitHub does not re-send
	// a failed one — the hooks repo's delivery-gap replay SDK exists exactly
	// because deliveries are consumed-and-lost during downtime — so the 503
	// the drain gate used to answer was an error AND a dropped webhook. Park
	// it instead and let the next process run it. Checked BEFORE Start so a
	// parked delivery leaves no errored run record: it did not fail, it is
	// waiting. By here it has passed auth and skip_if, so the spool never
	// holds an unauthenticated body.
	if s.runner.Draining() {
		if id, ok := s.spoolDelivery(hook.ID, title, r.Header, body); ok {
			writeJSON(w, http.StatusAccepted, map[string]string{
				"spooled":  id,
				"status":   "spooled",
				"detail":   "server is restarting; this delivery is parked and will run on the next start",
				"hook":     hook.ID,
				"received": time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		// No spool, or the spool is full: fall through to Start, whose drain
		// refusal gives the honest 503 + error run rather than pretending
		// the delivery is safe.
	}

	run, err := s.runner.Start(s.runRequestContext(), hook, body, r.Header, title)
	if err != nil {
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

// defaultSyncHold bounds how long a synchronous request is held open when
// the hook declares no timeout of its own: an uncapped run must not hold the
// HTTP connection indefinitely, so after this long the request degrades to a
// background-running 202 (the run itself is untouched). ?timeout= overrides.
const defaultSyncHold = 5 * time.Minute
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
	if syncTimeout <= 0 {
		syncTimeout = defaultSyncHold
	}
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
// ?exclude=run[,image,…] drops whole kind FAMILIES (the segment before the
// dot). The dashboard passes exclude=run on both feeds: run lifecycle
// belongs to the runs table, which shows each run as one row with its
// status, timings and output instead of three log lines.
//
// Both narrowings are applied by the recorder BEFORE max — see
// events.ListFiltered. A page-then-filter implementation would blank the
// feed on exactly the busy hooks it matters for.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	max := 200
	if m := r.URL.Query().Get("max"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			max = n
		}
	}
	f := events.Filter{Hook: r.URL.Query().Get("hook")}
	if ex := r.URL.Query().Get("exclude"); ex != "" {
		for _, fam := range strings.Split(ex, ",") {
			fam = strings.TrimSpace(fam)
			if fam == "" {
				continue
			}
			// A FAMILY, never a full kind: "run", not "run.started".
			// Accepting a kind here would silently match nothing and read as
			// "the filter did not work".
			if strings.Contains(fam, ".") {
				writeError(w, http.StatusBadRequest,
					"exclude takes kind families, not kinds: use "+events.Family(fam)+" to drop "+fam)
				return
			}
			f.ExcludeFamilies = append(f.ExcludeFamilies, fam)
		}
	}
	writeJSON(w, http.StatusOK, s.events.ListFiltered(f, max))
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
