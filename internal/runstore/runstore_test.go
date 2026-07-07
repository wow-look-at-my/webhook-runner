package runstore

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

func newStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "runs.db")
	}
	s, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func state(id, hook string, status runs.Status, started time.Time) runs.RunState {
	return runs.RunState{
		ID:       id,
		HookID:   hook,
		Status:   status,
		ExitCode: 0,
		Started:  started,
		Finished: started.Add(3 * time.Second),
	}
}

func TestRecordGetRoundtrip(t *testing.T) {
	s := newStore(t, Config{})
	st := state("aaaaaaaaaaaaaaaaaaaaaaaaaa", "h", runs.StatusFailure, time.Now().UTC().Add(-time.Minute))
	st.ExitCode = 3
	st.Error = "boom"
	st.Output = []string{"line1", "line2"}
	st.OutputTimes = []time.Time{st.Started, st.Started.Add(time.Second)}
	require.NoError(t, s.Record(st))

	got, ok := s.Get(st.ID)
	require.True(t, ok)
	assert.Equal(t, st.ID, got.ID)
	assert.Equal(t, "h", got.HookID)
	assert.Equal(t, runs.StatusFailure, got.Status)
	assert.Equal(t, 3, got.ExitCode)
	assert.Equal(t, "boom", got.Error)
	assert.Equal(t, []string{"line1", "line2"}, got.Output)
	require.Len(t, got.OutputTimes, 2)
	assert.True(t, got.OutputTimes[1].Equal(st.OutputTimes[1]))

	_, ok = s.Get("missing")
	assert.False(t, ok)
}

func TestRecordRejectsNonTerminal(t *testing.T) {
	s := newStore(t, Config{})
	err := s.Record(state("bbbbbbbbbbbbbbbbbbbbbbbbbb", "h", runs.StatusRunning, time.Now().UTC()))
	require.Error(t, err)
	_, ok := s.Get("bbbbbbbbbbbbbbbbbbbbbbbbbb")
	assert.False(t, ok)
}

func TestSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s1, err := Open(Config{Path: path}, logger)
	require.NoError(t, err)
	st := state("cccccccccccccccccccccccccc", "h", runs.StatusSuccess, time.Now().UTC().Add(-time.Minute))
	st.Output = []string{"kept"}
	st.OutputTimes = []time.Time{st.Started}
	require.NoError(t, s1.Record(st))
	require.NoError(t, s1.Close())

	s2, err := Open(Config{Path: path}, logger)
	require.NoError(t, err)
	defer s2.Close()
	got, ok := s2.Get(st.ID)
	require.True(t, ok, "run lost across reopen")
	assert.Equal(t, []string{"kept"}, got.Output)
	require.Len(t, s2.ListByHook("h", 0), 1)
}

func TestListsNewestFirstFilteredAndCapped(t *testing.T) {
	s := newStore(t, Config{})
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		hook := "a"
		if i%2 == 1 {
			hook = "b"
		}
		st := state(fmt.Sprintf("run%d", i), hook, runs.StatusSuccess, base.Add(time.Duration(i)*time.Minute))
		st.Output = []string{"noise"}
		st.OutputTimes = []time.Time{st.Started}
		require.NoError(t, s.Record(st))
	}

	all := s.ListAll(0)
	require.Len(t, all, 5)
	for i := 0; i < len(all)-1; i++ {
		assert.True(t, all[i].Started.After(all[i+1].Started), "not newest-first at %d", i)
	}
	// Lists never carry output.
	for _, st := range all {
		assert.Nil(t, st.Output)
		assert.Nil(t, st.OutputTimes)
	}

	assert.Len(t, s.ListAll(2), 2)
	assert.Equal(t, "run4", s.ListAll(2)[0].ID)

	a := s.ListByHook("a", 0)
	require.Len(t, a, 3)
	assert.Equal(t, "run4", a[0].ID)
	assert.Equal(t, "run0", a[2].ID)
	assert.Empty(t, s.ListByHook("nope", 0))
}

// The retention boundary is lazy on reads: a run just past the window
// disappears from Get/lists even before the sweeper has reclaimed it, and
// the boundary instant itself counts as expired (kv's TTL convention).
func TestRetentionBoundaryOnReads(t *testing.T) {
	s := newStore(t, Config{Retention: time.Hour})
	now := time.Now().UTC()
	inside := state("iiiiiiiiiiiiiiiiiiiiiiiiii", "h", runs.StatusSuccess, now.Add(-time.Hour+5*time.Second))
	outside := state("oooooooooooooooooooooooooo", "h", runs.StatusSuccess, now.Add(-time.Hour-5*time.Second))
	require.NoError(t, s.Record(inside))
	require.NoError(t, s.Record(outside))

	_, ok := s.Get(inside.ID)
	assert.True(t, ok)
	_, ok = s.Get(outside.ID)
	assert.False(t, ok, "expired run still readable")

	ids := func(states []runs.RunState) []string {
		var out []string
		for _, st := range states {
			out = append(out, st.ID)
		}
		return out
	}
	assert.Equal(t, []string{inside.ID}, ids(s.ListAll(0)))
	assert.Equal(t, []string{inside.ID}, ids(s.ListByHook("h", 0)))
	assert.Equal(t, []string{inside.ID}, ids(s.SummariesByHook("h")))
}

// The boundary instant itself is expired — Started exactly Retention ago is
// out, one nanosecond fresher is in (the kv store's TTL convention).
func TestExpiredBoundaryInstant(t *testing.T) {
	s := newStore(t, Config{Retention: time.Hour})
	now := time.Now().UTC()
	assert.True(t, s.expired(now.Add(-time.Hour), now))
	assert.False(t, s.expired(now.Add(-time.Hour+time.Nanosecond), now))
}

func TestSweepReclaimsExpired(t *testing.T) {
	s := newStore(t, Config{Retention: time.Hour})
	now := time.Now().UTC()
	old1 := state("dddddddddddddddddddddddddd", "a", runs.StatusSuccess, now.Add(-3*time.Hour))
	old1.Output = []string{"bye"}
	old1.OutputTimes = []time.Time{old1.Started}
	old2 := state("eeeeeeeeeeeeeeeeeeeeeeeeee", "b", runs.StatusFailure, now.Add(-2*time.Hour))
	fresh := state("ffffffffffffffffffffffffff", "a", runs.StatusSuccess, now.Add(-time.Minute))
	for _, st := range []runs.RunState{old1, old2, fresh} {
		require.NoError(t, s.Record(st))
	}

	removed, err := s.sweep(now)
	require.NoError(t, err)
	assert.Equal(t, 2, removed)

	// The fresh run is intact, the expired ones are gone from every bucket
	// (a second sweep finding nothing proves the indexes went too).
	_, ok := s.Get(fresh.ID)
	assert.True(t, ok)
	_, ok = s.Get(old1.ID)
	assert.False(t, ok)
	assert.Empty(t, s.ListByHook("b", 0))
	assert.Empty(t, s.SummariesByHook("b"))
	removed, err = s.sweep(now)
	require.NoError(t, err)
	assert.Zero(t, removed)
}

// MaxPerHook is the coarse disk safety net behind time retention: the sweep
// prunes a hook's oldest runs beyond the cap even when none have expired,
// and other hooks are untouched.
func TestSweepEnforcesPerHookCap(t *testing.T) {
	s := newStore(t, Config{Retention: 24 * time.Hour, MaxPerHook: 3})
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		st := state(fmt.Sprintf("capped%d", i), "big", runs.StatusSuccess, base.Add(time.Duration(i)*time.Minute))
		st.Output = []string{"x"}
		st.OutputTimes = []time.Time{st.Started}
		require.NoError(t, s.Record(st))
	}
	require.NoError(t, s.Record(state("small0", "small", runs.StatusSuccess, base)))

	removed, err := s.sweep(time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 2, removed)

	kept := s.ListByHook("big", 0)
	require.Len(t, kept, 3)
	assert.Equal(t, "capped4", kept[0].ID)
	assert.Equal(t, "capped2", kept[2].ID)
	for _, id := range []string{"capped0", "capped1"} {
		_, ok := s.Get(id)
		assert.False(t, ok, "%s survived the cap", id)
	}
	assert.Len(t, s.ListAll(0), 4)
	assert.Len(t, s.ListByHook("small", 0), 1)
}

// Summaries come from the index alone but must agree with the metadata on
// the fields stats aggregate: ID, hook, status, start and finish times.
func TestSummariesMatchMetadata(t *testing.T) {
	s := newStore(t, Config{})
	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(0)
	st := state("gggggggggggggggggggggggggg", "h", runs.StatusTimeout, started)
	require.NoError(t, s.Record(st))

	sums := s.SummariesByHook("h")
	require.Len(t, sums, 1)
	assert.Equal(t, st.ID, sums[0].ID)
	assert.Equal(t, "h", sums[0].HookID)
	assert.Equal(t, runs.StatusTimeout, sums[0].Status)
	assert.True(t, sums[0].Started.Equal(st.Started))
	assert.True(t, sums[0].Finished.Equal(st.Finished))
}
