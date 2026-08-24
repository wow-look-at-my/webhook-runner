package attention

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedClock returns an aggregator whose clock the test controls, plus the
// tick function advancing it.
func fixedClock() (*Aggregator, func(d time.Duration), func() time.Time) {
	a := New()
	var mu sync.Mutex
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	tick := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	return a, tick, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
}

// The core lifecycle: a problem appears with a bad load, persists across
// re-derivations, and CLEARS on the reload where the underlying state is
// fixed — no manual acknowledgement anywhere.
func TestReplaceSourceLifecycle(t *testing.T) {
	a, _, _ := fixedClock()
	a.ReplaceSource(SourceLoad, []Entry{{Hook: "broken", Key: KeyLoad, Message: "decode hook.json: boom"}})
	require.Equal(t, 1, a.Count())
	got := a.Snapshot()
	require.Len(t, got, 1)
	assert.Equal(t, SourceLoad, got[0].Source, "ReplaceSource must stamp its source")
	assert.Equal(t, "broken", got[0].Hook)

	// The good reload: the loader no longer reports the hook → entry gone.
	a.ReplaceSource(SourceLoad, nil)
	assert.Equal(t, 0, a.Count())
	assert.Empty(t, a.Snapshot())
}

// Since is the FIRST time the problem became active: stable across
// re-derivations (even when the message morphs), reset only when the
// problem clears and later recurs.
func TestSinceStableAcrossRederivationsResetsOnRecur(t *testing.T) {
	a, tick, now := fixedClock()
	first := now()
	a.ReplaceSource(SourceLoad, []Entry{{Hook: "h", Key: KeyLoad, Message: "reason one"}})

	tick(5 * time.Minute)
	a.ReplaceSource(SourceLoad, []Entry{{Hook: "h", Key: KeyLoad, Message: "reason one"}})
	require.Len(t, a.Snapshot(), 1)
	assert.Equal(t, first, a.Snapshot()[0].Since, "an unchanged problem keeps its Since")

	// The message morphs; Since still marks when the hook first broke.
	tick(5 * time.Minute)
	a.ReplaceSource(SourceLoad, []Entry{{Hook: "h", Key: KeyLoad, Message: "reason two"}})
	require.Len(t, a.Snapshot(), 1)
	assert.Equal(t, "reason two", a.Snapshot()[0].Message)
	assert.Equal(t, first, a.Snapshot()[0].Since, "a reworded problem keeps its Since")

	// Clears, then recurs: a NEW problem, a new Since.
	tick(5 * time.Minute)
	a.ReplaceSource(SourceLoad, nil)
	tick(5 * time.Minute)
	recur := now()
	a.ReplaceSource(SourceLoad, []Entry{{Hook: "h", Key: KeyLoad, Message: "reason one"}})
	require.Len(t, a.Snapshot(), 1)
	assert.Equal(t, recur, a.Snapshot()[0].Since, "clear + recur resets Since")
}

// onChange fires only on REAL set changes — a reload that re-derives the
// identical problem set must not re-push the attention section.
func TestOnChangeFiresOnlyOnRealChange(t *testing.T) {
	a, _, _ := fixedClock()
	var calls int
	a.SetOnChange(func() { calls++ })

	set := []Entry{{Hook: "h", Key: KeyLoad, Message: "m"}}
	a.ReplaceSource(SourceLoad, set)
	assert.Equal(t, 1, calls, "first derivation is a change")

	a.ReplaceSource(SourceLoad, set)
	assert.Equal(t, 1, calls, "identical re-derivation is not")

	a.ReplaceSource(SourceLoad, []Entry{{Hook: "h", Key: KeyLoad, Message: "m2"}})
	assert.Equal(t, 2, calls, "a reworded entry is")

	a.ReplaceSource(SourceLoad, nil)
	assert.Equal(t, 3, calls, "a clear is")
	a.ReplaceSource(SourceLoad, nil)
	assert.Equal(t, 3, calls, "an empty no-op is not")

	a.Report(Entry{Source: SourceServer, Key: KeyTmpDir, Message: "x"})
	assert.Equal(t, 4, calls)
	a.Report(Entry{Source: SourceServer, Key: KeyTmpDir, Message: "x"})
	assert.Equal(t, 4, calls, "an identical Report is not")
	a.Resolve(SourceServer, "", KeyTmpDir)
	assert.Equal(t, 5, calls)
	a.Resolve(SourceServer, "", KeyTmpDir)
	assert.Equal(t, 5, calls, "resolving an absent entry is not")
}

// ReplaceSource must only touch ITS source: the boot-scoped server verdict
// and event-derived entries survive every reload re-derivation.
func TestReplaceSourceLeavesOtherSourcesAlone(t *testing.T) {
	a, _, _ := fixedClock()
	a.Report(Entry{Source: SourceServer, Key: KeyTmpDir, Message: "tmpdir hazard"})
	a.Report(Entry{Source: SourceEvent, Hook: "h", Key: KeyAPIKey, Message: "denied"})
	a.ReplaceSource(SourceLoad, []Entry{{Hook: "x", Key: KeyLoad, Message: "m"}})
	a.ReplaceSource(SourceLoad, nil)
	assert.Equal(t, 2, a.Count(), "server + event entries must survive load re-derivations")
}

// The event seam end to end: recognized kinds create entries, the paired
// all-clear resolves them, unknown kinds are ignored.
func TestObserveEventStandardRules(t *testing.T) {
	a, _, _ := fixedClock()
	RegisterStandardEventRules(a)

	// Unrecognized kinds: no-ops.
	a.ObserveEvent("run.finished", "h", "whatever")
	a.ObserveEvent("hooks.reloaded", "", "5 hooks")
	assert.Equal(t, 0, a.Count())

	// The request-time api_key denial (recorded by auth.go today).
	a.ObserveEvent(KindHookMisconfigured, "h",
		"h: api_key reference ${NOPE} did not resolve (secrets.sops.env / host env); all callers are denied")
	require.Equal(t, 1, a.Count())
	e := a.Snapshot()[0]
	assert.Equal(t, SourceEvent, e.Source)
	assert.Equal(t, "h", e.Hook)
	assert.Equal(t, KeyAPIKey, e.Key)
	assert.Contains(t, e.Message, "${NOPE}")
	assert.NotContains(t, e.Message, "h: api_key", "the hook prefix is stripped — the Hook field carries it")

	// A repeat keeps one entry (same identity).
	a.ObserveEvent(KindHookMisconfigured, "h",
		"h: api_key reference ${NOPE} did not resolve (secrets.sops.env / host env); all callers are denied")
	assert.Equal(t, 1, a.Count())

	// One entry per distinct message, cleared by the hook's healthy signal.
	a.ObserveEvent(KindHookReported, "g", "missing permission: contents write")
	a.ObserveEvent(KindHookReported, "g", "feature inert: auto_merge label not found")
	a.ObserveEvent(KindHookReported, "g", "missing permission: contents write") // repeat: same identity
	assert.Equal(t, 3, a.Count())
	a.ObserveEvent(KindHookReportResolved, "other-hook", "")
	assert.Equal(t, 3, a.Count(), "another hook's all-clear must not touch g's entries")
	a.ObserveEvent(KindHookReportResolved, "g", "")
	assert.Equal(t, 1, a.Count(), "the hook's all-clear resolves every reported entry")
	assert.Equal(t, KeyAPIKey, a.Snapshot()[0].Key, "h's api_key event entry is untouched")

	// Hook-less events of recognized kinds are dropped.
	a.ObserveEvent(KindHookReported, "", "no hook field")
	assert.Equal(t, 1, a.Count())
}

// A nil aggregator is valid everywhere (the events.Recorder convention) —
// wiring paths and tests never need nil checks.
func TestNilAggregatorIsSafe(t *testing.T) {
	var a *Aggregator
	a.SetOnChange(func() {})
	a.ReplaceSource(SourceLoad, []Entry{{Key: "k"}})
	a.Report(Entry{Key: "k"})
	a.Resolve(SourceLoad, "", "k")
	a.RegisterEventRule("kind", func(_, _ string) ([]Entry, []Resolution) { return nil, nil })
	a.ObserveEvent("kind", "h", "m")
	assert.Equal(t, 0, a.Count())
	assert.NotNil(t, a.Snapshot(), "Snapshot must return an empty slice, never nil (it is marshaled)")
	assert.Empty(t, a.Snapshot())
}

// Snapshot orders oldest-first (the longest-standing problem leads the
// panel), deterministically tie-broken.
func TestSnapshotSortedOldestFirst(t *testing.T) {
	a, tick, _ := fixedClock()
	a.Report(Entry{Source: SourceServer, Key: KeyTmpDir, Message: "old"})
	tick(time.Minute)
	a.Report(Entry{Source: SourceEvent, Hook: "b", Key: "k", Message: "mid"})
	tick(time.Minute)
	a.Report(Entry{Source: SourceEvent, Hook: "a", Key: "k", Message: "new"})
	got := a.Snapshot()
	require.Len(t, got, 3)
	assert.Equal(t, "old", got[0].Message)
	assert.Equal(t, "mid", got[1].Message)
	assert.Equal(t, "new", got[2].Message)

	// Equal Since: source, then hook, then key ("event" < "load" < …).
	b, _, _ := fixedClock()
	b.Report(Entry{Source: SourceEvent, Hook: "z", Key: "k", Message: "1"})
	b.Report(Entry{Source: SourceEvent, Hook: "a", Key: "k", Message: "2"})
	b.Report(Entry{Source: SourceLoad, Hook: "m", Key: KeyLoad, Message: "3"})
	tied := b.Snapshot()
	require.Len(t, tied, 3)
	assert.Equal(t, []string{"2", "1", "3"},
		[]string{tied[0].Message, tied[1].Message, tied[2].Message})
}

// Concurrent mutation and reads must be race-free (the race detector is
// the real assertion here).
func TestConcurrentSafety(t *testing.T) {
	a := New()
	RegisterStandardEventRules(a)
	a.SetOnChange(func() {})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			hook := fmt.Sprintf("h%d", n%3)
			for j := 0; j < 200; j++ {
				switch j % 5 {
				case 0:
					a.ReplaceSource(SourceLoad, []Entry{{Hook: hook, Key: KeyLoad, Message: "m"}})
				case 1:
					a.Report(Entry{Source: SourceEvent, Hook: hook, Key: "k", Message: "m"})
				case 2:
					a.ObserveEvent(KindHookMisconfigured, hook, hook+": api_key reference ${X} did not resolve")
				case 3:
					a.Resolve(SourceEvent, hook, "k")
				default:
					a.Snapshot()
					a.Count()
				}
			}
		}(i)
	}
	wg.Wait()
}
