package runs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetSpawnedBy(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")

	// Half-empty attributions are no-ops: unspawned runs stay unattributed.
	r.SetSpawnedBy("", "prun")
	r.SetSpawnedBy("parent", "")
	assert.Nil(t, r.SpawnedBy())

	r.SetSpawnedBy("parent", "prun")
	sb := r.SpawnedBy()
	require.NotNil(t, sb)
	assert.Equal(t, "parent", sb.HookID)
	assert.Equal(t, "prun", sb.RunID)

	// Snapshots copy, never alias: mutating a snapshot's attribution must
	// not reach the live run (the WaitingOn rule).
	snap := r.Snapshot(0)
	require.NotNil(t, snap.SpawnedBy)
	snap.SpawnedBy.RunID = "mutated"
	assert.Equal(t, "prun", r.SpawnedBy().RunID)
	// The getter copies too.
	sb.RunID = "also-mutated"
	assert.Equal(t, "prun", r.SpawnedBy().RunID)
}

func TestSetSpawnedByRefusesFinishedRun(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.Finish(StatusSuccess, 0, "")
	// The SetTitle rule: the terminal snapshot already persisted, so a late
	// stamp would diverge live state from history.
	r.SetSpawnedBy("parent", "prun")
	assert.Nil(t, r.SpawnedBy())
	assert.Nil(t, r.Snapshot(0).SpawnedBy)
}
