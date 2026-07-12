package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// maxIncrBody caps the tiny JSON body of an increment request.
const maxIncrBody = 512

// maxLockBody caps the tiny JSON body of a lock acquire request.
const maxLockBody = 512

// Explicit lock TTL bounds (whole seconds). The TTL is a SECONDARY backstop
// — run-finish release is the primary mechanism — so the range only keeps
// callers from disabling the belt entirely (0/negative) or arming one so far
// out it stops being a backstop.
const (
	minLockTTLSeconds = 1
	maxLockTTLSeconds = 3600
)

// nsHandler is a state-port handler that has already had its caller's
// namespace and run identity resolved from the bearer token.
type nsHandler func(w http.ResponseWriter, r *http.Request, ns, runID string)

// withNamespace authenticates a state-port request by its bearer token and
// resolves the namespace — and the calling run's identity — from it, never
// from the URL: a hook can only ever touch its own data, and the lock verbs
// know which run is asking without any client-managed owner tokens.
func (s *Server) withNamespace(next nsHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.kv == nil {
			writeError(w, http.StatusServiceUnavailable, "state store not configured")
			return
		}
		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		ns, runID, ok := s.kv.VerifyToken(tok)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r, ns, runID)
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func (s *Server) handleKVGet(w http.ResponseWriter, r *http.Request, ns, _ string) {
	v, ok := s.kv.Get(ns, r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(v)
}

func (s *Server) handleKVPut(w http.ResponseWriter, r *http.Request, ns, _ string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(s.kv.MaxValueBytes())+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "value too large")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	ttl, err := parseTTL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.kv.Set(ns, r.PathValue("key"), body, ttl); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKVDelete(w http.ResponseWriter, r *http.Request, ns, _ string) {
	if err := s.kv.Delete(ns, r.PathValue("key")); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKVList(w http.ResponseWriter, _ *http.Request, ns, _ string) {
	writeJSON(w, http.StatusOK, map[string][]string{"keys": s.kv.List(ns)})
}

func (s *Server) handleKVIncr(w http.ResponseWriter, r *http.Request, ns, _ string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIncrBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	delta := int64(1)
	if len(bytes.TrimSpace(body)) > 0 {
		var req struct {
			Delta *int64 `json:"delta"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if req.Delta != nil {
			delta = *req.Delta
		}
	}
	ttl, err := parseTTL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.kv.Incr(ns, r.PathValue("key"), delta, ttl)
	if err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"value": n})
}

// lockRetryInterval is the poll cadence of a blocking acquire. Fairness is
// deliberately best-effort (no FIFO queue): blocked contenders — and any
// fresh caller — race on each poll, which keeps the lock table free of
// waiter state and lets a steal trivially beat every waiter. The cadence
// bounds handoff latency at ~250ms, plenty for lock-guarded hook work.
const lockRetryInterval = 250 * time.Millisecond

// lockConflict is the 409 body for a contended acquire (immediate or after
// a blocking acquire gave up): the error plus WHO holds the lock, so
// contention is actionable — display it, keep waiting, or steal.
type lockConflict struct {
	Error  string       `json:"error"`
	HeldBy *kv.LockInfo `json:"held_by,omitempty"`
}

// lockRequest is the acquire/steal request body. TTLSeconds bounds the
// backstop expiry; Block turns a contended acquire into a held request
// (see handleKVAcquire); BlockTimeoutSeconds caps the hold (default and max
// maxWaitSeconds, same 10-minute cap as /wait — loop for longer).
type lockRequest struct {
	TTLSeconds          *int `json:"ttl_seconds"`
	Block               bool `json:"block"`
	BlockTimeoutSeconds *int `json:"block_timeout_seconds"`
}

func parseLockRequest(w http.ResponseWriter, r *http.Request) (req lockRequest, ttl time.Duration, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLockBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return req, 0, false
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return req, 0, false
		}
		if req.TTLSeconds != nil {
			if *req.TTLSeconds < minLockTTLSeconds || *req.TTLSeconds > maxLockTTLSeconds {
				writeError(w, http.StatusBadRequest,
					"invalid ttl_seconds: must be "+strconv.Itoa(minLockTTLSeconds)+".."+strconv.Itoa(maxLockTTLSeconds))
				return req, 0, false
			}
			ttl = time.Duration(*req.TTLSeconds) * time.Second
		}
		if req.BlockTimeoutSeconds != nil {
			if *req.BlockTimeoutSeconds < minWaitSeconds || *req.BlockTimeoutSeconds > maxWaitSeconds {
				writeError(w, http.StatusBadRequest,
					"invalid block_timeout_seconds: must be "+strconv.Itoa(minWaitSeconds)+".."+strconv.Itoa(maxWaitSeconds))
				return req, 0, false
			}
		}
	}
	return req, ttl, true
}

// handleKVAcquire is the cooperative lock take: atomic under the store's
// lock-table mutex, owned by the CALLING RUN (the identity in the bearer
// token — there are no client-managed owner tokens). 200 when this run took
// or already held the lock (idempotent re-acquire); 409 when another live
// run holds it (nothing is mutated — in particular the holder's backstop
// expiry is never restamped by a contender), with the HOLDER named in the
// body's held_by. Optional body {"ttl_seconds": N} sets the secondary
// backstop expiry; absent, the store default applies (run-finish release is
// the primary mechanism either way).
//
// With {"block": true} a contended acquire is HELD instead of refused: the
// server retries every lockRetryInterval until the lock is taken (200), the
// block timeout passes (409 + held_by, default/max the /wait 10-minute
// cap via "block_timeout_seconds"), or the run ends underneath it. While
// blocked, the run is visibly waiting on the named holder (RunState
// waiting_on, rendered by the dashboard) and the hold counts as ACTIVITY
// for the idle `timeout` — exactly like a declared /wait.
func (s *Server) handleKVAcquire(w http.ResponseWriter, r *http.Request, ns, runID string) {
	req, ttl, ok := parseLockRequest(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")

	info, err := s.kv.AcquireLock(ns, key, runID, ttl)
	if err == nil {
		writeJSON(w, http.StatusOK, info)
		return
	}
	if !errors.Is(err, kv.ErrLockHeld) {
		s.writeKVError(w, ns, err)
		return
	}
	if !req.Block {
		writeJSON(w, http.StatusConflict, lockConflict{Error: err.Error(), HeldBy: &info})
		return
	}
	s.blockOnLock(w, r, ns, key, runID, ttl, req, info)
}

// blockOnLock is the held half of a blocking acquire: the first attempt was
// contended (holder describes it); keep retrying until acquired, timed out,
// or the run is gone.
func (s *Server) blockOnLock(w http.ResponseWriter, r *http.Request, ns, key, runID string, ttl time.Duration, req lockRequest, holder kv.LockInfo) {
	if s.tracker == nil {
		writeError(w, http.StatusServiceUnavailable, "run tracking not configured")
		return
	}
	run := s.tracker.Get(runID)
	if run == nil || run.HookID() != ns || run.Status().Terminal() {
		// Same rule as /wait: nothing to attribute the hold to — refuse
		// rather than blocking a connection nobody owns.
		writeError(w, http.StatusConflict, "run is not active")
		return
	}

	blockTimeout := time.Duration(maxWaitSeconds) * time.Second
	if req.BlockTimeoutSeconds != nil {
		blockTimeout = time.Duration(*req.BlockTimeoutSeconds) * time.Second
	}
	deadline := time.Now().Add(blockTimeout)

	seq := run.SetWaitingOn(waitingOnLock(key, deadline, holder))
	defer func() { run.ClearWaitingOn(seq) }()
	run.TouchActivity()
	s.events.Record("lock.waiting",
		fmt.Sprintf("%s run %s waiting on lock %q held by run %s", ns, runID, key, holder.RunID),
		map[string]string{"hook": ns, "run": runID})

	// Retry + watchdog-touch cadence: the touch interval a /wait would use,
	// tightened to the lock retry interval so handoff stays snappy.
	interval := s.waitTouchInterval(ns)
	if interval > lockRetryInterval {
		interval = lockRetryInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	giveUp := time.NewTimer(blockTimeout)
	defer giveUp.Stop()

	for {
		select {
		case <-tick.C:
			run.TouchActivity()
			info, err := s.kv.AcquireLock(ns, key, runID, ttl)
			if err == nil {
				run.TouchActivity()
				writeJSON(w, http.StatusOK, info)
				return
			}
			if !errors.Is(err, kv.ErrLockHeld) {
				s.writeKVError(w, ns, err)
				return
			}
			if info.RunID != holder.RunID {
				// The lock changed hands (release+re-acquire, or a steal)
				// and we lost the race: re-stamp who we're waiting on.
				holder = info
				seq = run.SetWaitingOn(waitingOnLock(key, deadline, holder))
			}
		case <-giveUp.C:
			writeJSON(w, http.StatusConflict, lockConflict{
				Error:  fmt.Sprintf("lock still held after %s", blockTimeout),
				HeldBy: &holder,
			})
			return
		case <-run.Done():
			writeJSON(w, http.StatusConflict, lockConflict{Error: "interrupted: run finished", HeldBy: &holder})
			return
		case <-run.Cancelled():
			writeJSON(w, http.StatusConflict, lockConflict{Error: "interrupted: run cancelled", HeldBy: &holder})
			return
		case <-r.Context().Done():
			// Client hung up; the deferred ClearWaitingOn tidies the state.
			return
		}
	}
}

func waitingOnLock(key string, deadline time.Time, holder kv.LockInfo) runs.WaitingOn {
	return runs.WaitingOn{
		Kind:         runs.WaitingOnLock,
		Key:          key,
		Until:        deadline.UTC(),
		HolderRunID:  holder.RunID,
		HolderHookID: holder.HookID,
	}
}

// stealResult is the POST /kv/{key}/steal response: the lock's new state
// (owned by the caller) plus, when another run was displaced, who lost it.
// stolen_from absent means the lock was free — a steal of an uncontended
// lock is exactly an acquire.
type stealResult struct {
	kv.LockInfo
	StolenFrom *stolenParty `json:"stolen_from,omitempty"`
}

type stolenParty struct {
	RunID  string `json:"run_id"`
	HookID string `json:"hook_id"`
}

// handleKVSteal destructively takes the lock: transfer it to the calling
// run AND cancel the displaced holder. A separate route (not an acquire
// flag) on purpose: stealing kills another run, so the intent must be
// unmistakable in the request line, the mux table, and the logs. The
// transfer is atomic in the lock table, so it cannot race the holder's
// finish-seam release (a holder that finished first just makes this a plain
// acquire — no error); the cancel then rides the existing cancel path
// (docker-kill-by-name), and the displaced run's OTHER locks release
// normally when it terminates — the stolen one is already owned by the
// thief, so the finish seam skips it. Namespace scoping means a run can
// only ever steal from — and thus cancel — runs of its OWN hook. Blocked
// waiters are NOT inherited by the thief; they keep polling and now contend
// against the new holder.
func (s *Server) handleKVSteal(w http.ResponseWriter, r *http.Request, ns, runID string) {
	req, ttl, ok := parseLockRequest(w, r)
	if !ok {
		return
	}
	if req.Block {
		// Steal never needs to block — it takes the lock unconditionally.
		writeError(w, http.StatusBadRequest, "block is not valid on steal")
		return
	}
	key := r.PathValue("key")

	info, displaced, err := s.kv.StealLock(ns, key, runID, ttl)
	if err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	res := stealResult{LockInfo: info}
	if displaced.RunID != "" {
		res.StolenFrom = &stolenParty{RunID: displaced.RunID, HookID: displaced.HookID}
		note := "holder already gone"
		if s.tracker != nil {
			if victim := s.tracker.Get(displaced.RunID); victim != nil &&
				victim.HookID() == ns && !victim.Status().Terminal() {
				victim.RequestCancelWithReason(
					fmt.Sprintf("cancelled: lock %q stolen by run %s", key, runID))
				note = "holder cancelled"
			}
		}
		s.log.Info("lock stolen", "hook", ns, "run", runID, "key", key, "from", displaced.RunID)
		s.events.Record("lock.stolen",
			fmt.Sprintf("%s run %s stole lock %q from run %s (%s)", ns, runID, key, displaced.RunID, note),
			map[string]string{"hook": ns, "run": runID})
	}
	writeJSON(w, http.StatusOK, res)
}

// handleKVRelease frees a lock the calling run holds (early release — the
// runner also frees everything a run still holds when it finishes). The
// owner check is server-side, from the token's run identity: 204 released,
// 404 not held (absent or already expired), 409 held by another run. Any
// request body is ignored — there is nothing a caller could need to say.
func (s *Server) handleKVRelease(w http.ResponseWriter, r *http.Request, ns, runID string) {
	if err := s.kv.ReleaseLock(ns, r.PathValue("key"), runID); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseTTL reads the optional TTL from the X-KV-TTL header or the ?ttl= query
// parameter, in whole seconds. Absent means no expiry (0). Negative or
// non-numeric is a client error.
func parseTTL(r *http.Request) (time.Duration, error) {
	raw := r.Header.Get("X-KV-TTL")
	if raw == "" {
		raw = r.URL.Query().Get("ttl")
	}
	if raw == "" {
		return 0, nil
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("invalid ttl: must be whole seconds")
	}
	if secs < 0 {
		return 0, errors.New("invalid ttl: must not be negative")
	}
	return time.Duration(secs) * time.Second, nil
}

// writeKVError maps the store's typed errors onto HTTP status codes. The
// typed errors are the caller's fault; anything else is an internal store
// failure — in practice a failed disk persist, after which the store has
// already rolled the in-memory mutation back. Those must be loud end-to-end:
// the hook gets a 5xx carrying the reason (its write did NOT happen), the
// server log gets the error, and the activity feed gets a kv.write_failed
// event so the dashboard can answer "are state writes failing?".
func (s *Server) writeKVError(w http.ResponseWriter, ns string, err error) {
	switch {
	case errors.Is(err, kv.ErrValueTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, kv.ErrTooManyKeys), errors.Is(err, kv.ErrTooManyNS):
		writeError(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, kv.ErrNotInteger):
		writeError(w, http.StatusConflict, err.Error())
	// Lock contention/ownership outcomes are normal control flow for the
	// caller (409/404), never write failures — no log, no event.
	case errors.Is(err, kv.ErrLockHeld):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, kv.ErrLockNotHeld):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kv.ErrBadNamespace):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("kv: state write failed", "ns", ns, "err", err)
		s.events.Record("kv.write_failed", ns+": state write failed (rolled back): "+err.Error(),
			map[string]string{"hook": ns})
		writeError(w, http.StatusInternalServerError, "state store error: "+err.Error())
	}
}
