package kv

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLockAcquireContendRelease(t *testing.T) {
	s := newStore(t)

	info, err := s.AcquireLock("ns", "l", "run-a", 0)
	require.NoError(t, err)
	require.Equal(t, "run-a", info.RunID)
	require.False(t, info.AcquiredAt.IsZero())
	// The default backstop applied (no TTL named).
	require.WithinDuration(t, time.Now().Add(DefaultLockTTL), info.ExpiresAt, 5*time.Second)

	// Another live run contends.
	_, err = s.AcquireLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockHeld, err)

	// Owner releases; the contender can now take it.
	require.NoError(t, s.ReleaseLock("ns", "l", "run-a"))
	info, err = s.AcquireLock("ns", "l", "run-b", 0)
	require.NoError(t, err)
	require.Equal(t, "run-b", info.RunID)
}

func TestLockSameRunReacquireIsIdempotent(t *testing.T) {
	s := newStore(t)

	first, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	again, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	// Original take time kept; the backstop expiry refreshed (only the live
	// owner can do this, so it can never prolong a dead run's lock).
	require.Equal(t, first.AcquiredAt, again.AcquiredAt)
	require.False(t, again.ExpiresAt.Before(first.ExpiresAt))

	// Still one lock: a third run stays locked out.
	_, err = s.AcquireLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockHeld, err)
}

func TestLockContendedAcquireMutatesNothing(t *testing.T) {
	s := newStore(t)
	info, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)

	_, err = s.AcquireLock("ns", "l", "run-b", time.Hour)
	require.Equal(t, ErrLockHeld, err)

	s.lockMu.Lock()
	e := s.locks["ns"]["l"]
	s.lockMu.Unlock()
	// The holder, its take time, and crucially its expiry are untouched — a
	// contender must never restamp (extend) the holder's backstop.
	require.Equal(t, "run-a", e.runID)
	require.Equal(t, info.AcquiredAt, e.acquiredAt)
	require.Equal(t, info.ExpiresAt, e.expiresAt)
}

func TestLockReleaseOwnership(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "l", "run-a", 0)
	require.NoError(t, err)

	// Another run cannot release it.
	require.Equal(t, ErrLockHeld, s.ReleaseLock("ns", "l", "run-b"))
	// Releasing something never locked is not-held.
	require.Equal(t, ErrLockNotHeld, s.ReleaseLock("ns", "other", "run-a"))
	require.Equal(t, ErrLockNotHeld, s.ReleaseLock("nothing", "l", "run-a"))

	// The owner can, exactly once.
	require.NoError(t, s.ReleaseLock("ns", "l", "run-a"))
	require.Equal(t, ErrLockNotHeld, s.ReleaseLock("ns", "l", "run-a"))
}

// Expiry alone frees NOTHING (the operator's mutex ruling): an over-budget
// hold is still the holder's, a contender is told so by name, and only the
// holder's own release / finish seam / an enforced takeover ends it.
func TestLockTTLBackstopExpiryDoesNotFreeTheLock(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "l", "run-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)

	// A contender is refused — and told WHICH condition, so it can enforce
	// the TTL rather than assume the lock is free.
	info, err := s.AcquireLock("ns", "l", "run-b", 0)
	require.ErrorIs(t, err, ErrLockExpired)
	require.Equal(t, "run-a", info.RunID, "the refusal names the over-budget holder")

	// The holder itself is unaffected: it can still release its own lock...
	require.NoError(t, s.ReleaseLock("ns", "l", "run-a"))
	// ...and only then does the contender get it.
	info, err = s.AcquireLock("ns", "l", "run-b", 0)
	require.NoError(t, err)
	require.Equal(t, "run-b", info.RunID)
}

// The live owner re-acquiring past its own expiry refreshes the backstop
// (one continuous hold), and a contender is refused again afterwards.
func TestExpiredOwnerReacquireRefreshesTheBackstop(t *testing.T) {
	s := newStore(t)
	first, err := s.AcquireLock("ns", "l", "run-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)

	again, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, first.AcquiredAt, again.AcquiredAt, "one continuous hold, not a new one")
	require.True(t, again.ExpiresAt.After(first.ExpiresAt), "the backstop is refreshed")

	_, err = s.AcquireLock("ns", "l", "run-b", 0)
	require.ErrorIs(t, err, ErrLockHeld, "no longer expired — plain contention")
}

// ReapExpiredLock is the enforcement's second half, for a holder the caller
// has CONFIRMED dead. It refuses anything else.
func TestReapExpiredLockIsNarrow(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "l", "run-a", 10*time.Millisecond)
	require.NoError(t, err)

	require.False(t, s.ReapExpiredLock("ns", "l", "run-a"), "not expired yet")
	time.Sleep(30 * time.Millisecond)
	require.False(t, s.ReapExpiredLock("ns", "l", "run-b"), "wrong holder — never reap another run's lock")
	require.False(t, s.ReapExpiredLock("ns", "other", "run-a"), "no such lock")

	require.True(t, s.ReapExpiredLock("ns", "l", "run-a"))
	require.False(t, s.ReapExpiredLock("ns", "l", "run-a"), "idempotent — already gone")

	info, err := s.AcquireLock("ns", "l", "run-b", 0)
	require.NoError(t, err)
	require.Equal(t, "run-b", info.RunID)
}

func TestReleaseRunLocksFreesEverythingAcrossNamespaces(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns1", "a", "run-x", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns1", "b", "run-x", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns2", "c", "run-x", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns1", "keep", "run-y", 0)
	require.NoError(t, err)

	require.Equal(t, 3, s.ReleaseRunLocks("run-x"))
	require.Equal(t, 0, s.ReleaseRunLocks("run-x")) // idempotent

	// run-x's locks are all takeable again; run-y's survived.
	for _, k := range []struct{ ns, key string }{{"ns1", "a"}, {"ns1", "b"}, {"ns2", "c"}} {
		_, err := s.AcquireLock(k.ns, k.key, "run-z", 0)
		require.NoError(t, err, "%s/%s should be free", k.ns, k.key)
	}
	_, err = s.AcquireLock("ns1", "keep", "run-z", 0)
	require.Equal(t, ErrLockHeld, err)
}

func TestLockRestartStartsFree(t *testing.T) {
	// Locks are memory-only ON PURPOSE: no run survives a restart, so a fresh
	// store must start lock-free even over the same data dir.
	dir := t.TempDir()
	s1, err := New(Config{Dir: dir}, []byte("secret"), nil)
	require.NoError(t, err)
	_, err = s1.AcquireLock("ns", "l", "run-a", time.Hour)
	require.NoError(t, err)
	// A regular entry, for contrast, persists across the restart.
	require.NoError(t, s1.Set("ns", "durable", []byte("v"), time.Hour))

	s2, err := New(Config{Dir: dir}, []byte("secret"), nil)
	require.NoError(t, err)
	_, err = s2.AcquireLock("ns", "l", "run-b", 0)
	require.NoError(t, err, "a restart must start lock-free")
	_, ok := s2.Get("ns", "durable")
	require.True(t, ok, "entries still persist")
}

func TestLocksAreNotEntries(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "l", "run-a", 0)
	require.NoError(t, err)

	// Invisible to the entry surface: GET misses, List omits.
	_, ok := s.Get("ns", "l")
	require.False(t, ok)
	require.Empty(t, s.List("ns"))

	// And entry writes never disturb the lock (they are separate facilities
	// sharing a key string at most).
	require.NoError(t, s.Set("ns", "l", []byte("data"), 0))
	_, err = s.AcquireLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockHeld, err)
}

func TestLockCaps(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir(), MaxNamespaces: 2}, []byte("x"), nil)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns", "a", "r", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns2", "a", "r", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns3", "a", "r", 0)
	require.Equal(t, ErrTooManyNS, err)

	_, err = s.AcquireLock("Bad NS", "a", "r", 0)
	require.Equal(t, ErrBadNamespace, err)
}

// The sweeper reaps an expired lock ONLY once its holder is certainly gone,
// and reaps nothing at all without a liveness oracle — expiry is not
// evidence of death, and freeing a live holder's mutex is the bug this whole
// path exists to prevent.
func TestLockSweeperReapsOnlyDeadHolders(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "gone", "run-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)

	nsPresent := func() bool {
		s.lockMu.Lock()
		defer s.lockMu.Unlock()
		_, ok := s.locks["ns"]
		return ok
	}

	// No oracle: nothing is reaped, expired or not.
	s.reapExpiredLocks()
	require.True(t, nsPresent(), "an unwired store cannot tell dead from slow — it reaps nothing")

	// Holder still running: the expired lock stays exactly where it is.
	s.SetRunLiveness(func(id string) bool { return id == "run-a" })
	s.reapExpiredLocks()
	require.True(t, nsPresent(), "a LIVE holder's expired lock is never reaped out from under it")

	// Holder gone: now the shell is reclaimed (with its empty namespace).
	s.SetRunLiveness(func(string) bool { return false })
	s.reapExpiredLocks()
	require.False(t, nsPresent(), "a dead holder's expired lock (and its empty namespace) is reaped")
}

func TestLockAcquireRace(t *testing.T) {
	// The whole point of the server-side primitive: of N simultaneous
	// acquirers, exactly one holds.
	s := newStore(t)
	const goroutines = 64
	var wg sync.WaitGroup
	winners := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		runID := "run-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AcquireLock("ns", "l", runID, 0); err == nil {
				winners <- runID
			}
		}()
	}
	wg.Wait()
	close(winners)
	var held []string
	for w := range winners {
		held = append(held, w)
	}
	require.Len(t, held, 1, "exactly one acquirer may win")

	// And the winner's release frees it for exactly one next winner.
	require.NoError(t, s.ReleaseLock("ns", "l", held[0]))
	_, err := s.AcquireLock("ns", "l", "run-final", 0)
	require.NoError(t, err)
}

func TestLockContendedAcquireNamesHolder(t *testing.T) {
	s := newStore(t)
	held, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, "ns", held.HookID)

	// The contender learns exactly who holds the lock — run, hook (== the
	// lock's namespace), and since when — alongside the refusal.
	holder, err := s.AcquireLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockHeld, err)
	require.Equal(t, "run-a", holder.RunID)
	require.Equal(t, "ns", holder.HookID)
	require.Equal(t, held.AcquiredAt, holder.AcquiredAt)
	require.Equal(t, held.ExpiresAt, holder.ExpiresAt)
}

func TestStealLockTransfersFromLiveHolder(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "l", "run-victim", time.Minute)
	require.NoError(t, err)

	info, displaced, err := s.StealLock("ns", "l", "run-thief", 0)
	require.NoError(t, err)
	require.Equal(t, "run-thief", info.RunID)
	require.Equal(t, "run-victim", displaced.RunID)
	require.Equal(t, "ns", displaced.HookID)

	// The thief owns it now: the victim can neither release nor re-acquire.
	require.Equal(t, ErrLockHeld, s.ReleaseLock("ns", "l", "run-victim"))
	_, err = s.AcquireLock("ns", "l", "run-victim", 0)
	require.Equal(t, ErrLockHeld, err)
}

// The steal/finish race-safety invariant: a stolen lock is TRANSFERRED, not
// released — when the displaced run terminates and the finish seam frees
// everything it still holds, the stolen lock (now the thief's) is skipped
// while the victim's OTHER locks release normally.
func TestStealLockSurvivesVictimFinishSeamRelease(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "stolen", "run-victim", time.Minute)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns", "other", "run-victim", time.Minute)
	require.NoError(t, err)

	_, displaced, err := s.StealLock("ns", "stolen", "run-thief", 0)
	require.NoError(t, err)
	require.Equal(t, "run-victim", displaced.RunID)

	// The victim finishes; the finish seam frees ITS remaining locks only.
	require.Equal(t, 1, s.ReleaseRunLocks("run-victim"))

	// "other" is free again; "stolen" is still the thief's.
	_, err = s.AcquireLock("ns", "other", "run-z", 0)
	require.NoError(t, err)
	holder, err := s.AcquireLock("ns", "stolen", "run-z", 0)
	require.Equal(t, ErrLockHeld, err)
	require.Equal(t, "run-thief", holder.RunID)
}

// A steal of a free (or expired, or already-finished-holder) lock degrades
// to a plain acquire: no displaced party, no error.
func TestStealLockUncontendedIsPlainAcquire(t *testing.T) {
	s := newStore(t)

	info, displaced, err := s.StealLock("ns", "free", "run-a", 0)
	require.NoError(t, err)
	require.Equal(t, "run-a", info.RunID)
	require.Empty(t, displaced.RunID)

	// Holder released (e.g. finished) between contention and steal: same.
	require.NoError(t, s.ReleaseLock("ns", "free", "run-a"))
	info, displaced, err = s.StealLock("ns", "free", "run-b", 0)
	require.NoError(t, err)
	require.Equal(t, "run-b", info.RunID)
	require.Empty(t, displaced.RunID)
}

// Stealing a lock the caller already holds is an idempotent re-acquire:
// nothing displaced, original take time kept.
func TestStealLockOwnLockIsIdempotent(t *testing.T) {
	s := newStore(t)
	first, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	again, displaced, err := s.StealLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.Empty(t, displaced.RunID)
	require.Equal(t, first.AcquiredAt, again.AcquiredAt)
	require.False(t, again.ExpiresAt.Before(first.ExpiresAt))
}
