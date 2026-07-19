package runs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// SetTitle trims, ignores empties (titles are replaced, never cleared),
// lets the last writer win (a /title override beats a template title), and
// refuses to retitle a finished run — its terminal snapshot has already
// flowed through OnFinish into persistence, and history must match.
func TestRunSetTitle(t *testing.T) {
	tr := NewTracker()
	run := tr.New("h")

	assert.Empty(t, run.Title(), "runs start untitled")

	run.SetTitle("  o/r#47  ")
	assert.Equal(t, "o/r#47", run.Title(), "titles are trimmed")
	assert.Equal(t, "o/r#47", run.Snapshot(-1).Title, "snapshots carry the title")

	run.SetTitle("   ")
	assert.Equal(t, "o/r#47", run.Title(), "blank writes never clear a title")

	run.SetTitle("sweep: o/r")
	assert.Equal(t, "sweep: o/r", run.Title(), "the newest title wins")

	run.Finish(StatusSuccess, 0, "")
	run.SetTitle("too late")
	assert.Equal(t, "sweep: o/r", run.Title(), "a terminal run is immutable")
}

// The OnFinish snapshot — the exact state the run store persists — carries
// a title set at any point before Finish.
func TestRunTitleInFinishSnapshot(t *testing.T) {
	tr := NewTracker()
	var got RunState
	tr.SetOnFinish(func(st RunState) { got = st })

	run := tr.New("h")
	run.SetTitle("o/r#47")
	run.Finish(StatusSuccess, 0, "")

	assert.Equal(t, "o/r#47", got.Title)
}
