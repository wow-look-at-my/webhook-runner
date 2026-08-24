package runs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Finish settles every terminal side effect BEFORE closing done, so
// <-run.Done() is a sufficient barrier on its own. This is the invariant
// three runner tests had to work around with a second Runner.Wait()
// barrier before the close moved to the end of Finish — if the close is
// ever hoisted back above the seams, this test fails.
func TestFinishClosesDoneLast(t *testing.T) {
	tr := NewTracker()
	var order []string
	tr.SetOnFinish(func(RunState) { order = append(order, "finish") })
	tr.SetOnChange(func(st RunState) {
		if st.Status.Terminal() {
			order = append(order, "change")
		}
	})
	r := tr.New("h")
	r.SetOnTerminal(func(RunState) { order = append(order, "terminal") })

	// Finish on ANOTHER goroutine, so the only thing ordering the write to `order` against the read below is the done channel's close.
	go r.Finish(StatusSuccess, 0, "")

	select {
	case <-r.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	assert.Equal(t, []string{"terminal", "finish", "change"}, order,
		"every terminal seam must have run before done closed")
}

// The terminal seam carries the run's finished state, so a registrant can
// build its message from the snapshot rather than racing to read it.
func TestOnTerminalSeesTerminalState(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	var got RunState
	r.SetOnTerminal(func(st RunState) { got = st })

	r.Finish(StatusFailure, 3, "boom")

	assert.Equal(t, StatusFailure, got.Status)
	assert.Equal(t, 3, got.ExitCode)
	assert.Equal(t, "boom", got.Error)
	assert.False(t, got.Finished.IsZero(), "the snapshot must already be terminal")
}

// Exactly-once, inherited from Finish's own guard: a second Finish is a
// no-op, so a run can never emit two terminal activity lines.
func TestOnTerminalFiresOnce(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	calls := 0
	r.SetOnTerminal(func(RunState) { calls++ })

	r.Finish(StatusSuccess, 0, "")
	r.Finish(StatusFailure, 1, "second")

	assert.Equal(t, 1, calls)
}

// The seam is optional — nothing registers it on the skip/early-error
// paths, and Finish must not care.
func TestFinishWithoutOnTerminal(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	require.NotPanics(t, func() { r.Finish(StatusSkipped, 0, "") })
	assert.Equal(t, StatusSkipped, r.Snapshot(-1).Status)
}
