package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// MaxBodyBytes caps the request body size accepted on POST /hook/{id}.
// 25 MiB matches GitHub's documented webhook payload ceiling and is
// generous for everything else.
const MaxBodyBytes = 25 * 1024 * 1024

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.List())
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hook, ok := s.registry.Get(id)
	if !ok {
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

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run := s.tracker.Get(id)
	if run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	tail := -1
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			tail = n
		}
	}
	snap := run.Snapshot(tail)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	max := 100
	if m := r.URL.Query().Get("max"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			max = n
		}
	}
	hookID := r.URL.Query().Get("hook")

	var src []*runs.Run
	if hookID != "" {
		src = s.tracker.ListByHook(hookID, max)
	} else {
		src = s.tracker.ListAll(max)
	}
	out := make([]runs.RunState, 0, len(src))
	for _, r := range src {
		// Don't ship full output in the list view; clients can fetch
		// /runs/{id} for that.
		s := r.Snapshot(0)
		s.Output = nil
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
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
	if s.onReload == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-op"})
		return
	}
	if err := s.onReload(); err != nil {
		s.log.Error("reload failed", "err", err)
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
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
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	if s.onReload == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-op"})
		return
	}
	if err := s.onReload(); err != nil {
		s.log.Error("reload failed", "err", err)
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
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
