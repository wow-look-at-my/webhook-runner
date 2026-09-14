package runner

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// The directive's core property at the runner level: with a global cap of
// N, N+ executions run at most N containers simultaneously — the extra
// QUEUES (pending, no container) and runs a slot frees; nothing is
// dropped or errored. Hooks share no concurrency group, so the global cap
// is the only thing gating them.
func TestRunnerGlobalCapQueuesExcess(t *testing.T) {
	const capN = 2
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	r := New(Options{
		Tracker:   tracker,
		Logger:    newSilentLogger(),
		TmpDir:    dir,
		Docker:    docker,
		GlobalCap: concurrency.NewGlobal(capN),
	})

	// N slow runs take the slots.
	holders := make([]*runs.Run, 0, capN)
	for i := 0; i < capN; i++ {
		hook := diskHook(t, dir, &hooks.Hook{ID: fmt.Sprintf("h%d", i), Command: []string{"SLEEP_1"}})
		run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
		require.NoError(t, err)
		holders = append(holders, run)
	}
	for _, run := range holders {
		waitStatus(t, run, runs.StatusRunning, 2*time.Second)
	}

	// The N+th queues: pending, container not started, while N run.
	extraHook := diskHook(t, dir, &hooks.Hook{ID: "extra", Command: []string{"echo", "extra"}})
	extra, err := r.Start(context.Background(), extraHook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		w := extra.Snapshot(0).WaitingOn
		return w != nil && w.Kind == runs.WaitingOnGroup && w.Key == concurrency.GlobalWaitKey
	}, 2*time.Second, 5*time.Millisecond, "the excess run must report waiting on the global cap")
	assert.Equal(t, runs.StatusPending, extra.Status(), "the excess run must queue, not start")
	for _, run := range holders {
		assert.Equal(t, runs.StatusRunning, run.Status(), "the cap holds at N while the extra queues")
	}

	// Slots free: the queued run executes. Nothing dropped, nothing errored.
	r.Wait()
	for _, run := range holders {
		assert.Equal(t, runs.StatusSuccess, run.Status())
	}
	assert.Equal(t, runs.StatusSuccess, extra.Status(), "the queued run must eventually run to completion")
	assert.Contains(t, extra.Snapshot(-1).Output, "extra")
}

// Raising the cap live admits already-queued runs immediately — without
// waiting for any holder to release (the waiter re-bind machinery).
func TestRunnerGlobalCapRaiseAdmitsQueued(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	cap1 := concurrency.NewGlobal(1)
	r := New(Options{
		Tracker:   runs.NewTracker(),
		Logger:    newSilentLogger(),
		TmpDir:    dir,
		Docker:    docker,
		GlobalCap: cap1,
	})

	hookA := diskHook(t, dir, &hooks.Hook{ID: "a", Command: []string{"SLEEP_1"}})
	runA, err := r.Start(context.Background(), hookA, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	waitStatus(t, runA, runs.StatusRunning, 2*time.Second)

	hookB := diskHook(t, dir, &hooks.Hook{ID: "b", Command: []string{"echo", "b"}})
	runB, err := r.Start(context.Background(), hookB, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return cap1.Status().Waiting == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, runs.StatusPending, runB.Status())

	// Raise: B must run to completion while A still holds its original slot — the proof it was admitted by the raise, not by A's release.
	require.NoError(t, cap1.SetLimitOverride(2))
	select {
	case <-runB.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("raising the cap did not admit the queued run")
	}
	assert.Equal(t, runs.StatusSuccess, runB.Status())
	assert.Equal(t, runs.StatusRunning, runA.Status(), "the raise admits B without touching A")

	r.Wait()
	assert.Equal(t, runs.StatusSuccess, runA.Status())
}

// A run cancelled while queued on the global cap terminates cleanly before
// its container ever starts (the group-queue cancel contract).
func TestRunnerGlobalCapCancelWhileQueued(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	r := New(Options{
		Tracker:   runs.NewTracker(),
		Logger:    newSilentLogger(),
		TmpDir:    dir,
		Docker:    docker,
		GlobalCap: concurrency.NewGlobal(1),
	})

	hookA := diskHook(t, dir, &hooks.Hook{ID: "a", Command: []string{"SLEEP_30"}})
	runA, err := r.Start(context.Background(), hookA, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	waitStatus(t, runA, runs.StatusRunning, 2*time.Second)

	hookB := diskHook(t, dir, &hooks.Hook{ID: "b", Command: []string{"echo", "b"}})
	runB, err := r.Start(context.Background(), hookB, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return runB.Snapshot(0).WaitingOn != nil }, 2*time.Second, 5*time.Millisecond)

	runB.RequestCancel()
	select {
	case <-runB.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("globally-queued run did not observe cancel")
	}
	assert.Equal(t, runs.StatusCancelled, runB.Status())
	assert.Empty(t, runB.Snapshot(-1).Output, "a cancelled queued run never starts its container")

	runA.RequestCancel()
	r.Wait()
}

// Every hook-run container carries the orphan-sweep marker label, before
// the image argument (so docker parses it as a run flag).
func TestRunnerStampsRunContainerLabel(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  writeArgDumpDocker(t, dir),
	})
	hook := diskHook(t, dir, &hooks.Hook{ID: "labeled", Command: []string{"x"}})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	lbl := indexOfArg(out, "arg=--label")
	val := indexOfArg(out, "arg="+RunContainerLabel+"=1")
	tag, err := ImageTag(hook)
	require.NoError(t, err)
	img := indexOfArg(out, "arg="+tag)
	require.NotEqual(t, -1, lbl, "--label must be present")
	require.NotEqual(t, -1, val, "the marker label pair must be present")
	assert.Equal(t, lbl+1, val, "the label value immediately follows --label")
	require.NotEqual(t, -1, img)
	assert.Less(t, lbl, img, "--label must precede the image")
}

// writeSweepDocker mocks the docker calls the orphan sweep makes:
// `docker ps -aq --filter label=…` prints the canned container ids, and
// every `docker rm -f <id>` is recorded to a log file.
func writeSweepDocker(t *testing.T, dir string, psIDs []string) (docker, rmLog string) {
	t.Helper()
	docker = filepath.Join(dir, "docker")
	rmLog = filepath.Join(dir, "rm.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ps\" ]; then\n"
	for _, id := range psIDs {
		script += "  echo " + id + "\n"
	}
	script += "  exit 0\nfi\n" +
		"if [ \"$1\" = \"rm\" ]; then echo \"$2 $3\" >> " + rmLog + "; exit 0; fi\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	waitExecutable(t, docker)
	return docker, rmLog
}

// The startup sweep force-removes every marker-labeled container and
// records summarizing event; with none present it does nothing.
func TestSweepOrphanContainers(t *testing.T) {
	dir := t.TempDir()
	docker, rmLog := writeSweepDocker(t, dir, []string{"aaa111", "bbb222"})
	rec := events.NewRecorder(10)
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Events:  rec,
	})
	r.SweepOrphanContainers()

	data, err := os.ReadFile(rmLog)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.ElementsMatch(t, []string{"-f aaa111", "-f bbb222"}, lines,
		"every orphan is force-removed")
	evs := rec.List(0)
	require.Len(t, evs, 1)
	assert.Equal(t, "run.orphans_removed", evs[0].Kind)
	assert.Contains(t, evs[0].Msg, "2 orphaned hook container(s)")
}

func TestSweepOrphanContainersNoOrphans(t *testing.T) {
	dir := t.TempDir()
	docker, rmLog := writeSweepDocker(t, dir, nil)
	rec := events.NewRecorder(10)
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Events:  rec,
	})
	r.SweepOrphanContainers()
	_, err := os.Stat(rmLog)
	assert.True(t, os.IsNotExist(err), "nothing to remove, nothing removed")
	assert.Empty(t, rec.List(0), "no orphans, no event")
}
