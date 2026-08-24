package events

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestFamily(t *testing.T) {
	assert.Equal(t, "run", Family("run.started"))
	assert.Equal(t, "image", Family("image.build_failed"))
	// A dotless kind is its own family, and a family must not match by bare prefix: excluding "run" cannot take "runstore.*" with it.
	assert.Equal(t, "k", Family("k"))
	assert.Equal(t, "runstore", Family("runstore.compacted"))
}

func TestExcludeFamiliesDropsWholeFamily(t *testing.T) {
	r := NewRecorder(20)
	r.Record("run.started", "started", map[string]string{"hook": "a"})
	r.Record("hook.denied", "bad signature", map[string]string{"hook": "a"})
	r.Record("run.finished", "finished", map[string]string{"hook": "a"})
	r.Record("image.built", "built", map[string]string{"hook": "a"})
	r.Record("runstore.compacted", "compacted", nil)

	got := r.ListFiltered(Filter{ExcludeFamilies: []string{"run"}}, 0)
	kinds := make([]string, 0, len(got))
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
	}
	// runstore is NOT the run family — a bare prefix match would eat it.
	assert.Equal(t, []string{"runstore.compacted", "image.built", "hook.denied"}, kinds)
}

// The bug this ordering prevents, in the shape it actually appears: a flood
// of excluded events sits in front of the entries the operator needs. Cap
// first and the page is all run events, the filter empties it, and the feed
// reports "nothing here" while the matches sit just behind them in the ring.
func TestExcludeFiltersBeforeTheCap(t *testing.T) {
	r := NewRecorder(200)
	r.Record("hook.denied", "oldest denial", map[string]string{"hook": "a"})
	r.Record("image.built", "a build", map[string]string{"hook": "a"})
	for i := 0; i < 55; i++ {
		r.Record("run.skipped", "skip", map[string]string{"hook": "a"})
	}

	// A cap far smaller than the excluded burst in front of the matches.
	got := r.ListFiltered(Filter{Hook: "a", ExcludeFamilies: []string{"run"}}, 10)
	require.Len(t, got, 2, "the 55 excluded events must not consume the cap")
	assert.Equal(t, "a build", got[0].Msg)
	assert.Equal(t, "oldest denial", got[1].Msg)
}

// The cap still bounds what is RETURNED — filtering early must not turn
// max into a suggestion.
func TestExcludeStillHonorsTheCap(t *testing.T) {
	r := NewRecorder(50)
	for i := 0; i < 10; i++ {
		r.Record("run.started", "noise", map[string]string{"hook": "a"})
		r.Record("hook.denied", fmt.Sprintf("denial-%d", i), map[string]string{"hook": "a"})
	}
	got := r.ListFiltered(Filter{Hook: "a", ExcludeFamilies: []string{"run"}}, 3)
	assert.Len(t, got, 3)
	assert.Equal(t, "denial-9", got[0].Msg) // newest first
}

func TestFilterCombinesHookAndExclusion(t *testing.T) {
	r := NewRecorder(20)
	r.Record("hook.denied", "a denied", map[string]string{"hook": "a"})
	r.Record("hook.denied", "b denied", map[string]string{"hook": "b"})
	r.Record("run.started", "a run", map[string]string{"hook": "a"})

	got := r.ListFiltered(Filter{Hook: "a", ExcludeFamilies: []string{"run"}}, 0)
	require.Len(t, got, 1)
	assert.Equal(t, "a denied", got[0].Msg)
}

func TestNilRecorderIsSafeForFilteredListing(t *testing.T) {
	var r *Recorder
	assert.Nil(t, r.ListFiltered(Filter{ExcludeFamilies: []string{"run"}}, 5))
}
