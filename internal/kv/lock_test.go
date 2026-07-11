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

func TestLockTTLBackstopExpiry(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "l", "run-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)

	// Expired = free: a release reports not-held...
	require.Equal(t, ErrLockNotHeld, s.ReleaseLock("ns", "l", "run-a"))
	// ...and another run's acquire succeeds.
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
	s, err := New(Config{Dir: t.TempDir(), MaxKeysPerNS: 2, MaxNamespaces: 2}, []byte("x"), nil)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns", "a", "r", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns", "b", "r", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns", "c", "r", 0)
	require.Equal(t, ErrTooManyKeys, err)

	_, err = s.AcquireLock("ns2", "a", "r", 0)
	require.NoError(t, err)
	_, err = s.AcquireLock("ns3", "a", "r", 0)
	require.Equal(t, ErrTooManyNS, err)

	_, err = s.AcquireLock("Bad NS", "a", "r", 0)
	require.Equal(t, ErrBadNamespace, err)
}

func TestLockReapExpired(t *testing.T) {
	s := newStore(t)
	_, err := s.AcquireLock("ns", "gone", "run-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)
	s.reapExpiredLocks()
	s.lockMu.Lock()
	_, nsLeft := s.locks["ns"]
	s.lockMu.Unlock()
	require.False(t, nsLeft, "expired lock (and its empty namespace) reaped")
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
