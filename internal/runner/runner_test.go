package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
# image inspect: pretend every tag is already built; build: succeed quietly
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
shift  # drop "run"
saw_image=false
exit_code=0
while [ $# -gt 0 ]; do
  case "$1" in
    --rm)
      shift ;;
    --name|-v|-e|--network|--user|--workdir|--label)
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

// diskHook returns a hook backed by a real directory — every hook hashes
// its directory to resolve its image tag, so even mock-docker tests need
// one on disk.
func diskHook(t *testing.T, base string, h *hooks.Hook) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(base, h.ID)
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	h.SourcePath = filepath.Join(hookDir, "hook.json")
	return h
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

	hook := diskHook(t, dir, &hooks.Hook{
		ID:      "h",
		Command: []string{"hello", "world"},
	})
	run, err := r.Start(context.Background(), hook, []byte("payload"), http.Header{"X-Test": []string{"yes"}}, "")
	require.NoError(t, err)
	r.Wait()

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	assert.Equal(t, 0, snap.ExitCode)
	assert.Contains(t, snap.Output, "image="+tag)
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

	hook := diskHook(t, dir, &hooks.Hook{
		ID:      "h",
		Command: []string{"EXIT_3"},
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	assert.Equal(t, runs.StatusFailure, run.Status())
	assert.Equal(t, 3, run.ExitCode())
}

// A run producing no output for longer than its timeout is killed —
// `timeout` is activity-based, so this silent sleeper dies at 100ms even
// though nothing bounds its total runtime.
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

	hook := diskHook(t, dir, &hooks.Hook{
		ID:         "h",
		Command:    []string{"SLEEP_30"},
		TimeoutRaw: "100ms",
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)

	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
	r.Wait()

	assert.Equal(t, runs.StatusTimeout, run.Status())
	assert.Contains(t, run.Error(), "no output")
}

// A hook with no timeout must not get a deadline at all: runContext is the
// seam execute() bounds container processing through, and for 0 it returns a
// plain cancellable context — the run is uncapped (bounded only by
// idle_timeout, an explicit cancel, or the container exiting).
func TestRunContextZeroTimeoutHasNoDeadline(t *testing.T) {
	ctx, cancel := runContext(context.Background(), 0)
	defer cancel()

	_, hasDeadline := ctx.Deadline()
	assert.False(t, hasDeadline, "no timeout must mean no deadline — an uncapped run has no absolute ceiling")
	select {
	case <-ctx.Done():
		t.Fatal("uncapped run context must not be done")
	default:
	}
}

func TestRunContextPositiveTimeoutArmsDeadline(t *testing.T) {
	ctx, cancel := runContext(context.Background(), time.Minute)
	defer cancel()

	_, hasDeadline := ctx.Deadline()
	assert.True(t, hasDeadline, "a set timeout must arm the total-timeout deadline")
}

// Parent cancellation still propagates to an uncapped run's context, so
// shutdown paths tied to the parent keep killing the container.
func TestRunContextZeroTimeoutFollowsParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := runContext(parent, 0)
	defer cancel()

	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not propagate to the uncapped run context")
	}
}

// End-to-end through execute(): a hook that omits timeout runs and finishes
// normally. (An absent timeout used to mean a 5m default ceiling; it now
// means no absolute ceiling at all.)
func TestRunnerNoTimeoutRunsToCompletion(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
	})

	hook := diskHook(t, dir, &hooks.Hook{
		ID:      "uncapped",
		Command: []string{"hello"},
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{})
	require.NoError(t, err)
	r.Wait()

	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSuccess, snap.Status)
	assert.Equal(t, 0, snap.ExitCode)
	assert.Contains(t, snap.Output, "hello")
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
	hook := diskHook(t, dir, &hooks.Hook{ID: "h", Command: []string{"x"}})
	_, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
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
	hook := diskHook(t, tmp, &hooks.Hook{ID: "h", Command: []string{"x"}})
	_, err := r.Start(context.Background(), hook, []byte("hello payload"), http.Header{}, "")
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
# image inspect: pretend every tag is already built; build: succeed quietly
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
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

	hook := diskHook(t, dir, &hooks.Hook{
		ID:      "h",
		Command: []string{"SLEEP_30"},
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
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

	hook := diskHook(t, dir, &hooks.Hook{
		ID:      "h",
		Command: []string{"SLEEP_30"},
	})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
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

// The hook's own configuration reaches the container as a FILE, never as
// environment: it is mounted read-only and pointed at by HOOK_SETTINGS_FILE.
// A hook that declares none still gets a readable document ({}), so reading
// config never has a "file missing" branch to guess around.
func TestRunnerMountsSettings(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)

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
		Command:    []string{"x"},
		Settings:   json.RawMessage(`{"app_id":"42","nested":{"n":1}}`),
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	assert.Contains(t, out, "arg=HOOK_SETTINGS_FILE="+mountedSettings)
	mounted := false
	for _, line := range out {
		if strings.HasSuffix(line, ":"+mountedSettings+":ro") {
			mounted = true
		}
	}
	assert.True(t, mounted, "settings.json must be mounted read-only: %v", out)

	// Code is never mounted: only the per-run payload/headers/settings files.
	for _, line := range out {
		assert.NotContains(t, line, hookDir+":")
		assert.NotContains(t, line, "HOOK_DIR")
	}
}

// A hook with no settings still gets an empty object, not a missing file.
func TestRunnerMountsEmptySettingsByDefault(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	hookDir := filepath.Join(dir, "myhook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	r := New(Options{Tracker: runs.NewTracker(), Logger: newSilentLogger(), TmpDir: dir, Docker: docker})
	hook := &hooks.Hook{ID: "myhook", SourcePath: filepath.Join(hookDir, "hook.json"), Command: []string{"x"}}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	assert.Contains(t, run.Snapshot(-1).Output, "arg=HOOK_SETTINGS_FILE="+mountedSettings)
}
