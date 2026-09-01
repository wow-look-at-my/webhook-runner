package runstore

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// A title rides the run's metadata BLOB — and only the blob: Get and the
// list walks return it, while the per-hook index value keeps its exact
// "<status> <finished-nanos> <startedat-nanos>" format (SummariesByHook's
// contract), byte-identical to what an untitled run writes.
func TestRecordTitleRoundtrip(t *testing.T) {
	s := newStore(t, Config{})
	started := time.Now().UTC().Add(-time.Minute)
	titled := state("titledtitledtitledtitledti", "h", runs.StatusSuccess, started)
	titled.Title = "wow-look-at-my/go-toolchain#47"
	titled.StartedAt = started.Add(time.Second)
	require.NoError(t, s.Record(titled))

	got, ok := s.Get(titled.ID)
	require.True(t, ok)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", got.Title)

	list := s.ListByHook("h", 0)
	require.Len(t, list, 1)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", list[0].Title,
		"history reads must return titles")

	// The index value is the SAME bytes a title-less run would produce — the title must never leak into the summary format that SummariesByHook.
	want := summaryValue(titled.Status, titled.Finished, titled.StartedAt)
	require.NoError(t, s.db.View(func(tx *bolt.Tx) error {
		hb := tx.Bucket(bucketByHook).Bucket([]byte("h"))
		_, v := hb.Cursor().First()
		assert.Equal(t, string(want), string(v))
		return nil
	}))

	// And the meta blob carries it under the additive "title" key.
	require.NoError(t, s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte(titled.ID))
		var blob map[string]any
		require.NoError(t, json.Unmarshal(raw, &blob))
		assert.Equal(t, "wow-look-at-my/go-toolchain#47", blob["title"])
		return nil
	}))
}

// An untitled run's blob has NO title key at all (omitempty) — the field is
// additive, so old readers of the blob see exactly the old shape.
func TestRecordUntitledOmitsTitleKey(t *testing.T) {
	s := newStore(t, Config{})
	st := state("untitleduntitleduntitledun", "h", runs.StatusSuccess, time.Now().UTC().Add(-time.Minute))
	require.NoError(t, s.Record(st))

	require.NoError(t, s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte(st.ID))
		var blob map[string]any
		require.NoError(t, json.Unmarshal(raw, &blob))
		_, present := blob["title"]
		assert.False(t, present)
		return nil
	}))
}

// A pre-title database row — a metadata blob written before the field
// existed, with the legacy two-field index value beside it — reads back
// title-less without error, on Get, the list walks, and the summaries.
func TestPreTitleRowsReadBackTitleless(t *testing.T) {
	s := newStore(t, Config{})
	st := state("legacylegacylegacylegacyle", "h", runs.StatusSuccess, time.Now().UTC().Add(-time.Minute))
	require.NoError(t, s.Record(st))

	// Rewrite the row in place to its pre-title generation: a hand-built
	// blob without the "title" key (what old binaries marshaled) and the
	// legacy two-field summary value.
	require.NoError(t, s.db.Update(func(tx *bolt.Tx) error {
		blob, err := json.Marshal(struct {
			ID       string `json:"id"`
			HookID   string `json:"hook_id"`
			Started  string `json:"started"`
			Finished string `json:"finished"`
			Status   string `json:"status"`
			ExitCode int    `json:"exit_code"`
		}{
			ID:       st.ID,
			HookID:   "h",
			Started:  st.Started.Format(time.RFC3339Nano),
			Finished: st.Finished.Format(time.RFC3339Nano),
			Status:   "success",
			ExitCode: 0,
		})
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketMeta).Put([]byte(st.ID), blob); err != nil {
			return err
		}
		hb := tx.Bucket(bucketByHook).Bucket([]byte("h"))
		k, _ := hb.Cursor().First()
		return hb.Put(append([]byte(nil), k...), fmt.Appendf(nil, "%s %d", st.Status, st.Finished.UnixNano()))
	}))

	got, ok := s.Get(st.ID)
	require.True(t, ok)
	assert.Empty(t, got.Title)
	assert.Equal(t, runs.StatusSuccess, got.Status)

	list := s.ListByHook("h", 0)
	require.Len(t, list, 1)
	assert.Empty(t, list[0].Title)

	sums := s.SummariesByHook("h")
	require.Len(t, sums, 1)
	assert.Equal(t, st.ID, sums[0].ID)
}
