package kv

import (
	"errors"
	"time"
)

// Cooperative per-key locks, owned by RUN INSTANCES.
//
// The operator's locking model: tracking which hook run holds a lock — and
// releasing everything it holds when that run terminates for any reason — is
// the PRIMARY liveness mechanism; a TTL is only a secondary backstop. So a
// lock here is bound to the run identity carried by the caller's state token
// (see Token/VerifyToken), acquire/release are atomic under one mutex (the
// compare-and-set/compare-and-delete a client cannot build from GET+PUT),
// and the runner frees a run's remaining locks at the tracker's OnFinish
// seam — which fires exactly once on EVERY terminal path (success, error,
// timeout kill, cancel), wired in cli/serve.go.
//
// The table is deliberately IN-MEMORY ONLY, unlike the persisted KV entries:
// a lock's lifecycle is bounded by its holding run, and no run survives a
// server restart (in-flight runs die with the server by design), so a
// restart correctly starts lock-free. Locks therefore never appear in the
// namespace files, the admin KV views, or GET/PUT/DELETE — they are a
// separate facility that happens to share the namespace and its
// namespace-count bound.
//
// The TTL backstop: every lock gets an expiry — DefaultLockTTL when the
// caller doesn't choose one — sized far beyond any legitimate hold, because
// it exists only to unwedge a lock if the finish-seam release were ever
// broken by a bug.
//
// EXPIRY NEVER FREES A LOCK BY ITSELF (operator ruling: "never have a TTL on
// a mutex, that doesn't make sense. Or, if you want to have a mutex TTL, you
// need to force kill the thing that's holding it when the time is up. ONCE
// THAT FORCE KILL COMPLETES AND THAT JOB IS CERTAIN TO BE DEAD, then the
// mutex would be freed automatically due to the ending job"). A lock that
// silently frees itself under a LIVE holder is not a mutex — it is a hint,
// and two runs then believe they hold it. So an expired entry stays the
// holder's: a contender gets ErrLockExpired naming it, and the TTL is
// ENFORCED by the only layer that knows run liveness (the server): kill the
// holder, wait for it to actually reach a terminal state, and let the
// finish-seam release free the lock. ReapExpiredLock is the narrow second
// half for a holder already CONFIRMED gone.
//
// Contended acquires mutate NOTHING — in particular they never restamp the
// holder's expiry, so contenders can't keep a dead run's lock alive — and
// they name the holder (LockInfo alongside ErrLockHeld/ErrLockExpired), so
// contention is never anonymous. StealLock is the destructive counterpart:
// it transfers a held lock to the caller atomically (namespace-scoped, so a
// run can only ever displace a run of its OWN hook); cancelling the
// displaced run is the server's job.
var (
	// ErrLockHeld: the lock is held by a different live run (acquire), or the
	// caller tried to release a lock a different live run holds (release).
	ErrLockHeld = errors.New("kv: lock held by another run")
	// ErrLockNotHeld: nothing (live) to release.
	ErrLockNotHeld = errors.New("kv: lock not held")
	// ErrLockExpired: the holder's TTL backstop passed, but the lock is
	// STILL the holder's — expiry alone frees nothing. The contender's info
	// names the holder so the caller can ENFORCE the TTL: kill that run,
	// confirm it is dead, and take the lock the finish seam then frees.
	// Distinct from ErrLockHeld on purpose — one means "wait or steal", the
	// other means "this hold is over its budget, enforce it".
	ErrLockExpired = errors.New("kv: lock TTL expired — the holder must be killed before the lock frees")
	// ErrLockPinned: a steal was refused because the holder PINNED the lock
	// (marked its critical section non-displaceable). The refusal mutates
	// nothing; the caller can fall back to a blocking acquire, which wins
	// the moment the pin lifts or the holder finishes (finish-seam release).
	ErrLockPinned = errors.New("kv: lock is pinned by its holder (steal refused)")
)

// DefaultLockTTL is the backstop expiry applied when an acquire names no
// ttl_seconds. 15 minutes is far beyond any legitimate hold (runs default to
// a 5m idle timeout; lock-guarded passes take seconds) — deliberately so,
// because run-finish release is the primary mechanism and the TTL only
// covers a release-path bug.
const DefaultLockTTL = 15 * time.Minute

type lockEntry struct {
	runID      string
	acquiredAt time.Time
	expiresAt  time.Time
	// pinned marks the holder's critical section non-displaceable: a steal
	// of a live pinned lock is refused with ErrLockPinned. Pin protects
	// against STEAL only — never against the holder's own release, the
	// finish-seam release, or the TTL backstop expiry (expired() ignores
	// it), so a pin can never outlive its run.
	pinned bool
}

func (l lockEntry) expired(now time.Time) bool {
	return !l.expiresAt.After(now)
}

// LockInfo describes a lock's holder: what a successful acquire/steal
// reports back to the caller, and — on a contended acquire — WHO currently
// holds the lock (so a contender can display, wait on, or steal from a
// named holder rather than an anonymous 409). HookID is the lock's
// namespace, which is the holding run's hook. Pinned reports the holder's
// steal-protection mark, so a refused stealer sees not just who holds but
// that displacement was deliberately forbidden.
type LockInfo struct {
	RunID      string    `json:"run_id"`
	HookID     string    `json:"hook_id"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Pinned     bool      `json:"pinned,omitempty"`
}

func (e lockEntry) info(ns string) LockInfo {
	return LockInfo{RunID: e.runID, HookID: ns, AcquiredAt: e.acquiredAt, ExpiresAt: e.expiresAt, Pinned: e.pinned}
}

// AcquireLock atomically takes the cooperative lock at key in ns for runID.
// A free (absent or expired) lock is taken with expiry now+ttl (DefaultLockTTL
// when ttl <= 0). A lock the SAME run already holds is re-acquired
// idempotently — its backstop expiry refreshed, its original acquiredAt kept
// (only the live owner can do this, so it can never prolong a dead run's
// lock). A lock held by another live run returns ErrLockHeld — mutating
// nothing — together with the HOLDER's LockInfo, so contention is never
// anonymous. The same namespace cap as the entry store applies.
func (s *Store) AcquireLock(ns, key, runID string, ttl time.Duration) (LockInfo, error) {
	if !validNamespace(ns) {
		return LockInfo{}, ErrBadNamespace
	}
	if runID == "" {
		// Unreachable through the state API (VerifyToken rejects tokens
		// without a run identity); refuse rather than mint anonymous locks.
		return LockInfo{}, errors.New("kv: lock requires a run identity")
	}
	ttl = lockTTL(ttl)

	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	info, _, err := s.takeLockLocked(ns, key, runID, ttl, false, false)
	return info, err
}

// AcquireLockPinned is AcquireLock with the pin applied ATOMICALLY in the
// same compare-and-set — take-and-pin in one step, so no stealer can slip
// between an acquire and a separate PinLock call. Same contention semantics
// as AcquireLock.
func (s *Store) AcquireLockPinned(ns, key, runID string, ttl time.Duration) (LockInfo, error) {
	if !validNamespace(ns) {
		return LockInfo{}, ErrBadNamespace
	}
	if runID == "" {
		return LockInfo{}, errors.New("kv: lock requires a run identity")
	}
	ttl = lockTTL(ttl)

	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	info, _, err := s.takeLockLocked(ns, key, runID, ttl, false, true)
	return info, err
}

// StealLock atomically takes the lock at key in ns for runID EVEN IF another
// live run holds it, returning the displaced holder's info (displaced.RunID
// == "" when the lock was free or already ours — a steal of an uncontended
// lock is exactly an acquire). The transfer happens under the lock-table
// mutex, so it cannot race the displaced run's finish-seam release: after a
// steal the entry is owned by the thief, and ReleaseRunLocks for the old
// holder skips it (ownership check) while still freeing the holder's OTHER
// locks. Cancelling the displaced run is the CALLER's job (the server does
// it via the tracker) — this package doesn't know about runs.
//
// A live lock the holder PINNED refuses the steal: ErrLockPinned together
// with the holder's info (Pinned true), nothing mutated. The refused caller
// falls back to a blocking acquire, which wins when the pin lifts or the
// holder finishes — a newer event is deferred, never dropped.
func (s *Store) StealLock(ns, key, runID string, ttl time.Duration) (info LockInfo, displaced LockInfo, err error) {
	if !validNamespace(ns) {
		return LockInfo{}, LockInfo{}, ErrBadNamespace
	}
	if runID == "" {
		return LockInfo{}, LockInfo{}, errors.New("kv: lock requires a run identity")
	}
	ttl = lockTTL(ttl)

	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.takeLockLocked(ns, key, runID, ttl, true, false)
}

// PinLock marks the lock at key non-stealable. ONLY the holding run may pin
// (the ReleaseLock auth rule): ErrLockNotHeld when the lock is absent or
// expired, ErrLockHeld when a different live run holds it. Idempotent — a
// pinned lock pins again silently. The pin changes nothing else: acquiredAt
// and the TTL backstop are untouched, and the finish-seam release frees a
// pinned lock exactly like any other (a pin never outlives its run).
func (s *Store) PinLock(ns, key, runID string) error {
	return s.setPinLocked(ns, key, runID, true)
}

// UnpinLock clears the pin, restoring normal steal semantics. Same
// owner-only auth and idempotence as PinLock.
func (s *Store) UnpinLock(ns, key, runID string) error {
	return s.setPinLocked(ns, key, runID, false)
}

func (s *Store) setPinLocked(ns, key, runID string, pinned bool) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	s.lockMu.Lock()
	defer s.lockMu.Unlock()

	m, ok := s.locks[ns]
	if !ok {
		return ErrLockNotHeld
	}
	prev, ok := m[key]
	if !ok {
		return ErrLockNotHeld
	}
	// Expiry is not self-clearing (see the header): the owner may still pin
	// or unpin its own over-budget hold, and a non-owner is refused exactly
	// as it would be before the expiry.
	if prev.runID != runID {
		return ErrLockHeld
	}
	prev.pinned = pinned
	m[key] = prev
	return nil
}

func lockTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return DefaultLockTTL
	}
	return ttl
}

// takeLockLocked is the one compare-and-set acquire, take-and-pin, and
// steal all share. Caller holds lockMu. When steal is false and another
// live run holds the lock, it returns that holder's info with ErrLockHeld
// (mutating nothing); when steal is true the entry is transferred to runID
// and the displaced holder's info returned — UNLESS the holder pinned the
// lock, in which case the steal is refused with ErrLockPinned (mutating
// nothing; the info names the pinned holder). pin marks the freshly taken
// entry pinned atomically with the take; a plain re-acquire by the live
// owner PRESERVES an existing pin (unpinning is only ever the explicit
// UnpinLock, never a side effect).
func (s *Store) takeLockLocked(ns, key, runID string, ttl time.Duration, steal, pin bool) (info LockInfo, displaced LockInfo, err error) {
	now := time.Now()
	m, nsExisted := s.locks[ns]
	if nsExisted {
		if prev, ok := m[key]; ok && prev.runID != runID {
			// An EXPIRED hold is still a hold (see the header): report it as
			// its own condition so the server enforces the TTL against the
			// holder instead of quietly handing the mutex to two runs.
			if !steal && prev.expired(now) {
				return prev.info(ns), LockInfo{}, ErrLockExpired
			}
			if !steal {
				return prev.info(ns), LockInfo{}, ErrLockHeld
			}
			// A steal already IS the kill-the-holder path, so it displaces a
			// live and an expired holder alike (a pin protects neither less).
			if prev.pinned && !prev.expired(now) {
				return prev.info(ns), LockInfo{}, ErrLockPinned
			}
			displaced = prev.info(ns)
		}
	} else {
		if len(s.locks) >= s.cfg.MaxNamespaces {
			return LockInfo{}, LockInfo{}, ErrTooManyNS
		}
		m = make(map[string]lockEntry)
		s.locks[ns] = m
	}

	prev, keyExisted := m[key]

	e := lockEntry{runID: runID, acquiredAt: now, expiresAt: now.Add(ttl), pinned: pin}
	if keyExisted && prev.runID == runID {
		// Idempotent re-acquire by the owner — which is by definition alive,
		// since it is the one calling: keep the original take time, and keep
		// an existing pin (explicit unpin only). An owner re-acquiring PAST
		// its expiry refreshes the backstop rather than starting a new hold:
		// the TTL measures one continuous hold, and only the owner can do
		// this, so it can never prolong a dead run's lock.
		e.acquiredAt = prev.acquiredAt
		e.pinned = pin || prev.pinned
	}
	m[key] = e
	return e.info(ns), displaced, nil
}

// ReleaseLock atomically frees the lock at key in ns iff runID holds it —
// the server-side owner check, derived from the caller's token, that makes
// it impossible for one run to free another's lock. ErrLockNotHeld when the
// lock is absent; ErrLockHeld when a DIFFERENT run holds it, expired or not
// (expiry never reassigns ownership — see the header). A holder releasing
// its own over-budget lock succeeds: that is the normal, wanted ending.
func (s *Store) ReleaseLock(ns, key, runID string) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	s.lockMu.Lock()
	defer s.lockMu.Unlock()

	m, ok := s.locks[ns]
	if !ok {
		return ErrLockNotHeld
	}
	prev, ok := m[key]
	if !ok {
		return ErrLockNotHeld
	}
	if prev.runID != runID {
		return ErrLockHeld
	}
	delete(m, key)
	return nil
}

// ReapExpiredLock drops the entry at key iff it is EXPIRED and still held by
// holderRunID — the second half of TTL enforcement, for the one case where
// no kill is possible or needed: the holder is already CONFIRMED gone (the
// server found no live run behind it) yet its finish-seam release never
// landed, which is the release-path bug the backstop exists for. Reports
// whether it reaped, and refuses (false, nothing mutated) if the entry
// changed hands, is unexpired, or the holder does not match — so a caller
// racing the real holder's release can never delete a fresh hold.
func (s *Store) ReapExpiredLock(ns, key, holderRunID string) bool {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()

	m, ok := s.locks[ns]
	if !ok {
		return false
	}
	prev, ok := m[key]
	if !ok || prev.runID != holderRunID || !prev.expired(time.Now()) {
		return false
	}
	delete(m, key)
	if len(m) == 0 {
		delete(s.locks, ns)
	}
	return true
}

// ReleaseRunLocks frees EVERY lock runID still holds, across all namespaces,
// and returns how many it freed. This is the primary release mechanism: the
// runner calls it from the run tracker's OnFinish seam, which fires exactly
// once per run on every terminal path — so a run that crashed, timed out,
// was cancelled, or simply forgot to release still drops its locks the
// moment it finishes.
func (s *Store) ReleaseRunLocks(runID string) int {
	if runID == "" {
		return 0
	}
	s.lockMu.Lock()
	defer s.lockMu.Unlock()

	freed := 0
	for ns, m := range s.locks {
		for key, e := range m {
			if e.runID == runID {
				delete(m, key)
				freed++
			}
		}
		if len(m) == 0 {
			delete(s.locks, ns)
		}
	}
	return freed
}

// reapExpiredLocks drops expired lock entries whose holder is CERTAINLY
// GONE — the memory backstop for a run that ended without its finish-seam
// release, run from the store's existing sweeper alongside entry reaping.
//
// It is NOT the TTL's enforcement: an expired lock whose holder is still
// running is left exactly where it is (freeing it would hand one mutex to
// two live runs — see the header). Enforcement means killing that holder,
// and only the server can do that. Without a liveness oracle
// (SetRunLiveness) nothing is reaped at all: an unwired store cannot tell
// "dead holder" from "slow holder", and guessing wrong is the failure this
// whole path exists to prevent.
func (s *Store) reapExpiredLocks() {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.runLive == nil {
		return
	}
	now := time.Now()
	for ns, m := range s.locks {
		for key, e := range m {
			if e.expired(now) && !s.runLive(e.runID) {
				delete(m, key)
			}
		}
		if len(m) == 0 {
			delete(s.locks, ns)
		}
	}
}
