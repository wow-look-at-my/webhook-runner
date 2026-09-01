package kv

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A pinned lock refuses steals with ErrLockPinned — naming the pinned
// holder, mutating nothing — until unpinned; then the steal transfers.
func TestPinnedLockRefusesSteal(t *testing.T) {
	s := newStore(t)

	held, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.False(t, held.Pinned)
	require.NoError(t, s.PinLock("ns", "l", "run-a"))

	info, displaced, err := s.StealLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockPinned, err)
	require.Equal(t, "run-a", info.RunID)
	require.True(t, info.Pinned)
	require.Empty(t, displaced.RunID)

	// Nothing mutated: the owner still holds, still pinned.
	again, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, held.AcquiredAt, again.AcquiredAt)
	require.True(t, again.Pinned)

	// Unpin restores normal steal semantics.
	require.NoError(t, s.UnpinLock("ns", "l", "run-a"))
	info, displaced, err = s.StealLock("ns", "l", "run-b", 0)
	require.NoError(t, err)
	require.Equal(t, "run-b", info.RunID)
	require.Equal(t, "run-a", displaced.RunID)
}

// A plain contended ACQUIRE behaves identically pinned or not (it never
// displaced anyone) — and its info now reports the pin.
func TestPinnedLockContendedAcquireNamesPin(t *testing.T) {
	s := newStore(t)

	_, err := s.AcquireLockPinned("ns", "l", "run-a", 0)
	require.NoError(t, err)

	info, err := s.AcquireLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockHeld, err)
	require.Equal(t, "run-a", info.RunID)
	require.True(t, info.Pinned)
}

// AcquireLockPinned is take-and-pin in compare-and-set: the very
// observable state is already pinned.
func TestAcquireLockPinnedIsAtomic(t *testing.T) {
	s := newStore(t)

	info, err := s.AcquireLockPinned("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.True(t, info.Pinned)

	_, _, err = s.StealLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockPinned, err)
}

// Only the holding run may pin or unpin — the release auth rule.
func TestPinOwnership(t *testing.T) {
	s := newStore(t)

	// Nothing held: -shaped.
	require.Equal(t, ErrLockNotHeld, s.PinLock("ns", "l", "run-a"))
	require.Equal(t, ErrLockNotHeld, s.UnpinLock("ns", "l", "run-a"))

	_, err := s.AcquireLock("ns", "l", "run-a", 0)
	require.NoError(t, err)

	// A different run may not toggle the holder's pin.
	require.Equal(t, ErrLockHeld, s.PinLock("ns", "l", "run-b"))
	require.Equal(t, ErrLockHeld, s.UnpinLock("ns", "l", "run-b"))

	// The owner may, idempotently.
	require.NoError(t, s.PinLock("ns", "l", "run-a"))
	require.NoError(t, s.PinLock("ns", "l", "run-a"))
	require.NoError(t, s.UnpinLock("ns", "l", "run-a"))
	require.NoError(t, s.UnpinLock("ns", "l", "run-a"))
}

// A same-owner re-acquire PRESERVES an existing pin: unpinning is only ever
// the explicit UnpinLock, never a side effect of touching the lock.
func TestReacquirePreservesPin(t *testing.T) {
	s := newStore(t)

	_, err := s.AcquireLockPinned("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)

	again, err := s.AcquireLock("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.True(t, again.Pinned)

	_, _, err = s.StealLock("ns", "l", "run-b", 0)
	require.Equal(t, ErrLockPinned, err)
}

// The pin dies with its run: the finish-seam release frees pinned locks
// exactly like any other, and the freed key is takeable (unpinned).
func TestPinReleasedByRunFinishSeam(t *testing.T) {
	s := newStore(t)

	_, err := s.AcquireLockPinned("ns", "l", "run-a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, s.ReleaseRunLocks("run-a"))

	info, err := s.AcquireLock("ns", "l", "run-b", 0)
	require.NoError(t, err)
	require.Equal(t, "run-b", info.RunID)
	require.False(t, info.Pinned)
}

// The TTL backstop ignores the pin: a pinned lock past its backstop is
// STEALABLE like any other (pin protects against steal only while the hold
// is within budget). A plain acquire still does not take it — expiry frees
// nothing on its own — it reports the expiry so the server can enforce it.
func TestPinDoesNotOutliveTTLBackstop(t *testing.T) {
	s := newStore(t)

	_, err := s.AcquireLockPinned("ns", "l", "run-a", 10*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	held, err := s.AcquireLock("ns", "l", "run-b", 0)
	require.ErrorIs(t, err, ErrLockExpired, "expiry is enforced, never assumed")
	require.Equal(t, "run-a", held.RunID)

	info, displaced, err := s.StealLock("ns", "l", "run-b", 0)
	require.NoError(t, err, "the pin does not survive the backstop")
	require.Equal(t, "run-b", info.RunID)
	require.False(t, info.Pinned)
	require.Equal(t, "run-a", displaced.RunID)
}

// A steal-with-pin request against a FREE lock is a plain take-and-pin (a
// steal of an uncontended lock is exactly an acquire) — and an explicit
// release clears the pinned entry wholesale.
func TestPinReleaseClearsPin(t *testing.T) {
	s := newStore(t)

	_, err := s.AcquireLockPinned("ns", "l", "run-a", 0)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseLock("ns", "l", "run-a"))

	info, err := s.AcquireLock("ns", "l", "run-a", 0)
	require.NoError(t, err)
	require.False(t, info.Pinned)
}
