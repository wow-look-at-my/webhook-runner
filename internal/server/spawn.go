package server

// POST /spawn on the state API — the runner-native spawn primitive: a
// permitted state hook starts runs of ANOTHER hook through the runner
// itself, replacing the retired pattern of a coordinator hook POSTing
// HMAC-signed synthetic webhooks at the public hook endpoints.
//
// The CALLER (parent hook + parent run) comes from the verified bearer
// token, never the body — the same auth path as /wait and the lock routes.
// Authorization is a DENY-BY-DEFAULT allowlist configured RUNNER-side
// (WEBHOOK_RUNNER_SPAWN_ALLOW; see SpawnAllowlist), deliberately NOT a
// hook.json field: the published hook schema stays untouched, so consumers
// need zero new hook.json fields.
//
// A spawned run is a NORMAL run of the target hook, dispatched the way the
// scheduler's Fire callback dispatches (registry lookup → runner start with
// context.Background() and a payload/headers pair): tracked, group-gated
// (the target's concurrency_group applies — excess spawns queue as
// pending), KV-enabled, dashboard-visible, persisted via the OnFinish seam.
// skip_if is BYPASSED exactly like scheduled fires: a spawn is operator
// machinery's own doing, not an unwanted delivery — the target's in-code
// guards still run. Each spawned run carries spawned_by {run_id, hook_id}
// attribution (see internal/runs).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Spawn request bounds. The payload is webhook-sized (it becomes each
// spawned run's HOOK_PAYLOAD_FILE), not upload-sized; the count cap keeps a
// misbehaving parent from fork-bombing the runner in one request.
const (
	minSpawnCount = 1
	maxSpawnCount = 100
	// maxSpawnPayloadBytes bounds the payload handed to EACH spawned run.
	maxSpawnPayloadBytes = 256 * 1024
	// maxSpawnBody bounds the whole request body: the payload plus slack
	// for the JSON envelope around it.
	maxSpawnBody = maxSpawnPayloadBytes + 4096
	// maxSpawnEventLen bounds the optional synthetic X-GitHub-Event value
	// (real GitHub event names are short words).
	maxSpawnEventLen = 100
)

// SpawnAllowlist is POST /spawn's DENY-BY-DEFAULT authorization: which
// parent hooks may spawn which target hooks. It is runner-side OPERATOR
// config (the WEBHOOK_RUNNER_SPAWN_ALLOW env var, parsed by
// ParseSpawnAllow), deliberately NOT a hook.json field — the published hook
// schema is frozen and consumers must need zero new hook.json fields. A
// nil/empty allowlist refuses every spawn.
type SpawnAllowlist map[string]map[string]bool

// Allowed reports whether parent may spawn target. Nil-safe: a nil (or
// empty) allowlist denies everything.
func (a SpawnAllowlist) Allowed(parent, target string) bool {
	return a[parent][target]
}

// ParseSpawnAllow parses the WEBHOOK_RUNNER_SPAWN_ALLOW value:
// semicolon-separated entries, each "parent=target[,target...]", whitespace
// around every token trimmed, empty entries ignored — so a compose file can
// write it readably:
//
//	WEBHOOK_RUNNER_SPAWN_ALLOW=gha-coordinator=gha-runner,gha-runner-dind; other=target
//
// Duplicate parent entries merge (union of targets). "" parses to an empty
// allowlist (spawning disabled — the deny-by-default). A malformed entry —
// no "=", an empty parent, an empty target — is an ERROR the caller must
// fail startup on: a typo here would otherwise silently turn spawning off.
func ParseSpawnAllow(raw string) (SpawnAllowlist, error) {
	out := SpawnAllowlist{}
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parent, targets, ok := strings.Cut(entry, "=")
		parent = strings.TrimSpace(parent)
		if !ok || parent == "" {
			return nil, fmt.Errorf("spawn allowlist entry %q: want parent=target[,target...]", entry)
		}
		for _, target := range strings.Split(targets, ",") {
			target = strings.TrimSpace(target)
			if target == "" {
				return nil, fmt.Errorf("spawn allowlist entry %q: empty target hook id", entry)
			}
			if out[parent] == nil {
				out[parent] = map[string]bool{}
			}
			out[parent][target] = true
		}
	}
	return out, nil
}

// spawnRequest is the POST /spawn body. Payload is REQUIRED and must be a
// JSON object — it becomes each spawned run's payload file verbatim. Event,
// when set, becomes the spawned runs' X-GitHub-Event header value (hooks
// legitimately branch on the event header; e.g. the gha coordinator passes
// "workflow_job").
type spawnRequest struct {
	Hook    string          `json:"hook"`
	Count   int             `json:"count"`
	Payload json.RawMessage `json:"payload"`
	Event   string          `json:"event"`
}

// spawnResult is the POST /spawn response: the spawned run IDs in start
// order. Start is async dispatch, so /spawn returns the moment the runs are
// accepted — spawned runs may still be pending (queued on the target's
// concurrency group); the caller never waits on slots. On a mid-loop start
// failure the response (a 500) still lists every run that DID start, plus
// the error — an honest partial report, never a silent gap.
type spawnResult struct {
	RunIDs []string `json:"run_ids"`
	Error  string   `json:"error,omitempty"`
}

// handleSpawn implements POST /spawn on the state API. Validation is
// all-or-nothing BEFORE anything starts — bad request (400/413), parent run
// not active (409, the /wait rule), unknown target (404), caller not
// allowlisted (403), target effectively disabled by the operator kill
// switch (409) — and only then does the loop start count runs. Denials are
// loud: each records a spawn.denied activity event naming the parent and
// target (rejections are events on purpose — the dashboard must answer
// "did you receive anything?").
func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request, ns, runID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSpawnBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req spawnRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Hook) == "" {
		writeError(w, http.StatusBadRequest, "hook is required: the target hook id to spawn")
		return
	}
	if req.Count < minSpawnCount || req.Count > maxSpawnCount {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid count: must be %d..%d", minSpawnCount, maxSpawnCount))
		return
	}
	if len(req.Payload) > maxSpawnPayloadBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("payload too large: max %d bytes", maxSpawnPayloadBytes))
		return
	}
	if !isJSONObject(req.Payload) {
		writeError(w, http.StatusBadRequest, "payload is required and must be a JSON object")
		return
	}
	event := strings.TrimSpace(req.Event)
	if len(event) > maxSpawnEventLen {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("event too long: max %d characters", maxSpawnEventLen))
		return
	}

	if s.tracker == nil || s.registry == nil || s.runner == nil {
		writeError(w, http.StatusServiceUnavailable, "spawn not configured")
		return
	}
	// The parent must be live — the /wait and /title rule: a token always
	// names a real (namespace, id) pair, but the run can be over or evicted
	// from the bounded tracker, and a dead parent has nothing to attribute
	// its spawns to. The HookID check mirrors handleCancelRun's cross-hook
	// guard (belt-only — the HMAC already binds the pair). MANAGER
	// instances are valid parents too (first-class identity, not a run):
	// the liveness check goes to the supervisor — a stale instance's token
	// 409s here, the API-level single-instance guard.
	parent := s.tracker.Get(runID)
	if parent == nil || parent.HookID() != ns || parent.Status().Terminal() {
		if !s.managerCaller(ns, runID) {
			writeError(w, http.StatusConflict, "run is not active")
			return
		}
	}

	// Pre-validation of the TARGET, all-or-nothing, before anything starts.
	target, ok := s.registry.Get(req.Hook)
	if !ok {
		s.events.Record("spawn.denied",
			fmt.Sprintf("%s run %s: spawn target %q does not exist", ns, runID, req.Hook),
			map[string]string{"hook": ns, "run": runID, "target": req.Hook})
		writeError(w, http.StatusNotFound, "no such hook")
		return
	}
	// Deny-by-default: only an explicit WEBHOOK_RUNNER_SPAWN_ALLOW pair may
	// spawn. Checked after existence (the brief 404/403 split) — spawn
	// callers are operator-curated hooks, not an untrusted surface.
	if !s.spawnAllow.Allowed(ns, target.ID) {
		s.events.Record("spawn.denied",
			fmt.Sprintf("%s run %s: not allowlisted to spawn %s (WEBHOOK_RUNNER_SPAWN_ALLOW)", ns, runID, target.ID),
			map[string]string{"hook": ns, "run": runID, "target": target.ID})
		writeError(w, http.StatusForbidden, ns+" is not allowed to spawn "+target.ID)
		return
	}
	// The operator kill switch gates spawns exactly like deliveries and
	// scheduled fires — the same effective-disabled state handleTrigger and
	// buildScheduleFire consult — and the refusal is loud on the feed.
	if s.effectiveDisabled(target.ID) {
		s.events.Record("spawn.denied",
			fmt.Sprintf("%s run %s: spawn target %s is disabled by operator", ns, runID, target.ID),
			map[string]string{"hook": ns, "run": runID, "target": target.ID})
		writeError(w, http.StatusConflict, "target hook disabled by operator")
		return
	}

	// Dispatch: the scheduler-Fire shape — runner start with a background
	// context (never the request context: the caller's container outlives
	// this request, and the runs must outlive both) and a synthetic
	// payload/headers pair. skip_if is deliberately NOT evaluated (the
	// scheduled-fire rule: a spawn is the operator's own machinery asking,
	// not an unwanted delivery); the run title renders from the target's
	// run_title template against the spawned payload/headers, same as a
	// delivery would.
	payload := []byte(req.Payload)
	headers := spawnHeaders(ns, runID, event)
	title := target.RenderRunTitle(payload, headers)
	runIDs := make([]string, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		run, err := s.runner.StartSpawned(s.runRequestContext(), target, payload, headers, title, ns, runID)
		if err != nil {
			// Report honestly: the runs that DID start, and exactly which
			// start failed (the failed run itself exists in history with a
			// terminal error status). No rollback — started runs are real
			// work in flight, and the remaining starts are skipped (the
			// same failure would repeat, e.g. a draining runner).
			s.log.Error("spawned run failed to start",
				"parent_hook", ns, "parent_run", runID, "target", target.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, spawnResult{
				RunIDs: runIDs,
				Error: fmt.Sprintf("start %d of %d for hook %s failed (run %s): %v",
					i+1, req.Count, target.ID, run.ID(), err),
			})
			return
		}
		runIDs = append(runIDs, run.ID())
	}
	s.log.Info("runs spawned",
		"parent_hook", ns, "parent_run", runID, "target", target.ID, "count", len(runIDs))
	writeJSON(w, http.StatusOK, spawnResult{RunIDs: runIDs})
}

// isJSONObject reports whether raw is a JSON object. The enclosing
// json.Unmarshal already validated the document, so the first non-space
// byte settles it; nil (payload absent) and null both fail. Spawned
// payloads must be objects: hooks parse their payload file as a JSON
// document, and skip_if-style traversal, run titles, and the webhooks sdk
// all expect an object at the top.
func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

// spawnHeaders are the synthetic request headers a spawned run receives in
// HOOK_HEADERS_FILE (the scheduleHeaders convention in cli/serve.go): the
// parent's identity, plus — when the caller asked — the X-GitHub-Event
// value hooks branch on. Set through http.Header so key canonicalization
// matches a real delivery's parsed headers byte-for-byte.
func spawnHeaders(parentHookID, parentRunID, event string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Webhook-Runner-Spawned-By", parentHookID)
	h.Set("X-Webhook-Runner-Spawned-By-Run", parentRunID)
	if event != "" {
		h.Set("X-GitHub-Event", event)
	}
	return h
}
