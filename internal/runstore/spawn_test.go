package runstore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Parent attribution rides the run's metadata BLOB — and only the blob,
// exactly like titles: Get and the list walks return it, while the per-hook
// index value keeps its exact "<status> <finished-nanos> <startedat-nanos>"
// format (SummariesByHook's contract), byte-identical to what an unspawned
// run writes.
func TestRecordSpawnedByRoundtrip(t *testing.T) {
	s := newStore(t, Config{})
	started := time.Now().UTC().Add(-time.Minute)
	spawned := state("spawnedspawnedspawnedspawn", "h", runs.StatusSuccess, started)
	spawned.SpawnedBy = &runs.SpawnedBy{RunID: "parentrunid", HookID: "parent-hook"}
	spawned.StartedAt = started.Add(time.Second)
	require.NoError(t, s.Record(spawned))

	got, ok := s.Get(spawned.ID)
	require.True(t, ok)
	require.NotNil(t, got.SpawnedBy)
	assert.Equal(t, "parentrunid", got.SpawnedBy.RunID)
	assert.Equal(t, "parent-hook", got.SpawnedBy.HookID)

	list := s.ListByHook("h", 0)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].SpawnedBy, "history reads must return the attribution")
	assert.Equal(t, "parentrunid", list[0].SpawnedBy.RunID)

	// The index value is the SAME bytes an unspawned run would produce —
	// the attribution must never leak into the summary format.
	want := summaryValue(spawned.Status, spawned.Finished, spawned.StartedAt)
	require.NoError(t, s.db.View(func(tx *bolt.Tx) error {
		hb := tx.Bucket(bucketByHook).Bucket([]byte("h"))
		_, v := hb.Cursor().First()
		assert.Equal(t, string(want), string(v))
		return nil
	}))

	// And the meta blob carries it under the additive "spawned_by" key.
	require.NoError(t, s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte(spawned.ID))
		var blob map[string]any
		require.NoError(t, json.Unmarshal(raw, &blob))
		assert.Equal(t, map[string]any{"run_id": "parentrunid", "hook_id": "parent-hook"}, blob["spawned_by"])
		return nil
	}))
}

// An unspawned run's blob has NO spawned_by key at all (omitempty) — the
// field is additive, so old readers of the blob see exactly the old shape.
func TestRecordUnspawnedOmitsSpawnedByKey(t *testing.T) {
	s := newStore(t, Config{})
	st := state("unspawnedunspawnedunspawne", "h", runs.StatusSuccess, time.Now().UTC().Add(-time.Minute))
	require.NoError(t, s.Record(st))

	require.NoError(t, s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte(st.ID))
		var blob map[string]any
		require.NoError(t, json.Unmarshal(raw, &blob))
		_, present := blob["spawned_by"]
		assert.False(t, present)
		return nil
	}))
}
