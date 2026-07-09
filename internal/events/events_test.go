package events

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecorderOrderAndBound(t *testing.T) {
	r := NewRecorder(3)
	for i := 1; i <= 5; i++ {
		r.Record("k", fmt.Sprintf("msg-%d", i), nil)
	}
	got := r.List(0)
	assert.Len(t, got, 3)
	assert.Equal(t, "msg-5", got[0].Msg) // newest first
	assert.Equal(t, "msg-4", got[1].Msg)
	assert.Equal(t, "msg-3", got[2].Msg)

	limited := r.List(2)
	assert.Len(t, limited, 2)
	assert.Equal(t, "msg-5", limited[0].Msg)
}

func TestRecorderFields(t *testing.T) {
	r := NewRecorder(10)
	r.Record("image.built", "built x", map[string]string{"hook": "h"})
	got := r.List(1)
	assert.Equal(t, "image.built", got[0].Kind)
	assert.Equal(t, "h", got[0].Fields["hook"])
	assert.False(t, got[0].Time.IsZero())
}

func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Record("k", "m", nil) // must not panic
	assert.Nil(t, r.List(5))
	assert.Nil(t, r.ListByHook("h", 5))
}

func TestListByHook(t *testing.T) {
	r := NewRecorder(10)
	r.Record("run.started", "a run 1", map[string]string{"hook": "a"})
	r.Record("reload.requested", "global", nil) // no hook field: never matches
	r.Record("run.finished", "b run 1", map[string]string{"hook": "b"})
	r.Record("run.started", "a run 2", map[string]string{"hook": "a"})

	got := r.ListByHook("a", 0)
	assert.Len(t, got, 2)
	assert.Equal(t, "a run 2", got[0].Msg) // newest first
	assert.Equal(t, "a run 1", got[1].Msg)

	limited := r.ListByHook("a", 1)
	assert.Len(t, limited, 1)
	assert.Equal(t, "a run 2", limited[0].Msg)

	assert.Empty(t, r.ListByHook("missing", 0))
}

// The bound applies to matches, not scanned entries: a hook's events must
// survive being interleaved with (or displaced past) other hooks' noise up
// to the ring's own retention.
func TestListByHookScansWholeRing(t *testing.T) {
	r := NewRecorder(6)
	r.Record("run.started", "mine", map[string]string{"hook": "a"})
	for i := 0; i < 5; i++ {
		r.Record("run.started", "noise", map[string]string{"hook": "b"})
	}
	got := r.ListByHook("a", 3)
	assert.Len(t, got, 1)
	assert.Equal(t, "mine", got[0].Msg)
}
