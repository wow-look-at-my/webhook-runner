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

func TestBeginEndWait(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	until := time.Now().UTC().Add(45 * time.Second)

	seq := r.BeginWait("green-settle", until)
	require.NotZero(t, seq)
	snap := r.Snapshot(-1)
	assert.True(t, snap.Waiting)
	assert.Equal(t, "green-settle", snap.WaitReason)
	assert.Equal(t, until, snap.WaitUntil)

	r.EndWait(seq)
	snap = r.Snapshot(-1)
	assert.False(t, snap.Waiting)
	assert.Empty(t, snap.WaitReason)
	assert.True(t, snap.WaitUntil.IsZero())
}

// The wait fields ride the documented JSON names while a wait is active and
// vanish entirely (omitempty/omitzero) once it ends — the list and detail
// endpoints ship RunState verbatim, so this IS the dashboard contract.
func TestWaitFieldsJSON(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	seq := r.BeginWait("green-settle", time.Now().UTC().Add(30*time.Second))

	b, err := json.Marshal(r.Snapshot(-1))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"waiting":true`)
	assert.Contains(t, string(b), `"wait_reason":"green-settle"`)
	assert.Contains(t, string(b), `"wait_until"`)

	r.EndWait(seq)
	b, err = json.Marshal(r.Snapshot(-1))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "waiting")
	assert.NotContains(t, string(b), "wait_reason")
	assert.NotContains(t, string(b), "wait_until")
}

// A stale EndWait — from a wait that a newer one overlapped — must not clear
// the newer wait's state; only the current sequence token does.
func TestEndWaitIgnoresStaleSequence(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	seq1 := r.BeginWait("first", time.Now().Add(time.Minute))
	seq2 := r.BeginWait("second", time.Now().Add(2*time.Minute))
	require.NotEqual(t, seq1, seq2)

	r.EndWait(seq1) // stale: the second wait's state stays
	snap := r.Snapshot(-1)
	assert.True(t, snap.Waiting)
	assert.Equal(t, "second", snap.WaitReason)

	r.EndWait(seq2)
	assert.False(t, r.Snapshot(-1).Waiting)
}

// A terminal run is never waiting: Finish clears in-flight wait state, so
// the OnFinish snapshot (what the run store persists) never carries it, and
// a BeginWait after the fact is inert.
func TestFinishClearsWaitState(t *testing.T) {
	tr := NewTracker()
	var persisted RunState
	tr.SetOnFinish(func(st RunState) { persisted = st })
	r := tr.New("h")

	r.BeginWait("about to die", time.Now().Add(time.Minute))
	r.Finish(StatusTimeout, -1, "timed out")

	assert.False(t, persisted.Waiting)
	assert.Empty(t, persisted.WaitReason)
	assert.True(t, persisted.WaitUntil.IsZero())
	assert.False(t, r.Snapshot(-1).Waiting)

	assert.Zero(t, r.BeginWait("too late", time.Now().Add(time.Minute)))
	assert.False(t, r.Snapshot(-1).Waiting)
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
