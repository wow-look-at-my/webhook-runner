package runs

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
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
	seen := set.New[string]()
	for i := 0; i < 1000; i++ {
		id := newID()
		require.Equal(t, 26, len(id))
		assert.False(t, strings.ContainsAny(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
		dup := seen.Contains(id)
		assert.False(t, dup)
		seen.Add(id)
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
		r := tr.New("h")
		r.Finish(StatusSuccess, 0, "")
	}
	got := len(tr.ListByHook("h", 0))
	assert.Equal(t, 3, got)
}

// The per-hook trim evicts oldest TERMINAL runs only: an active
// (non-terminal) run is the server's current truth and must survive any
// flood of newer runs — evicting one made GET /runs/{id} 404 while the
// container still ran (the runstore fallback holds terminal snapshots
// only) and cut live runs out of /runs windows.
func TestTrackerNeverEvictsActiveRuns(t *testing.T) {
	t.Run("oldest terminal evicted, older actives survive", func(t *testing.T) {
		tr := NewTracker()
		tr.maxByHook = 3
		t1 := tr.New("h")
		t1.Finish(StatusSuccess, 0, "")
		a1 := tr.New("h") // stays pending — must never be evicted
		t2 := tr.New("h")
		t2.Finish(StatusFailure, 1, "")
		a2 := tr.New("h")
		a2.SetRunning() // running — must never be evicted
		t3 := tr.New("h")
		t3.Finish(StatusSuccess, 0, "")
		t4 := tr.New("h")
		t4.Finish(StatusSuccess, 0, "")

		require.NotNil(t, tr.Get(a1.ID()), "active run evicted by newer runs")
		require.NotNil(t, tr.Get(a2.ID()), "active run evicted by newer runs")
		assert.NotNil(t, tr.Get(t4.ID()), "newest terminal run must be retained")
		assert.Nil(t, tr.Get(t1.ID()), "oldest terminal run must be evicted")
		assert.Nil(t, tr.Get(t2.ID()), "next-oldest terminal run must be evicted")
		assert.Len(t, tr.ListByHook("h", 0), 3)
	})

	t.Run("all active: the list exceeds the cap rather than dropping truth", func(t *testing.T) {
		tr := NewTracker()
		tr.maxByHook = 2
		var all []*Run
		for i := 0; i < 5; i++ {
			all = append(all, tr.New("h"))
		}
		assert.Len(t, tr.ListByHook("h", 0), 5, "active runs are never trimmed")
		for _, r := range all {
			assert.NotNil(t, tr.Get(r.ID()))
		}

		// The backlog shrinks again as runs finish: the next New trims the
		// now-terminal oldest back down to the cap.
		for _, r := range all[:4] {
			r.Finish(StatusSuccess, 0, "")
		}
		last := tr.New("h")
		assert.Len(t, tr.ListByHook("h", 0), 2)
		assert.NotNil(t, tr.Get(all[4].ID()), "the still-active run survives the catch-up trim")
		assert.NotNil(t, tr.Get(last.ID()))
		for _, r := range all[:4] {
			assert.Nil(t, tr.Get(r.ID()), "finished backlog must be trimmed")
		}
	})
}

// ActiveIDs is the hb heartbeat's live-set truth: every non-terminal run's
// id, sorted, and never nil — an idle tracker answers the empty-but-real
// [] verdict (clients must distinguish "nothing active" from "no data").
func TestTrackerActiveIDs(t *testing.T) {
	tr := NewTracker()
	require.NotNil(t, tr.ActiveIDs())
	assert.Empty(t, tr.ActiveIDs())

	a := tr.New("h")
	b := tr.New("h")
	b.SetRunning()
	skipped := tr.New("other")
	skipped.Finish(StatusSkipped, 0, "")

	want := []string{a.ID(), b.ID()}
	sort.Strings(want)
	assert.Equal(t, want, tr.ActiveIDs())

	a.Finish(StatusSuccess, 0, "")
	assert.Equal(t, []string{b.ID()}, tr.ActiveIDs())

	b.Finish(StatusFailure, 1, "boom")
	got := tr.ActiveIDs()
	require.NotNil(t, got, "an idle tracker still answers [], never nil")
	assert.Empty(t, got)
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
