package runs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOutputBounded(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	for i := 0; i < MaxOutputLines+50; i++ {
		r.AppendOutput("line")
	}
	got := len(r.Snapshot(-1).Output)
	assert.Equal(t, MaxOutputLines, got)
}

func TestNewIDUniqueAndShape(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := newID()
		require.Equal(t, 26, len(id))
		assert.False(t, strings.ContainsAny(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
		_, dup := seen[id]
		assert.False(t, dup)
		seen[id] = struct{}{}
	}
}

func TestFinishIsTerminal(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.Finish(StatusSuccess, 0, "")
	r.Finish(StatusFailure, 1, "")
	assert.Equal(t, StatusSuccess, r.Status())
	assert.Equal(t, 0, r.ExitCode())
	select {
	case <-r.Done():
	default:
		t.Errorf("Done channel not closed")
	}
}

func TestFinishWithError(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.Finish(StatusError, -1, "boom")
	assert.Equal(t, "boom", r.Error())
}

func TestPerHookEviction(t *testing.T) {
	tr := NewTracker()
	tr.maxByHook = 3
	for i := 0; i < 10; i++ {
		tr.New("h")
	}
	got := len(tr.ListByHook("h", 0))
	assert.Equal(t, 3, got)
}

func TestSnapshotTail(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	for _, line := range []string{"a", "b", "c", "d"} {
		r.AppendOutput(line)
	}
	assert.Equal(t, []string{"c", "d"}, r.Snapshot(2).Output)
	assert.Equal(t, []string{"a", "b", "c", "d"}, r.Snapshot(-1).Output)
	assert.Empty(t, r.Snapshot(0).Output)
}

func TestLastLines(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	assert.Empty(t, r.LastLines(5))
	r.AppendOutput("a")
	r.AppendOutput("b")
	r.AppendOutput("c")
	assert.Equal(t, []string{"b", "c"}, r.LastLines(2))
	assert.Equal(t, []string{"a", "b", "c"}, r.LastLines(10))
	assert.Empty(t, r.LastLines(0))
}

func TestSetRunningOnlyFromPending(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	assert.Equal(t, StatusPending, r.Status())
	r.SetRunning()
	assert.Equal(t, StatusRunning, r.Status())

	r.Finish(StatusSuccess, 0, "")
	r.SetRunning() // must not regress
	assert.Equal(t, StatusSuccess, r.Status())
}

// SetRunning stamps StartedAt exactly once, at the pending→running
// transition — the queue-wait/processing split point. A run that never
// starts keeps a zero StartedAt.
func TestSetRunningStampsStartedAt(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	assert.True(t, r.StartedAt().IsZero(), "a pending run has not started")
	assert.True(t, r.Snapshot(0).StartedAt.IsZero())

	r.SetRunning()
	startedAt := r.StartedAt()
	require.False(t, startedAt.IsZero(), "SetRunning must stamp StartedAt")
	assert.False(t, startedAt.Before(r.Started()), "processing cannot begin before the run was queued")

	r.SetRunning() // ineffective second call must not restamp
	assert.True(t, r.StartedAt().Equal(startedAt))

	r.Finish(StatusSuccess, 0, "")
	assert.True(t, r.Snapshot(0).StartedAt.Equal(startedAt))

	// Cancelled while pending: never started, StartedAt stays zero through
	// the terminal snapshot (the dashboard shows no duration for it).
	never := tr.New("h")
	never.Finish(StatusCancelled, -1, "cancelled before start")
	assert.True(t, never.Snapshot(0).StartedAt.IsZero())
}

func TestTrackerListAll(t *testing.T) {
	tr := NewTracker()
	tr.New("a")
	tr.New("b")
	tr.New("c")
	all := tr.ListAll(0)
	assert.Len(t, all, 3)
	limited := tr.ListAll(2)
	assert.Len(t, limited, 2)
}

func TestTrackerGet(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	got := tr.Get(r.ID())
	require.NotNil(t, got)
	assert.Equal(t, r.ID(), got.ID())
	assert.Nil(t, tr.Get("missing"))
}

func TestRunGetters(t *testing.T) {
	tr := NewTracker()
	r := tr.New("hook-a")
	assert.Equal(t, "hook-a", r.HookID())
	assert.False(t, r.Started().IsZero())
	assert.NotEmpty(t, r.ID())
}

func TestAppendOutputTrimsNewline(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.AppendOutput("hello\n")
	r.AppendOutput("world\r\n")
	assert.Equal(t, []string{"hello", "world"}, r.Snapshot(-1).Output)
}

// OutputTimes must stay 1:1 with Output through append, ring eviction, and
// tail slicing — the dashboard zips the two by index, so a length mismatch
// would misalign every timestamp.
func TestOutputTimesTrackOutput(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")

	before := time.Now().UTC()
	for _, line := range []string{"a", "b", "c", "d"} {
		r.AppendOutput(line)
	}
	after := time.Now().UTC()

	full := r.Snapshot(-1)
	require.Len(t, full.OutputTimes, len(full.Output))
	for _, ts := range full.OutputTimes {
		assert.False(t, ts.Before(before), "timestamp predates the appends")
		assert.False(t, ts.After(after), "timestamp postdates the appends")
	}

	// Tail slices both halves together.
	tail := r.Snapshot(2)
	assert.Equal(t, []string{"c", "d"}, tail.Output)
	require.Len(t, tail.OutputTimes, 2)
	assert.Equal(t, full.OutputTimes[2:], tail.OutputTimes)

	// Eviction drops from both halves together.
	r2 := tr.New("h")
	for i := 0; i < MaxOutputLines+50; i++ {
		r2.AppendOutput("line")
	}
	snap := r2.Snapshot(-1)
	assert.Len(t, snap.Output, MaxOutputLines)
	assert.Len(t, snap.OutputTimes, MaxOutputLines)
}

func TestRequestCancel(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")

	select {
	case <-r.Cancelled():
		t.Fatal("Cancelled closed before any request")
	default:
	}

	r.RequestCancel()
	r.RequestCancel() // idempotent — a second request must not re-close the channel

	select {
	case <-r.Cancelled():
	default:
		t.Error("Cancelled channel not closed after RequestCancel")
	}
	assert.True(t, r.Snapshot(0).CancelRequested)

	// Cancel only signals; the run is finished by the runner.
	assert.Equal(t, StatusPending, r.Status())
	r.Finish(StatusCancelled, -1, "cancelled")
	assert.Equal(t, StatusCancelled, r.Status())
}

// The OnFinish observer is the run store's write-once seam: it must fire
// exactly once per run (Finish's once-guard), with the full terminal
// snapshot including output.
func TestOnFinishFiresOnceWithTerminalSnapshot(t *testing.T) {
	tr := NewTracker()
	var got []RunState
	tr.SetOnFinish(func(st RunState) { got = append(got, st) })

	r := tr.New("h")
	r.AppendOutput("hello")
	assert.Empty(t, got, "observer fired before the run finished")

	r.Finish(StatusFailure, 2, "boom")
	r.Finish(StatusSuccess, 0, "") // second Finish is a no-op — no second callback
	require.Len(t, got, 1)
	assert.Equal(t, r.ID(), got[0].ID)
	assert.Equal(t, "h", got[0].HookID)
	assert.Equal(t, StatusFailure, got[0].Status)
	assert.Equal(t, 2, got[0].ExitCode)
	assert.Equal(t, "boom", got[0].Error)
	assert.False(t, got[0].Finished.IsZero())
	assert.Equal(t, []string{"hello"}, got[0].Output)

	// Runs created before SetOnFinish never see the callback.
	tr2 := NewTracker()
	early := tr2.New("h")
	tr2.SetOnFinish(func(RunState) { t.Error("callback leaked to a pre-existing run") })
	early.Finish(StatusSuccess, 0, "")
}

func TestStatusTerminal(t *testing.T) {
	for _, s := range []Status{StatusSuccess, StatusFailure, StatusTimeout, StatusError, StatusCancelled} {
		assert.True(t, s.Terminal(), string(s))
	}
	for _, s := range []Status{StatusPending, StatusRunning} {
		assert.False(t, s.Terminal(), string(s))
	}
}

func TestSetClearWaitingOn(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	until := time.Now().UTC().Add(45 * time.Second)

	seq := r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "green-settle", Until: until})
	require.NotZero(t, seq)
	snap := r.Snapshot(-1)
	require.NotNil(t, snap.WaitingOn)
	assert.Equal(t, WaitingOnWait, snap.WaitingOn.Kind)
	assert.Equal(t, "green-settle", snap.WaitingOn.Reason)
	assert.Equal(t, until, snap.WaitingOn.Until)

	r.ClearWaitingOn(seq)
	assert.Nil(t, r.Snapshot(-1).WaitingOn)
}

// waiting_on rides the documented JSON names while a pause is active and
// vanishes entirely (omitempty) once it ends — the list and detail endpoints
// ship RunState verbatim, so this IS the dashboard contract. Both kinds are
// exercised: a declared sleep and a blocked lock acquire naming its holder.
func TestWaitingOnJSON(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	seq := r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "green-settle", Until: time.Now().UTC().Add(30 * time.Second)})

	b, err := json.Marshal(r.Snapshot(-1))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"waiting_on":{"kind":"wait"`)
	assert.Contains(t, string(b), `"reason":"green-settle"`)
	assert.Contains(t, string(b), `"until"`)

	seq = r.SetWaitingOn(WaitingOn{Kind: WaitingOnLock, Key: "pr-7", HolderRunID: "holderrun", HolderHookID: "h"})
	b, err = json.Marshal(r.Snapshot(-1))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"waiting_on":{"kind":"lock"`)
	assert.Contains(t, string(b), `"key":"pr-7"`)
	assert.Contains(t, string(b), `"holder_run_id":"holderrun"`)
	assert.Contains(t, string(b), `"holder_hook_id":"h"`)

	r.ClearWaitingOn(seq)
	b, err = json.Marshal(r.Snapshot(-1))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "waiting_on")

	// Waiters marshals under its documented name too (it is server-derived,
	// so set it on a detached snapshot the way the read path does).
	snap := r.Snapshot(-1)
	snap.Waiters = []Waiter{{RunID: "w1", HookID: "h", Key: "pr-7"}}
	b, err = json.Marshal(snap)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"waiters":[{"run_id":"w1","hook_id":"h","key":"pr-7"}]`)
}

// A stale ClearWaitingOn — from a pause that a newer one overlapped — must
// not clear the newer pause's state; only the current sequence token does.
func TestClearWaitingOnIgnoresStaleSequence(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	seq1 := r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "first", Until: time.Now().Add(time.Minute)})
	seq2 := r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "second", Until: time.Now().Add(2 * time.Minute)})
	require.NotEqual(t, seq1, seq2)

	r.ClearWaitingOn(seq1) // stale: the second pause's state stays
	snap := r.Snapshot(-1)
	require.NotNil(t, snap.WaitingOn)
	assert.Equal(t, "second", snap.WaitingOn.Reason)

	r.ClearWaitingOn(seq2)
	assert.Nil(t, r.Snapshot(-1).WaitingOn)
}

// A terminal run is never waiting: Finish clears in-flight pause state, so
// the OnFinish snapshot (what the run store persists) never carries it, and
// a SetWaitingOn after the fact is inert.
func TestFinishClearsWaitingOn(t *testing.T) {
	tr := NewTracker()
	var persisted RunState
	tr.SetOnFinish(func(st RunState) { persisted = st })
	r := tr.New("h")

	r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "about to die", Until: time.Now().Add(time.Minute)})
	r.Finish(StatusTimeout, -1, "timed out")

	assert.Nil(t, persisted.WaitingOn)
	assert.Nil(t, r.Snapshot(-1).WaitingOn)

	assert.Zero(t, r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "too late"}))
	assert.Nil(t, r.Snapshot(-1).WaitingOn)
}

// A cancel can carry a reason (e.g. "lock stolen by run X"); only the first
// one sticks, and a plain RequestCancel carries none.
func TestRequestCancelWithReason(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	assert.Empty(t, r.CancelReason())

	r.RequestCancelWithReason("displaced by a steal")
	assert.Equal(t, "displaced by a steal", r.CancelReason())
	r.RequestCancelWithReason("second reason loses")
	assert.Equal(t, "displaced by a steal", r.CancelReason())

	r2 := tr.New("h")
	r2.RequestCancel()
	assert.Empty(t, r2.CancelReason())
}

func TestTouchActivity(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.TouchActivity() // no touch registered: a safe no-op

	var touches int
	r.SetActivityTouch(func() { touches++ })
	r.TouchActivity()
	r.TouchActivity()
	assert.Equal(t, 2, touches)
}

// Group-wait fields ride WaitingOn additively: present when stamped,
// omitted from JSON when empty, and deep-copied by Snapshot.
func TestWaitingOnGroupFieldsJSONAndCopy(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	holders := []string{"run-1", "run-2"}
	r.SetWaitingOn(WaitingOn{Kind: WaitingOnGroup, Key: "model-gateway", HolderRunIDs: holders, Position: 3})

	snap := r.Snapshot(0)
	require.NotNil(t, snap.WaitingOn)
	// Mutating the caller's slice must not reach the snapshot (deep copy).
	holders[0] = "mutated"
	assert.Equal(t, []string{"run-1", "run-2"}, snap.WaitingOn.HolderRunIDs)

	b, err := json.Marshal(snap.WaitingOn)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"holder_run_ids":["run-1","run-2"]`)
	assert.Contains(t, string(b), `"position":3`)

	// A lock wait (no group fields) omits them.
	r2 := tr.New("h")
	r2.SetWaitingOn(WaitingOn{Kind: WaitingOnLock, Key: "k", HolderRunID: "x"})
	b2, err := json.Marshal(r2.Snapshot(0).WaitingOn)
	require.NoError(t, err)
	assert.NotContains(t, string(b2), "holder_run_ids")
	assert.NotContains(t, string(b2), "position")
}

// The OnChange seam: one notification per observable mutation, in order,
// with output stripped; no-op mutations stay silent; nil-safe.
func TestTrackerOnChangeNotifications(t *testing.T) {
	tr := NewTracker()
	var got []RunState
	tr.SetOnChange(func(st RunState) { got = append(got, st) })

	r := tr.New("h") // 1: created (pending)
	r.AppendOutput("line 1")
	r.SetRunning()                                                       // 2: running
	r.SetRunning()                                                       // no-op: already running
	seq := r.SetWaitingOn(WaitingOn{Kind: WaitingOnWait, Reason: "zzz"}) // 3
	r.ClearWaitingOn(0)                                                  // no-op: zero token
	r.ClearWaitingOn(seq)                                                // 4: cleared
	r.ClearWaitingOn(seq)                                                // no-op: already cleared
	r.SetTitle("")                                                       // no-op: empty
	r.SetTitle("owner/repo#1")                                           // 5: titled
	r.RequestCancel()                                                    // 6: cancel requested
	r.RequestCancel()                                                    // no-op: second request
	r.Finish(StatusCancelled, -1, "cancelled")                           // 7: terminal
	r.Finish(StatusSuccess, 0, "")                                       // no-op: already finished
	r.SetTitle("late")                                                   // no-op: finished

	require.Len(t, got, 7)
	assert.Equal(t, StatusPending, got[0].Status)
	assert.Equal(t, StatusRunning, got[1].Status)
	require.NotNil(t, got[2].WaitingOn)
	assert.Equal(t, "zzz", got[2].WaitingOn.Reason)
	assert.Nil(t, got[3].WaitingOn)
	assert.Equal(t, "owner/repo#1", got[4].Title)
	assert.True(t, got[5].CancelRequested)
	assert.Equal(t, StatusCancelled, got[6].Status)
	assert.False(t, got[6].Finished.IsZero())
	for i, st := range got {
		assert.Empty(t, st.Output, "notification %d must strip output", i)
		assert.Equal(t, r.ID(), st.ID)
	}

	// nil observer (the default): everything above is a no-op, not a panic.
	tr2 := NewTracker()
	r2 := tr2.New("h")
	r2.SetRunning()
	r2.Finish(StatusSuccess, 0, "")
}

// The terminal notification fires AFTER the OnFinish persistence seam.
func TestOnChangeTerminalAfterOnFinish(t *testing.T) {
	tr := NewTracker()
	var order []string
	tr.SetOnFinish(func(RunState) { order = append(order, "finish") })
	tr.SetOnChange(func(st RunState) {
		if st.Status.Terminal() {
			order = append(order, "change")
		}
	})
	r := tr.New("h")
	r.Finish(StatusSuccess, 0, "")
	assert.Equal(t, []string{"finish", "change"}, order)
}
