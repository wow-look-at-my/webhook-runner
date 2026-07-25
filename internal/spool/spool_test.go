package spool

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The spool's whole reason to exist: a delivery that arrives during a deploy
// must not be lost. GitHub does not re-send a failed one, so anything that
// leaves this directory without running is gone for good.

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool"), nil)
	require.NoError(t, err)
	return s
}

func TestPutReplayRoundTrip(t *testing.T) {
	s := openTemp(t)
	id, err := s.Put(Entry{
		HookID:  "pr-minder",
		Title:   "t",
		Headers: map[string][]string{"X-Github-Event": {"pull_request"}},
		Body:    []byte(`{"action":"opened"}`),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	assert.Equal(t, 1, s.Len())

	var got []Entry
	n := s.Replay(func(e Entry) error { got = append(got, e); return nil })

	assert.Equal(t, 1, n)
	require.Len(t, got, 1)
	assert.Equal(t, "pr-minder", got[0].HookID)
	assert.Equal(t, "t", got[0].Title)
	assert.Equal(t, []byte(`{"action":"opened"}`), got[0].Body)
	assert.Equal(t, []string{"pull_request"}, got[0].Headers["X-Github-Event"],
		"headers must survive so the hook cannot tell a replay from the live delivery")
	assert.Zero(t, s.Len(), "a replayed entry is removed")
}

func TestReplayIsOldestFirst(t *testing.T) {
	s := openTemp(t)
	base := time.Now().UTC()
	for i, id := range []string{"third", "first", "second"} {
		offset := map[string]time.Duration{"first": 0, "second": time.Second, "third": 2 * time.Second}[id]
		_, err := s.Put(Entry{ID: id, HookID: "h", Body: []byte{byte(i)}, Received: base.Add(offset)})
		require.NoError(t, err)
	}

	var order []string
	s.Replay(func(e Entry) error { order = append(order, e.ID); return nil })

	assert.Equal(t, []string{"first", "second", "third"}, order,
		"arrival order is preserved so a hook sees its events in sequence")
}

// THE core guarantee: a dispatch that fails leaves the delivery on disk for
// the next boot. Dropping it here would be the exact loss this package exists
// to prevent.
func TestFailedDispatchKeepsTheEntry(t *testing.T) {
	s := openTemp(t)
	_, err := s.Put(Entry{HookID: "h", Body: []byte("x")})
	require.NoError(t, err)

	n := s.Replay(func(Entry) error { return errors.New("runner not ready") })

	assert.Zero(t, n)
	assert.Equal(t, 1, s.Len(), "a failed replay must never lose the delivery")

	n = s.Replay(func(Entry) error { return nil }) // the next boot succeeds
	assert.Equal(t, 1, n)
	assert.Zero(t, s.Len())
}

// One unparseable entry must not wedge every delivery behind it.
func TestCorruptEntryIsQuarantinedNotBlocking(t *testing.T) {
	s := openTemp(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "00000000000000000001-bad.json"), []byte("{not json"), 0o600))
	_, err := s.Put(Entry{HookID: "h", Body: []byte("good"), Received: time.Unix(0, 2)})
	require.NoError(t, err)

	var seen int
	n := s.Replay(func(Entry) error { seen++; return nil })

	assert.Equal(t, 1, n, "the good entry still ran")
	assert.Equal(t, 1, seen)
	entries, err := os.ReadDir(s.dir)
	require.NoError(t, err)
	var quarantined bool
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".corrupt" {
			quarantined = true
		}
	}
	assert.True(t, quarantined, "the corrupt entry is set aside, not deleted — it stays inspectable")
}

func TestPutRefusesPastEntryBound(t *testing.T) {
	s := openTemp(t)
	s.maxEntries = 2
	for i := 0; i < 2; i++ {
		_, err := s.Put(Entry{HookID: "h", Body: []byte("x")})
		require.NoError(t, err)
	}

	_, err := s.Put(Entry{HookID: "h", Body: []byte("x")})

	assert.ErrorIs(t, err, ErrFull,
		"a full spool must say so, so the caller answers an honest 503 instead of pretending")
}

func TestPutRefusesPastByteBound(t *testing.T) {
	s := openTemp(t)
	s.maxBytes = 32

	_, err := s.Put(Entry{HookID: "h", Body: make([]byte, 64)})

	assert.ErrorIs(t, err, ErrFull)
}

// A crash mid-write must not leave a half-entry the next boot would replay.
func TestTempFilesAreNeverReplayed(t *testing.T) {
	s := openTemp(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, ".spool-halfwritten.tmp"), []byte(`{"hook_id":"h"`), 0o600))

	assert.Zero(t, s.Len(), "an uncommitted temp is not a delivery")
	assert.Zero(t, s.Replay(func(Entry) error { return nil }))
}

func TestPutStampsIDAndReceived(t *testing.T) {
	s := openTemp(t)
	id, err := s.Put(Entry{HookID: "h", Body: []byte("x")})
	require.NoError(t, err)

	var got Entry
	s.Replay(func(e Entry) error { got = e; return nil })

	assert.Equal(t, id, got.ID)
	assert.False(t, got.Received.IsZero(), "receipt time is stamped so replay can order by arrival")
}

func TestOpenIsIdempotentAndSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s1, err := Open(dir, nil)
	require.NoError(t, err)
	_, err = s1.Put(Entry{HookID: "h", Body: []byte("x")})
	require.NoError(t, err)

	// The next PROCESS is what replays: reopening must find the entry.
	s2, err := Open(dir, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, s2.Len(), "the spool survives the restart it exists for")
}

func TestReplayOnEmptySpoolIsANoop(t *testing.T) {
	s := openTemp(t)
	assert.Zero(t, s.Replay(func(Entry) error { t.Fatal("must not be called"); return nil }))
}
