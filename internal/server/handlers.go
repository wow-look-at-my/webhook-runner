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

func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.List())
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

	wantSync, syncTimeout, err := parseWaitParams(r, hook)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	run, err := s.runner.Start(s.runRequestContext(), hook, body, r.Header)
	if err != nil {
		// The runner has already recorded the failure; return the run
		// ID anyway so the client can fetch details.
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"run_id": run.ID(),
			"error":  err.Error(),
		})
		return
	}

	if !wantSync {
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": run.ID()})
		return
	}

	// Synchronous: hold the connection until done or sync timeout.
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
// has actually killed the container.
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
		writeJSON(w, http.StatusOK, run.Snapshot(tail))
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
	writeJSON(w, http.StatusOK, s.mergedRuns(r.URL.Query().Get("hook"), max))
}

// mergedRuns is the /runs read path: live tracker runs (active + recent)
// merged with the persisted completed history, deduped by run ID (the live
// copy wins — for the same run it can never be older than the persisted
// one), newest-first, capped at max. Output is never shipped in the list
// view; clients fetch /runs/{id} for that.
func (s *Server) mergedRuns(hookID string, max int) []runs.RunState {
	var live []*runs.Run
	if hookID != "" {
		live = s.tracker.ListByHook(hookID, max)
	} else {
		live = s.tracker.ListAll(max)
	}
	out := make([]runs.RunState, 0, len(live))
	seen := make(map[string]struct{}, len(live))
	for _, r := range live {
		snap := r.Snapshot(0)
		snap.Output = nil
		snap.OutputTimes = nil
		out = append(out, snap)
		seen[snap.ID] = struct{}{}
	}
	if s.runstore != nil {
		var persisted []runs.RunState
		if hookID != "" {
			persisted = s.runstore.ListByHook(hookID, max)
		} else {
			persisted = s.runstore.ListAll(max)
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
	return out
}

// parseWaitParams reads the optional ?wait=true and ?timeout=<go-duration>
// query parameters and merges them with the hook's Synchronous setting.
//
// Returns the desired sync mode and the maximum time we'll hold the HTTP
// response open before degrading to a background-running 202.
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
// port is behind zero trust).
func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	s.events.Record("reload.requested", "reload requested via admin port", map[string]string{"source": "admin"})
	s.runReload(w)
}

// handleReloadWebhook triggers a reload on the hook port, authenticated
// with the HMAC-SHA256 secret in WEBHOOK_RUNNER_HOOKS_REPO_SECRET. This
// is the endpoint a GitHub push webhook should target.
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

	s.events.Record("github.push", describePush(body), map[string]string{"source": "github"})
	s.runReload(w)
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

// handleConcurrency reports the live state of every declared concurrency
// group — its limit, how many runs are active, and how many are queued
// behind it (admin port).
func (s *Server) handleConcurrency(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.concurrency.Status())
}

// handleKVStats reports per-namespace key counts and byte totals for the
// state store (admin port). It never exposes stored values.
func (s *Server) handleKVStats(w http.ResponseWriter, _ *http.Request) {
	if s.kv == nil {
		writeJSON(w, http.StatusOK, []kv.NamespaceStat{})
		return
	}
	writeJSON(w, http.StatusOK, s.kv.Stats())
}

// handleKVKeys lists one namespace's keys with value-free metadata (admin
// port): name, size, and expiry per live key, plus totals. Unknown
// namespace is a 404. Note the exposure line here: the admin port never
// serves CONFIG secrets (api_key/env values stay value-free everywhere),
// but a hook's runtime KV DATA is the operator's to inspect — this listing
// stays value-free, and /kv/{namespace}/{key} serves the stored value.
func (s *Server) handleKVKeys(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	if s.kv == nil {
		writeError(w, http.StatusNotFound, "no such namespace")
		return
	}
	keys, ok := s.kv.Keys(ns)
	if !ok {
		writeError(w, http.StatusNotFound, "no such namespace")
		return
	}
	total := 0
	for _, k := range keys {
		total += k.Bytes
	}
	writeJSON(w, http.StatusOK, struct {
		Namespace  string       `json:"namespace"`
		Keys       []kv.KeyStat `json:"keys"`
		TotalKeys  int          `json:"total_keys"`
		TotalBytes int          `json:"total_bytes"`
	}{Namespace: ns, Keys: keys, TotalKeys: len(keys), TotalBytes: total})
}

// handleKVValue serves one stored value verbatim (admin port) — runtime KV
// data, deliberately readable by the operator (unlike config secrets, which
// no admin endpoint ever exposes). Content-Type is application/json when
// the bytes parse as JSON, else text/plain. Missing or expired keys are a
// 404 (same lazy-expiry rule as the state API's GET).
func (s *Server) handleKVValue(w http.ResponseWriter, r *http.Request) {
	if s.kv == nil {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	v, ok := s.kv.Get(r.PathValue("namespace"), r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	ct := "text/plain; charset=utf-8"
	if json.Valid(v) {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(v)
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
