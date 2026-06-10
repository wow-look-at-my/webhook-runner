package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// writeMockDocker drops a shell script at <dir>/docker that:
//   - parses docker-run-style flags (skipping known flag-value pairs)
//   - prints "image=<name>" once it identifies the image
//   - prints each command token after the image on its own line
//   - exits with code N when it sees "EXIT_N" in the command
//   - sleeps N seconds when it sees "SLEEP_N" (kill-able)
//
// This lets the runner be exercised end-to-end without a real docker
// daemon.
func writeMockDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
shift  # drop "run"
saw_image=false
exit_code=0
while [ $# -gt 0 ]; do
  case "$1" in
    --rm)
      shift ;;
    --name|-v|-e|--network|--user|--workdir)
      shift; shift ;;
    --cap-add=*|-*)
      shift ;;
    *)
      if [ "$saw_image" = false ]; then
        saw_image=true
        echo "image=$1"
        shift
      else
        case "$1" in
          EXIT_*) exit_code="${1#EXIT_}"; shift ;;
          SLEEP_*) sleep "${1#SLEEP_}"; shift ;;
          *) echo "$1"; shift ;;
        esac
      fi ;;
  esac
done
exit "$exit_code"
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunnerSuccess(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{
		ID:      "h",
		Image:   "alpine",
		Command: []string{"hello", "world"},
	}
	run, err := r.Start(context.Background(), hook, []byte("payload"), http.Header{"X-Test": []string{"yes"}})
	require.NoError(t, err)
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	assert.Equal(t, 0, snap.ExitCode)
	assert.Contains(t, snap.Output, "image=alpine")
	assert.Contains(t, snap.Output, "hello")
	assert.Contains(t, snap.Output, "world")
}

func TestRunnerFailure(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{
		ID:      "h",
		Image:   "alpine",
		Command: []string{"EXIT_3"},
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	assert.Equal(t, runs.StatusFailure, run.Status())
	assert.Equal(t, 3, run.ExitCode())
}

func TestRunnerTimeout(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{
		ID:         "h",
		Image:      "alpine",
		Command:    []string{"SLEEP_30"},
		TimeoutRaw: "100ms",
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)

	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
	r.Wait()

	assert.Equal(t, runs.StatusTimeout, run.Status())
}

func TestRunnerStartHookCallback(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	var startCalled, finishCalled bool
	r := New(Options{
		Tracker:  tracker,
		Logger:   newSilentLogger(),
		TmpDir:   dir,
		Docker:   docker,
		OnStart:  func(*hooks.Hook, *runs.Run, []byte) { startCalled = true },
		OnFinish: func(*hooks.Hook, *runs.Run, []byte) { finishCalled = true },
	})
	hook := &hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}}
	_, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()
	assert.True(t, startCalled)
	assert.True(t, finishCalled)
}

func TestRunnerWritesPayloadFile(t *testing.T) {
	// Verify the runner writes the payload to the temp dir.
	tmp := t.TempDir()
	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  tmp,
		Docker:  "/bin/true", // accepts and ignores all args, exits 0
	})
	hook := &hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}}
	_, err := r.Start(context.Background(), hook, []byte("hello payload"), http.Header{})
	require.NoError(t, err)
	r.Wait()
	// Temp dir should have been cleaned up; no leftover wh-* dirs.
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, filepath.Ext(e.Name()) == "" && len(e.Name()) > 3 && e.Name()[:3] == "wh-")

	}
}

// writeArgDumpDocker drops a mock docker that prints every raw argument on
// its own line, for asserting the exact docker-run invocation (mounts, env).
func writeArgDumpDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
for a in "$@"; do echo "arg=$a"; done
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestRunnerCancelWhileRunning(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{
		ID:      "h",
		Image:   "alpine",
		Command: []string{"SLEEP_30"},
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)

	deadline := time.Now().Add(5 * time.Second)
	for run.Status() != runs.StatusRunning && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, runs.StatusRunning, run.Status())
	run.RequestCancel()

	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after cancel")
	}
	r.Wait()

	assert.Equal(t, runs.StatusCancelled, run.Status())
	assert.True(t, run.Snapshot(0).CancelRequested)
}

func TestRunnerCancelImmediately(t *testing.T) {
	// A cancel racing the container launch must still end in StatusCancelled,
	// whether the pre-start check catches it or the watcher kills the
	// just-started container.
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{
		ID:      "h",
		Image:   "alpine",
		Command: []string{"SLEEP_30"},
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	run.RequestCancel()

	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after cancel")
	}
	r.Wait()

	assert.Equal(t, runs.StatusCancelled, run.Status())
}

func TestRunnerMountsHookDirAndExpandsEnv(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	t.Setenv("WHR_TEST_SECRET", "s3cret")

	hookDir := filepath.Join(dir, "myhook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{
		ID:         "myhook",
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Image:      "alpine",
		Command:    []string{"x"},
		Env: map[string]string{
			"TOKEN":   "${WHR_TEST_SECRET}",
			"MISSING": "${WHR_TEST_UNSET_VAR}",
			"PLAIN":   "v",
		},
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	assert.Contains(t, out, "arg="+hookDir+":/var/run/webhook-runner/hook:ro")
	assert.Contains(t, out, "arg=HOOK_DIR=/var/run/webhook-runner/hook")
	assert.Contains(t, out, "arg=TOKEN=s3cret")
	assert.Contains(t, out, "arg=MISSING=")
	assert.Contains(t, out, "arg=PLAIN=v")
}

func TestRunnerNoHookDirMountWithoutSourcePath(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := &hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	for _, line := range run.Snapshot(-1).Output {
		assert.NotContains(t, line, "HOOK_DIR")
	}
}
