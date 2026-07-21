package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// lockRequest is the acquire/steal request body. TTLSeconds bounds the
// backstop expiry; Block turns a contended acquire into a held request
// (see handleKVAcquire); BlockTimeoutSeconds caps the hold (default and max
// maxWaitSeconds, same 10-minute cap as /wait — loop for longer); Pinned
// (acquire only) applies the steal-protection pin ATOMICALLY with the take
// — take-and-pin in one compare-and-set, so no stealer can slip between an
// acquire and a separate POST /kv/{key}/pin.
type lockRequest struct {
	TTLSeconds          *int `json:"ttl_seconds"`
	Block               bool `json:"block"`
	BlockTimeoutSeconds *int `json:"block_timeout_seconds"`
	Pinned              bool `json:"pinned"`
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

	info, err := s.acquireLock(ns, key, runID, ttl, req.Pinned)
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

// acquireLock dispatches to the plain or the atomic take-and-pin acquire.
// One seam so the immediate path and blockOnLock's retry loop can never
// disagree about the requested pin.
func (s *Server) acquireLock(ns, key, runID string, ttl time.Duration, pinned bool) (kv.LockInfo, error) {
	if pinned {
		return s.kv.AcquireLockPinned(ns, key, runID, ttl)
	}
	return s.kv.AcquireLock(ns, key, runID, ttl)
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
		// Manager instances are not runs (first-class identity): they get
		// the same blocking retry loop, feeding the INSTANCE watchdog and
		// ending when the instance stops being current — minus the
		// run-row waiting_on badge (there is no run row).
		if s.managerCaller(ns, runID) {
			s.managerBlockOnLock(w, r, ns, key, runID, ttl, req, holder)
			return
		}
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
			info, err := s.acquireLock(ns, key, runID, ttl, req.Pinned)
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

// managerBlockOnLock is blockOnLock for a manager instance: the same flat
// retry loop against the same acquire (pin request included), feeding the
// instance's idle watchdog, aborting when the instance stops being current.
func (s *Server) managerBlockOnLock(w http.ResponseWriter, r *http.Request, ns, key, instanceID string, ttl time.Duration, req lockRequest, holder kv.LockInfo) {
	blockTimeout := time.Duration(maxWaitSeconds) * time.Second
	if req.BlockTimeoutSeconds != nil {
		blockTimeout = time.Duration(*req.BlockTimeoutSeconds) * time.Second
	}
	s.events.Record("lock.waiting",
		fmt.Sprintf("%s instance %s waiting on lock %q held by run %s", ns, instanceID, key, holder.RunID),
		map[string]string{"hook": ns})

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
			if !s.managers.TouchInstance(ns, instanceID) {
				writeError(w, http.StatusConflict, "not the current manager instance")
				return
			}
			info, err := s.acquireLock(ns, key, instanceID, ttl, req.Pinned)
			if err == nil {
				s.managers.TouchInstance(ns, instanceID)
				writeJSON(w, http.StatusOK, info)
				return
			}
			if !errors.Is(err, kv.ErrLockHeld) {
				s.writeKVError(w, ns, err)
				return
			}
			holder = info
		case <-giveUp.C:
			writeJSON(w, http.StatusConflict, lockConflict{
				Error:  fmt.Sprintf("lock still held after %s", blockTimeout),
				HeldBy: &holder,
			})
			return
		case <-r.Context().Done():
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
	if errors.Is(err, kv.ErrLockPinned) {
		// The holder marked its critical section non-displaceable. Refuse
		// LOUDLY (contention is never anonymous — and neither is protection):
		// the 409 names the pinned holder, and the feed records the refused
		// displacement so a long-pinned section is visible to the operator.
		// The caller's correct fallback is a blocking acquire, which wins the
		// moment the pin lifts or the holder finishes.
		s.events.Record("lock.steal_refused",
			fmt.Sprintf("%s run %s: steal of lock %q refused — pinned by run %s", ns, runID, key, info.RunID),
			map[string]string{"hook": ns, "run": runID})
		writeJSON(w, http.StatusConflict, lockConflict{Error: err.Error(), HeldBy: &info})
		return
	}
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

// handleKVPin marks a lock the calling run holds non-stealable —
// POST /kv/{key}/pin, no body. Owner-only, the release auth rule: 204
// pinned (idempotent), 404 not held (absent/expired), 409 held by another
// run. A separate route rather than an acquire flag so the mode change is
// unmistakable in request lines and logs; the
// atomic take-and-pin lives on acquire as {"pinned": true} for callers that
// need zero window between take and protection.
func (s *Server) handleKVPin(w http.ResponseWriter, r *http.Request, ns, runID string) {
	if err := s.kv.PinLock(ns, r.PathValue("key"), runID); err != nil {
		s.writeKVError(w, ns, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleKVUnpin restores normal steal semantics on a lock the calling run
// holds — POST /kv/{key}/unpin, no body. Same auth and idempotence as pin.
func (s *Server) handleKVUnpin(w http.ResponseWriter, r *http.Request, ns, runID string) {
	if err := s.kv.UnpinLock(ns, r.PathValue("key"), runID); err != nil {
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
