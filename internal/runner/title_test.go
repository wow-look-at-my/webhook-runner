package runner

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// A title handed to Start rides the run end to end: set before anything can
// finish (so the OnFinish snapshot — the run store's write — carries it) and
// woven into the run.started/run.finished activity messages as "id (title)".
func TestRunnerStartCarriesTitle(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	var finished []runs.RunState
	tracker.SetOnFinish(func(st runs.RunState) { finished = append(finished, st) })
	rec := events.NewRecorder(10)
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker, Events: rec})

	hook := diskHook(t, dir, &hooks.Hook{ID: "h", Command: []string{"hello"}})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "wow-look-at-my/go-toolchain#47")
	require.NoError(t, err)
	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	// Finish settles everything before closing done; see runs.Run.Finish.
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", run.Snapshot(-1).Title)
	require.Len(t, finished, 1)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", finished[0].Title,
		"the persisted terminal snapshot must be titled")

	ref := run.ID() + " (wow-look-at-my/go-toolchain#47)"
	var sawStarted, sawFinished bool
	for _, ev := range rec.List(0) {
		switch ev.Kind {
		case "run.started":
			sawStarted = true
			assert.Contains(t, ev.Msg, ref)
		case "run.finished":
			sawFinished = true
			assert.Contains(t, ev.Msg, ref)
		}
	}
	assert.True(t, sawStarted && sawFinished)
}

// An untitled run stays exactly as before: no Title field, plain run ids in
// the feed — the whole feature is additive.
func TestRunnerStartUntitled(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	rec := events.NewRecorder(10)
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker, Events: rec})

	hook := diskHook(t, dir, &hooks.Hook{ID: "h", Command: []string{"hello"}})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	assert.Empty(t, run.Snapshot(-1).Title)
	// The bare id directly precedes the verb — no title parenthetical was
	// inserted (the trailing "(image)" / "(exit N)" parens are unrelated).
	for _, ev := range rec.List(0) {
		switch ev.Kind {
		case "run.started":
			assert.Contains(t, ev.Msg, run.ID()+" started")
		case "run.finished":
			assert.Contains(t, ev.Msg, run.ID()+" finished")
		}
	}
}

// A skipped run is titled too — the title is set BEFORE Finish, so the
// persisted snapshot has it and the run.skipped message names the subject:
// a skip should still say which PR it was about.
func TestRunnerSkipCarriesTitle(t *testing.T) {
	tracker := runs.NewTracker()
	var finished []runs.RunState
	tracker.SetOnFinish(func(st runs.RunState) { finished = append(finished, st) })
	rec := events.NewRecorder(10)
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: t.TempDir(), Events: rec})

	run := r.Skip(&hooks.Hook{ID: "h"}, "skip_if[0]: x == \"y\"", "o/r#9")

	assert.Equal(t, "o/r#9", run.Snapshot(-1).Title)
	require.Len(t, finished, 1)
	assert.Equal(t, "o/r#9", finished[0].Title)

	evs := rec.List(0)
	require.Len(t, evs, 1)
	assert.Equal(t, "run.skipped", evs[0].Kind)
	assert.Contains(t, evs[0].Msg, run.ID()+" (o/r#9) skipped")
}

// A title set mid-run (the state API's /title path calls Run.SetTitle on a
// live run) lands in the run.finished message — the terminal feed line
// names the subject even when no template could have known it.
func TestRunnerMidRunTitleReachesFinishedEvent(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	rec := events.NewRecorder(10)
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker, Events: rec})

	// SLEEP_ keeps the container "running" long enough to title it mid-run.
	hook := diskHook(t, dir, &hooks.Hook{ID: "h", Command: []string{"SLEEP_1"}})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	run.SetTitle("sweep: o/r")
	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	// Finish settles the run.finished write before closing done, so no
	var finishedMsg string
	for _, ev := range rec.List(0) {
		if ev.Kind == "run.finished" {
			finishedMsg = ev.Msg
		}
	}
	assert.Contains(t, finishedMsg, run.ID()+" (sweep: o/r) finished")
}
