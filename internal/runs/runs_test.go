package runs

import (
	"strings"
	"testing"

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

func TestStatusTerminal(t *testing.T) {
	for _, s := range []Status{StatusSuccess, StatusFailure, StatusTimeout, StatusError, StatusCancelled} {
		assert.True(t, s.Terminal(), string(s))
	}
	for _, s := range []Status{StatusPending, StatusRunning} {
		assert.False(t, s.Terminal(), string(s))
	}
}
