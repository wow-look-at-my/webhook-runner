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
// separate facility that happens to share the namespace and its caps.
//
// The TTL backstop: every lock gets an expiry — DefaultLockTTL when the
// caller doesn't choose one — sized far beyond any legitimate hold, because
// it exists only to unwedge a lock if the finish-seam release were ever
// broken by a bug. Expiry is enforced lazily (an expired lock reads as free
// at acquire/release time) and reaped by the store's existing sweeper.
// Contended acquires mutate NOTHING — in particular they never restamp the
// holder's expiry, so contenders can't keep a dead run's lock alive — and
// they name the holder (LockInfo alongside ErrLockHeld), so contention is
// never anonymous. StealLock is the destructive counterpart: it transfers
// a held lock to the caller atomically (namespace-scoped, so a run can only
// ever displace a run of its OWN hook); cancelling the displaced run is the
// server's job.
var (
	// ErrLockHeld: the lock is held by a different live run (acquire), or the
	// caller tried to release a lock a different live run holds (release).
	ErrLockHeld = errors.New("kv: lock held by another run")
	// ErrLockNotHeld: nothing (live) to release.
	ErrLockNotHeld = errors.New("kv: lock not held")
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
}

func (l lockEntry) expired(now time.Time) bool {
	return !l.expiresAt.After(now)
}

// LockInfo describes a lock's holder: what a successful acquire/steal
// reports back to the caller, and — on a contended acquire — WHO currently
// holds the lock (so a contender can display, wait on, or steal from a
// named holder rather than an anonymous 409). HookID is the lock's
// namespace, which is the holding run's hook.
type LockInfo struct {
	RunID      string    `json:"run_id"`
	HookID     string    `json:"hook_id"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (e lockEntry) info(ns string) LockInfo {
	return LockInfo{RunID: e.runID, HookID: ns, AcquiredAt: e.acquiredAt, ExpiresAt: e.expiresAt}
}

// AcquireLock atomically takes the cooperative lock at key in ns for runID.
// A free (absent or expired) lock is taken with expiry now+ttl (DefaultLockTTL
// when ttl <= 0). A lock the SAME run already holds is re-acquired
// idempotently — its backstop expiry refreshed, its original acquiredAt kept
// (only the live owner can do this, so it can never prolong a dead run's
// lock). A lock held by another live run returns ErrLockHeld — mutating
// nothing — together with the HOLDER's LockInfo, so contention is never
// anonymous. The same namespace/key caps as the entry store apply.
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
	info, _, err := s.takeLockLocked(ns, key, runID, ttl, false)
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
	return s.takeLockLocked(ns, key, runID, ttl, true)
}

func lockTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return DefaultLockTTL
	}
	return ttl
}

// takeLockLocked is the one compare-and-set both acquire and steal share.
// Caller holds lockMu. When steal is false and another live run holds the
// lock, it returns that holder's info with ErrLockHeld (mutating nothing);
// when steal is true the entry is transferred to runID and the displaced
// holder's info returned.
func (s *Store) takeLockLocked(ns, key, runID string, ttl time.Duration, steal bool) (info LockInfo, displaced LockInfo, err error) {
	now := time.Now()
	m, nsExisted := s.locks[ns]
	if nsExisted {
		if prev, ok := m[key]; ok && !prev.expired(now) && prev.runID != runID {
			if !steal {
				return prev.info(ns), LockInfo{}, ErrLockHeld
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
	if !keyExisted && len(m) >= s.cfg.MaxKeysPerNS {
		if !nsExisted {
			delete(s.locks, ns)
		}
		return LockInfo{}, LockInfo{}, ErrTooManyKeys
	}

	e := lockEntry{runID: runID, acquiredAt: now, expiresAt: now.Add(ttl)}
	if keyExisted && !prev.expired(now) && prev.runID == runID {
		// Idempotent re-acquire by the live owner: keep the original take time.
		e.acquiredAt = prev.acquiredAt
	}
	m[key] = e
	return e.info(ns), displaced, nil
}

// ReleaseLock atomically frees the lock at key in ns iff runID holds it —
// the server-side owner check, derived from the caller's token, that makes
// it impossible for one run to free another's lock. ErrLockNotHeld when the
// lock is absent or expired (it already self-cleared); ErrLockHeld when a
// different live run holds it.
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
	if prev.expired(time.Now()) {
		// Already self-cleared; reap the shell while we're here.
		delete(m, key)
		return ErrLockNotHeld
	}
	if prev.runID != runID {
		return ErrLockHeld
	}
	delete(m, key)
	return nil
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

// reapExpiredLocks drops expired lock entries. Lazy expiry already treats
// them as free; this is the memory backstop, run from the store's existing
// sweeper alongside entry reaping.
func (s *Store) reapExpiredLocks() {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	now := time.Now()
	for ns, m := range s.locks {
		for key, e := range m {
			if e.expired(now) {
				delete(m, key)
			}
		}
		if len(m) == 0 {
			delete(s.locks, ns)
		}
	}
}
