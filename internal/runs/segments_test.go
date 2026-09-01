// Wait-history segments + the cancel-request timestamp: spans render their
// TRUE lifecycle, so every transition must be recorded at its real time.
package runs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitHistoryAccumulation(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")

	seq := r.SetWaitingOn(WaitingOn{Kind: WaitingOnGroup, Key: "model-gateway", Position: 1})
	time.Sleep(5 * time.Millisecond)
	r.ClearWaitingOn(seq)
	seq = r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "sleep"})
	r.ClearWaitingOn(seq)
	r.SetWaitingOn(WaitingOn{Kind: WaitingOnLock, Key: "pr:1"}) // left open

	st := r.Snapshot(0)
	require.Len(t, st.WaitHistory, 3)
	assert.Equal(t, WaitingOnGroup, st.WaitHistory[0].Kind)
	assert.Equal(t, "model-gateway", st.WaitHistory[0].Key)
	assert.False(t, st.WaitHistory[0].Start.IsZero())
	assert.False(t, st.WaitHistory[0].End.IsZero(), "cleared wait must be CLOSED at its real end")
	assert.True(t, st.WaitHistory[0].End.After(st.WaitHistory[0].Start))
	assert.False(t, st.WaitHistory[1].End.IsZero())
	assert.True(t, st.WaitHistory[2].End.IsZero(), "live wait stays open")

	// Finish closes the trailing open segment at the finish instant.
	r.Finish(StatusCancelled, -1, "cancelled")
	st = r.Snapshot(0)
	require.Len(t, st.WaitHistory, 3)
	assert.Equal(t, st.Finished, st.WaitHistory[2].End, "finish must close the open segment at Finished")
	assert.False(t, st.WaitHistoryTruncated)

	// Snapshot isolation: mutating the snapshot must not touch the run.
	st.WaitHistory[0].Key = "mutated"
	assert.Equal(t, "model-gateway", r.Snapshot(0).WaitHistory[0].Key)
}

func TestWaitHistoryCapAndTruncationFlag(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	for i := 0; i < MaxWaitSegments+5; i++ {
		seq := r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "tick"})
		r.ClearWaitingOn(seq)
	}
	st := r.Snapshot(0)
	assert.Len(t, st.WaitHistory, MaxWaitSegments)
	assert.True(t, st.WaitHistoryTruncated, "overflow must be flagged, never silent")
}

func TestCancelRequestedAtStampedOnce(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	assert.True(t, r.Snapshot(0).CancelRequestedAt.IsZero())
	r.RequestCancel()
	first := r.Snapshot(0).CancelRequestedAt
	require.False(t, first.IsZero())
	r.RequestCancel() // request: timestamp keeps the instant
	assert.Equal(t, first, r.Snapshot(0).CancelRequestedAt)

	b, err := json.Marshal(r.Snapshot(0))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"cancel_requested_at"`)
}

// Additive-JSON guarantee: a run that never waited or was cancelled emits
// none of the new fields.
func TestSegmentsFieldsOmittedWhenUnused(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.Finish(StatusSuccess, 0, "")
	b, err := json.Marshal(r.Snapshot(0))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "wait_history")
	assert.NotContains(t, string(b), "cancel_requested_at")
}

// logical wait = history entry: a re-stamp of the same kind+key (a
// queued group acquire whose position/holders changed, a blocked lock
// changing hands) continues the open segment instead of fragmenting it.
// Reproduced live before the fix: a single -deep queue wait shipped
// contiguous micro-segments on every SSE delta.
func TestSetWaitingOnRestampContinuesOpenSegment(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")

	// Join the queue at position , then advance through the line: same logical wait, restamped per queue movement.
	var seq uint64
	for pos := 7; pos >= 1; pos-- {
		seq = r.SetWaitingOn(WaitingOn{Kind: WaitingOnGroup, Key: "model-gateway", Position: pos,
			HolderRunIDs: []string{"a", "b", "c"}})
	}
	st := r.Snapshot(0)
	require.Len(t, st.WaitHistory, 1, "restamps of the same wait must not fragment the history")
	assert.True(t, st.WaitHistory[0].End.IsZero(), "the one segment is still open (still waiting)")
	require.NotNil(t, st.WaitingOn)
	assert.Equal(t, 1, st.WaitingOn.Position, "the live WaitingOn still tracks the newest restamp")

	// A DIFFERENT wait (other key) closes the segment and opens a new .
	seq = r.SetWaitingOn(WaitingOn{Kind: WaitingOnLock, Key: "pr:1"})
	st = r.Snapshot(0)
	require.Len(t, st.WaitHistory, 2)
	assert.False(t, st.WaitHistory[0].End.IsZero(), "the group wait closed when the lock wait began")
	assert.True(t, st.WaitHistory[1].End.IsZero())

	// Clear, then a NEW wait of the original kind+key: the previous segment is closed, so this is a fresh entry — continuation only applies to an.
	r.ClearWaitingOn(seq)
	r.SetWaitingOn(WaitingOn{Kind: WaitingOnGroup, Key: "model-gateway", Position: 3})
	st = r.Snapshot(0)
	require.Len(t, st.WaitHistory, 3)
	assert.False(t, st.WaitHistory[1].End.IsZero())
	assert.True(t, st.WaitHistory[2].End.IsZero())
}
