package kv

import (
	"errors"
	"time"
)

// Cooperative per-key locks, owned by RUN INSTANCES. The operator's locking model: tracking which hook run holds a lock — and releasing everything it holds when that run terminates for any reason — is the PRIMARY liveness mechanism; a TTL is only a secondary backstop. So a lock here is bound to the run identity carried by the caller's state token (see Token/VerifyToken), acquire/release are atomic under mutex (the compare-and-set/compare-and-delete a client cannot build from GET+PUT), and the runner frees a run's remaining locks at the tracker's OnFinish seam — which fires exactly on EVERY terminal path (success, error, timeout kill, cancel), wired in cli/serve.go.
var (
	// ErrLockHeld: the lock is held by a different live run (acquire), or the caller tried to release a lock a different live run holds (release).
	ErrLockHeld = errors.New("kv: lock held by another run")
	// ErrLockNotHeld: nothing (live) to release.
	ErrLockNotHeld = errors.New("kv: lock not held")
	// ErrLockExpired: the holder's TTL backstop passed, but the lock is STILL the holder's — expiry alone frees nothing.
	ErrLockExpired = errors.New("kv: lock TTL expired — the holder must be killed before the lock frees")
	// ErrLockPinned: a steal was refused because the holder PINNED the lock (marked its critical section non-displaceable).
	ErrLockPinned = errors.New("kv: lock is pinned by its holder (steal refused)")
)

// DefaultLockTTL is the backstop expiry applied when an acquire names no ttl_seconds.
const DefaultLockTTL = 15 * time.Minute

type lockEntry struct {
	runID      string
	acquiredAt time.Time
	expiresAt  time.Time
	// pinned marks the holder's critical section non-displaceable: a steal of a live pinned lock is refused with ErrLockPinned.
	pinned bool
}

func (l lockEntry) expired(now time.Time) bool {
	return !l.expiresAt.After(now)
}

// LockInfo describes a lock's holder: what a successful acquire/steal reports back to the caller, and — on a contended acquire — WHO currently holds the lock (so a contender can display, wait on, or steal from a named.
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

// AcquireLock atomically takes the cooperative lock at key in ns for runID. A free (absent or expired) lock is taken with expiry now+ttl (DefaultLockTTL when ttl <= ). A lock the SAME run already holds is re-acquired idempotently — its backstop expiry refreshed, its original acquiredAt kept (only the live owner can do this, so it can never prolong a dead run's lock). A lock held by another live run returns ErrLockHeld — mutating nothing — together with the HOLDER's LockInfo, so contention is never anonymous.
func (s *Store) AcquireLock(ns, key, runID string, ttl time.Duration) (LockInfo, error) {
	if !validNamespace(ns) {
		return LockInfo{}, ErrBadNamespace
	}
	if runID == "" {
		// Unreachable through the state API (VerifyToken rejects tokens without a run identity); refuse rather than mint anonymous locks.
		return LockInfo{}, errors.New("kv: lock requires a run identity")
	}
	ttl = lockTTL(ttl)

	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	info, _, err := s.takeLockLocked(ns, key, runID, ttl, false, false)
	return info, err
}

// AcquireLockPinned is AcquireLock with the pin applied ATOMICALLY in the
// same compare-and-set — take-and-pin in step, so no stealer can slip
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

// StealLock atomically takes the lock at key in ns for runID EVEN IF another live run holds it, returning the displaced holder's info (displaced.RunID == "" when the lock was free or already ours — a steal of an uncontended lock is exactly an acquire).
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

// PinLock marks the lock at key non-stealable.
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
	// Expiry is not self-clearing (see the header): the owner may still pin or unpin its own over-budget hold, and a non-owner is refused exactly.
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

// takeLockLocked is the compare-and-set acquire, take-and-pin, and
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
			// An EXPIRED hold is still a hold (see the header): report it as its own condition so the server enforces the TTL against the holder instead of.
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
		// Idempotent re-acquire by the owner — which is by definition alive, since it is the calling: keep the original take time, and keep an.
		e.acquiredAt = prev.acquiredAt
		e.pinned = pin || prev.pinned
	}
	m[key] = e
	return e.info(ns), displaced, nil
}

// ReleaseLock atomically frees the lock at key in ns iff runID holds it — the server-side owner check, derived from the caller's token, that makes it impossible for run to free another's lock.
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

// ReapExpiredLock drops the entry at key iff it is EXPIRED and still held by holderRunID — the half of TTL enforcement, for the case where no kill is possible or needed: the holder is already CONFIRMED gone (the server found no live run behind it) yet its finish-seam release never landed, which is the release-path.
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

// ReleaseRunLocks frees EVERY lock runID still holds, across all namespaces, and returns how many it freed.
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

// reapExpiredLocks drops expired lock entries whose holder is CERTAINLY GONE — the memory backstop for a run that ended without its finish-seam release, run from the store's existing sweeper alongside entry reaping.
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
