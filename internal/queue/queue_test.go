package queue

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "queues")
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return s
}

// The core contract: FIFO, push is a set union, take removes.
func TestPushTakeFIFO(t *testing.T) {
	s := newStore(t, Config{})

	res, err := s.Push("h", "backlog", []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Equal(t, PushResult{Queued: 3, Depth: 3}, res)

	items, depth, err := s.Take("h", "backlog", 2)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, items, "head first")
	require.Equal(t, 1, depth)

	items, depth, err = s.Take("h", "backlog", 10)
	require.NoError(t, err)
	require.Equal(t, []string{"c"}, items, "take never blocks on a short queue")
	require.Equal(t, 0, depth)

	items, depth, err = s.Take("h", "backlog", 5)
	require.NoError(t, err)
	require.Empty(t, items, "an empty queue is not an error — it is nothing to do")
	require.Equal(t, 0, depth)
}

// Re-pushing the whole backlog every tick is the INTENDED usage: duplicates
// are skipped and — the part that matters — keep their original position, so
// the tail cannot starve behind items that keep being re-pushed.
func TestPushDedupeKeepsOriginalPosition(t *testing.T) {
	s := newStore(t, Config{})

	_, err := s.Push("h", "backlog", []string{"a", "b", "c"})
	require.NoError(t, err)

	res, err := s.Push("h", "backlog", []string{"a", "b", "c", "d"})
	require.NoError(t, err)
	require.Equal(t, PushResult{Queued: 1, Duplicates: 3, Depth: 4}, res)

	items, _, err := s.Take("h", "backlog", 4)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c", "d"}, items, "the re-push must not move 'a' behind 'd'")

	// Dedupe is per-queue, not per-namespace.
	_, err = s.Push("h", "other", []string{"a"})
	require.NoError(t, err)
	require.Equal(t, 1, s.Depth("h", "other"))
}

// A queue outliving the process that filled it is the whole point.
func TestQueueSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queues")
	s := newStore(t, Config{Dir: dir})
	_, err := s.Push("h", "backlog", []string{"a", "b"})
	require.NoError(t, err)

	reopened := newStore(t, Config{Dir: dir})
	items, depth, err := reopened.Take("h", "backlog", 5)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, items)
	require.Equal(t, 0, depth)

	// A fully drained queue leaves nothing behind to reload.
	again := newStore(t, Config{Dir: dir})
	require.Equal(t, 0, again.Depth("h", "backlog"))
	require.Empty(t, again.List("h"))
}

// The depth cap never silently swallows work: the overflow is COUNTED and
// reported, because a full backlog means the consumer is not keeping up.
func TestDepthCapReportsDropped(t *testing.T) {
	s := newStore(t, Config{MaxDepth: 3})

	res, err := s.Push("h", "backlog", []string{"a", "b", "c", "d", "e"})
	require.NoError(t, err)
	require.Equal(t, PushResult{Queued: 3, Dropped: 2, Depth: 3}, res)

	items, _, err := s.Take("h", "backlog", 5)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, items, "the head is kept, the overflow is refused")
}

func TestListReportsDepthsSorted(t *testing.T) {
	s := newStore(t, Config{})
	_, err := s.Push("h", "sweep", []string{"x"})
	require.NoError(t, err)
	_, err = s.Push("h", "minded", []string{"a", "b"})
	require.NoError(t, err)

	require.Equal(t, []Stat{{Name: "minded", Depth: 2}, {Name: "sweep", Depth: 1}}, s.List("h"))
	require.Empty(t, s.List("other-hook"), "namespaces are isolated")
}

func TestValidationRejectsBadInput(t *testing.T) {
	s := newStore(t, Config{MaxItemBytes: 8, MaxQueues: 1})

	_, err := s.Push("Bad NS", "q", []string{"a"})
	require.ErrorIs(t, err, ErrBadNamespace)
	_, err = s.Push("h", "Bad Name", []string{"a"})
	require.ErrorIs(t, err, ErrBadName)
	_, err = s.Push("h", "q", []string{""})
	require.ErrorIs(t, err, ErrEmptyItem)
	_, err = s.Push("h", "q", []string{"waaaaaaaaay too long"})
	require.ErrorIs(t, err, ErrItemTooLarge)

	// A rejected push mutates nothing — not even the namespace it would have
	// created.
	require.Empty(t, s.List("h"))

	_, err = s.Push("h", "q", []string{"ok"})
	require.NoError(t, err)
	_, err = s.Push("h", "second", []string{"ok"})
	require.ErrorIs(t, err, ErrTooManyQueues)
	require.Equal(t, []Stat{{Name: "q", Depth: 1}}, s.List("h"), "the refused queue was never created")
}
