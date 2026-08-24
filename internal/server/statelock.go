package server

import (
	"bytes"
	"context"
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

	info, err := s.takeLock(r.Context(), ns, key, runID, ttl, req.Pinned)
	if err == nil {
		writeJSON(w, http.StatusOK, info)
		return
	}
	if errors.Is(err, kv.ErrLockExpired) {
		// TTL enforcement did not finish: the holder is still dying, or it is
		// an entity this server cannot kill. The lock is NOT handed over —
		// the whole point — so the caller gets a named 409 either way.
		writeJSON(w, http.StatusConflict, lockConflict{Error: err.Error(), HeldBy: &info})
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

// lockKillTimeout bounds ONE TTL enforcement: how long an acquire waits for
// the over-budget holder it just killed to actually reach a terminal state
// (docker kill + the finish seam — normally well under a second). Past it
// the acquire is refused rather than granted: a mutex is only ever handed
// over once the previous holder is CERTAIN to be dead.
// (a var only so tests can shorten it; nothing reassigns it in production)
var lockKillTimeout = 30 * time.Second

// takeLock is THE acquire seam — the plain compare-and-set plus TTL
// ENFORCEMENT. Every acquire path goes through it (the immediate take and
// both blocking retry loops), so none of them can disagree about what an
// expired hold means.
func (s *Server) takeLock(ctx context.Context, ns, key, runID string, ttl time.Duration, pinned bool) (kv.LockInfo, error) {
	info, err := s.acquireLock(ns, key, runID, ttl, pinned)
	if !errors.Is(err, kv.ErrLockExpired) {
		return info, err
	}
	return s.enforceLockTTL(ctx, ns, key, runID, ttl, pinned, info)
}

// enforceLockTTL makes the TTL mean something (operator ruling — see
// kv/lock.go's header): the store never frees an expired lock on its own,
// because a mutex that lets go under a live holder is not a mutex. So when a
// contender meets an over-budget hold, the runner KILLS the holder, waits
// until that run is certainly dead, and only then takes the lock the finish
// seam released. The sequence is the whole point — kill, confirm, then free
// — and every branch that cannot complete it refuses the lock instead:
//   - holder already gone (the release-path bug the backstop exists for):
//     reap its shell and take the lock. Nothing to kill, nothing to wait for;
//   - holder is a live run: cancel it once, then poll until terminal;
//   - holder is a MANAGER INSTANCE: refuse. Manager instances are supervised
//     and restart-on-exit, so "kill it" is not this endpoint's call to make;
//   - no run tracker wired: refuse. Liveness is unknowable, and guessing
//     "dead" is exactly the two-holders bug.
func (s *Server) enforceLockTTL(ctx context.Context, ns, key, runID string, ttl time.Duration, pinned bool, holder kv.LockInfo) (kv.LockInfo, error) {
	if s.managerCaller(ns, holder.RunID) {
		return holder, fmt.Errorf("%w: held by the live manager instance %s, which this endpoint may not kill", kv.ErrLockExpired, holder.RunID)
	}
	if s.tracker == nil {
		return holder, fmt.Errorf("%w: run tracking not configured, so the holder cannot be confirmed dead", kv.ErrLockExpired)
	}

	deadline := time.Now().Add(lockKillTimeout)
	killed := false
	for {
		victim := s.tracker.Get(holder.RunID)
		if victim == nil || victim.HookID() != ns || victim.Status().Terminal() {
			// CERTAIN to be dead. Its finish seam normally freed the lock
			// already; the reap covers the case that seam never ran (a no-op
			// if the entry changed hands meanwhile — ReapExpiredLock refuses
			// anything but this exact expired holder).
			if s.kv.ReapExpiredLock(ns, key, holder.RunID) {
				s.log.Info("expired lock reaped", "hook", ns, "key", key, "holder", holder.RunID, "taker", runID)
				s.events.Record("lock.ttl_reaped",
					fmt.Sprintf("%s: lock %q freed for run %s — its holder (run %s) was over its TTL and already gone", ns, key, runID, holder.RunID),
					map[string]string{"hook": ns, "run": runID})
			}
			return s.acquireLock(ns, key, runID, ttl, pinned)
		}
		if !killed {
			victim.RequestCancelWithReason(fmt.Sprintf("cancelled: lock %q held past its TTL (since %s) — killed so the lock can be handed to run %s",
				key, holder.AcquiredAt.UTC().Format(time.RFC3339), runID))
			killed = true
			s.log.Info("killing lock holder past its TTL", "hook", ns, "key", key, "holder", holder.RunID, "taker", runID)
			s.events.Record("lock.ttl_enforced",
				fmt.Sprintf("%s: run %s held lock %q past its TTL — cancelling it so run %s can take the lock", ns, holder.RunID, key, runID),
				map[string]string{"hook": ns, "run": holder.RunID})
		}
		if time.Now().After(deadline) {
			return holder, fmt.Errorf("%w: holder run %s did not terminate within %s of being cancelled — the lock stays its", kv.ErrLockExpired, holder.RunID, lockKillTimeout)
		}
		select {
		case <-time.After(lockRetryInterval):
		case <-ctx.Done():
			return holder, ctx.Err()
		}
	}
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

// lockWaiter is blockOnLock's ONLY seam between a RUN and a MANAGER
// INSTANCE blocking on a contended lock. There is one blocking algorithm —
// poll, touch, take, or give up — because there is no meaningful difference
// between the two callers: both are just "something webhook-runner is
// running that wants a lock another live thing holds". Before this, the two
// were separate hand-written retry loops (blockOnLock and
// managerBlockOnLock) that had already drifted in small ways no test caught
// — the manager path had zero coverage.
type lockWaiter interface {
	// label names the caller in events and messages: "run" or "instance".
	label() string
	// touch credits one poll's worth of activity toward the caller's idle
	// watchdog and reports whether it is still the entity this wait should
	// keep running for. A run always answers true — its liveness is tracked
	// separately via done()/cancelled() — while a manager instance's
	// liveness IS this check (TouchInstance), since it has no other signal.
	touch() bool
	// setWaitingOn/clearWaitingOn mirror the hold onto a run row's dashboard
	// badge. A manager instance has no row; both are no-ops there.
	setWaitingOn(runs.WaitingOn) uint64
	clearWaitingOn(seq uint64)
	// done/cancelled fire when the caller is torn down out from under the
	// wait. A manager instance has neither concept (its termination IS
	// touch() going false), so both return nil — and a select on a nil
	// channel simply never fires, which is exactly "no such signal", not a
	// workaround.
	done() <-chan struct{}
	cancelled() <-chan struct{}
}

type runLockWaiter struct{ run *runs.Run }

func (w runLockWaiter) label() string { return "run" }
func (w runLockWaiter) touch() bool {
	w.run.TouchActivity()
	return true
}
func (w runLockWaiter) setWaitingOn(wo runs.WaitingOn) uint64 { return w.run.SetWaitingOn(wo) }
func (w runLockWaiter) clearWaitingOn(seq uint64)             { w.run.ClearWaitingOn(seq) }
func (w runLockWaiter) done() <-chan struct{}                 { return w.run.Done() }
func (w runLockWaiter) cancelled() <-chan struct{}            { return w.run.Cancelled() }

type managerLockWaiter struct {
	s  *Server
	ns string
	id string
}

func (w managerLockWaiter) label() string                      { return "instance" }
func (w managerLockWaiter) touch() bool                        { return w.s.managers.TouchInstance(w.ns, w.id) }
func (w managerLockWaiter) setWaitingOn(runs.WaitingOn) uint64 { return 0 }
func (w managerLockWaiter) clearWaitingOn(uint64)              {}
func (w managerLockWaiter) done() <-chan struct{}              { return nil }
func (w managerLockWaiter) cancelled() <-chan struct{}         { return nil }

// blockOnLock is the held half of a blocking acquire: the first attempt was
// contended (holder describes it); determine who is asking — a run or a
// manager instance — and keep retrying on their behalf until acquired,
// timed out, or they are gone.
//
// The run tracker is consulted first, but its ABSENCE no longer refuses a
// manager instance's block: a prior version 503'd here whenever s.tracker
// was nil, before ever checking whether the caller was a manager — wrong,
// since a manager instance's liveness (TouchInstance) has nothing to do
// with the run tracker at all.
func (s *Server) blockOnLock(w http.ResponseWriter, r *http.Request, ns, key, id string, ttl time.Duration, req lockRequest, holder kv.LockInfo) {
	var waiter lockWaiter
	if s.tracker != nil {
		if run := s.tracker.Get(id); run != nil && run.HookID() == ns && !run.Status().Terminal() {
			waiter = runLockWaiter{run: run}
		}
	}
	if waiter == nil {
		if !s.managerCaller(ns, id) {
			// Same rule as /wait: nothing to attribute the hold to — refuse
			// rather than blocking a connection nobody owns.
			writeError(w, http.StatusConflict, "run is not active")
			return
		}
		waiter = managerLockWaiter{s: s, ns: ns, id: id}
	}

	blockTimeout := time.Duration(maxWaitSeconds) * time.Second
	if req.BlockTimeoutSeconds != nil {
		blockTimeout = time.Duration(*req.BlockTimeoutSeconds) * time.Second
	}
	deadline := time.Now().Add(blockTimeout)

	seq := waiter.setWaitingOn(waitingOnLock(key, deadline, holder))
	defer func() { waiter.clearWaitingOn(seq) }()
	waiter.touch()
	s.events.Record("lock.waiting",
		fmt.Sprintf("%s %s %s waiting on lock %q held by run %s", ns, waiter.label(), id, key, holder.RunID),
		map[string]string{"hook": ns, "run": id})

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
			if !waiter.touch() {
				writeError(w, http.StatusConflict, fmt.Sprintf("not the current %s", waiter.label()))
				return
			}
			info, err := s.takeLock(r.Context(), ns, key, id, ttl, req.Pinned)
			if err == nil {
				waiter.touch()
				writeJSON(w, http.StatusOK, info)
				return
			}
			if errors.Is(err, context.Canceled) {
				return // client hung up mid-enforcement; the deferred cleanup tidies up
			}
			// An expired hold whose enforcement did not complete keeps this
			// waiter waiting — the retry re-enters enforcement, and the hold
			// is never handed over on a guess.
			if !errors.Is(err, kv.ErrLockHeld) && !errors.Is(err, kv.ErrLockExpired) {
				s.writeKVError(w, ns, err)
				return
			}
			if info.RunID != holder.RunID {
				// The lock changed hands (release+re-acquire, or a steal)
				// and we lost the race: re-stamp who we're waiting on (a
				// no-op for a manager waiter, which has no dashboard row).
				seq = waiter.setWaitingOn(waitingOnLock(key, deadline, info))
			}
			holder = info
		case <-giveUp.C:
			writeJSON(w, http.StatusConflict, lockConflict{
				Error:  fmt.Sprintf("lock still held after %s", blockTimeout),
				HeldBy: &holder,
			})
			return
		case <-waiter.done():
			writeJSON(w, http.StatusConflict, lockConflict{Error: "interrupted: run finished", HeldBy: &holder})
			return
		case <-waiter.cancelled():
			writeJSON(w, http.StatusConflict, lockConflict{Error: "interrupted: run cancelled", HeldBy: &holder})
			return
		case <-r.Context().Done():
			// Client hung up; the deferred clearWaitingOn tidies the state.
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
