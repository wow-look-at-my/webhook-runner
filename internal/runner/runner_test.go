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

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// writeMockDocker drops a shell script at <dir>/docker that: - parses docker-run-style flags (skipping known flag-value pairs) - prints "image=<name>" once it identifies the image - prints each command token after the image on its own line - exits with code N when it sees "EXIT_N" in the command - sleeps N seconds when it sees "SLEEP_N" (kill-able) This lets the runner be.
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

// A run producing no output for longer than its idle_timeout is killed —
// this silent sleeper dies at 100ms even though nothing bounds its total
// runtime (no `timeout` is set).
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
		ID:             "h",
		Command:        []string{"SLEEP_30"},
		IdleTimeoutRaw: "100ms",
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
	// A cancel racing the container launch must still end in StatusCancelled, whether the pre-start check catches it or the watcher kills the.
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

// What actually lands on disk for the container to read. Asserted straight off
// writeTempFiles: by the time a run finishes, its temp dir is already gone.
func TestWriteTempFilesWritesTheSettingsDocument(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{Tracker: runs.NewTracker(), Logger: newSilentLogger(), TmpDir: dir, Docker: "/bin/true"})

	_, _, settingsPath, cleanup, err := r.writeTempFiles("run1", []byte("p"), http.Header{}, []byte(`{"app_id":"42","nested":{"n":1}}`))
	require.NoError(t, err)
	defer cleanup()
	b, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"app_id":"42","nested":{"n":1}}`, string(b))
	assert.Equal(t, "settings.json", filepath.Base(settingsPath))

	// A hook that declares none still gets a readable, parseable document.
	_, _, emptyPath, cleanup2, err := r.writeTempFiles("run2", []byte("p"), http.Header{}, (&hooks.Hook{ID: "h"}).SettingsJSON())
	require.NoError(t, err)
	defer cleanup2()
	b2, err := os.ReadFile(emptyPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(b2))
}

// writeBuildAwareDocker drops a mock docker that understands the image lifecycle: `image inspect` reports not-built, `build` records its args to a file, `image ls` lists nothing, and `run` dumps args like writeArgDumpDocker.
func writeBuildAwareDocker(t *testing.T, dir string) (docker, buildLog string) {
	t.Helper()
	docker = filepath.Join(dir, "docker")
	buildLog = filepath.Join(dir, "build.log")
	script := `#!/bin/sh
case "$1" in
  kill) exit 0 ;;
  image)
    case "$2" in
      inspect) exit 1 ;;
    esac
    exit 0 ;;
  build)
    for a in "$@"; do echo "buildarg=$a" >> ` + buildLog + `; done
    exit 0 ;;
  run)
    for a in "$@"; do echo "arg=$a"; done
    exit 0 ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	return docker, buildLog
}

func dockerfileHook(t *testing.T, dir string) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(dir, "myhook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "Dockerfile"),
		[]byte("FROM alpine\nCMD [\"true\"]\n"), 0o644))
	return &hooks.Hook{
		ID:         "myhook",
		SourcePath: filepath.Join(hookDir, "hook.json"),
	}
}

func TestRunnerBuildsDockerfileHookImage(t *testing.T) {
	dir := t.TempDir()
	docker, buildLog := writeBuildAwareDocker(t, dir)
	hook := dockerfileHook(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()
	require.Equal(t, runs.StatusSuccess, run.Status())

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(tag, "whr-hook/myhook:"))

	built, err := os.ReadFile(buildLog)
	require.NoError(t, err)
	assert.Contains(t, string(built), "buildarg="+tag)
	assert.Contains(t, string(built), "buildarg="+filepath.Join(dir, "myhook"))

	out := run.Snapshot(-1).Output
	// The container runs the built tag with no command (image CMD) and no code mount.
	assert.Contains(t, out, "arg="+tag)
	for _, line := range out {
		assert.NotContains(t, line, "HOOK_DIR")
	}
}

func TestRunnerDockerfileBuildFailureFailsRun(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$1" in
  image) exit 1 ;;
  build) echo "no such base image" >&2; exit 1 ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	hook := dockerfileHook(t, dir)

	tracker := runs.NewTracker()
	r := New(Options{Tracker: tracker, Logger: newSilentLogger(), TmpDir: dir, Docker: docker})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	assert.Equal(t, runs.StatusError, run.Status())
	assert.Contains(t, run.Error(), "docker build")
	assert.Empty(t, run.Snapshot(-1).Output) // the container never started
}

// writeRunnerMockSops mirrors hooks' test mock: cats the (plaintext in tests)
// secrets file passed as its last argument.
func writeRunnerMockSops(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "sops")
	script := `#!/bin/sh
for a; do f="$a"; done
cat "$f"
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestRunnerInjectsSopsSecrets(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	t.Setenv("WHR_RUNNER_HOST_ONLY", "host-val")

	hookDir := filepath.Join(dir, "myhook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	secretsFile := "INJECTED=from-sops\n" +
		"OVERRIDDEN=secret-version\n" +
		"REFERENCED=ref-value\n" +
		"HOOK_ID=evil\n" // reserved: must be skipped, not injected
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, hooks.SecretsFileName), []byte(secretsFile), 0o600))

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Secrets: hooks.NewSecretsLoader(writeRunnerMockSops(t, dir)),
	})

	hook := &hooks.Hook{
		ID:         "myhook",
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Command:    []string{"x"},
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	require.Equal(t, runs.StatusSuccess, run.Status())
	// secrets.sops.env entries are still injected as environment; they are the runner's own mechanism, not hook config (which now rides.
	assert.Contains(t, out, "arg=INJECTED=from-sops")
	assert.Contains(t, out, "arg=OVERRIDDEN=secret-version")
	// A secret may never shadow a key the runner sets itself.
	assert.NotContains(t, out, "arg=HOOK_ID=evil")
}

func TestRunnerSecretsDecryptFailureFailsRun(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)

	hookDir := filepath.Join(dir, "myhook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, hooks.SecretsFileName), []byte("KEY=v\n"), 0o600))

	failingSops := filepath.Join(dir, "sops")
	require.NoError(t, os.WriteFile(failingSops, []byte("#!/bin/sh\necho 'cannot decrypt' >&2\nexit 1\n"), 0o755))

	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Secrets: hooks.NewSecretsLoader(failingSops),
	})

	hook := &hooks.Hook{
		ID:         "myhook",
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Command:    []string{"x"},
	}
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	assert.Equal(t, runs.StatusError, run.Status())
	assert.Contains(t, run.Error(), "cannot decrypt")
	assert.Empty(t, run.Snapshot(-1).Output) // the container never started
}

// waitStatus polls until the run reaches want or the timeout elapses.
func waitStatus(t *testing.T, run *runs.Run, want runs.Status, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if run.Status() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s status %q != want %q within %s", run.ID(), run.Status(), want, timeout)
}

// TestRunnerConcurrencyGroupQueuesAndDefersTimeout is the heart of the
// feature: a second run sharing a limit-1 group queues behind the first
// (staying "pending", not "running") and its absolute `timeout` deadline
// only starts once the slot is acquired — so it does not time out while
// waiting in the queue. This is also the integration proof of the ctx
// arming rule: runContext is created only after the concurrency-group
// slot is acquired, so a queued run's ceiling never ticks.
func TestRunnerConcurrencyGroupQueuesAndDefersTimeout(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 1}},
	})
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Groups:  mgr,
	})

	// A holds the single slot for ~1s.
	hookA := diskHook(t, dir, &hooks.Hook{ID: "a", Command: []string{"SLEEP_1"}, ConcurrencyGroup: "g"})
	runA, err := r.Start(context.Background(), hookA, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	waitStatus(t, runA, runs.StatusRunning, 2*time.Second)

	// B has a tiny timeout and a fast command.
	hookB := diskHook(t, dir, &hooks.Hook{ID: "b", Command: []string{"echo", "b"}, TimeoutRaw: "300ms", ConcurrencyGroup: "g"})
	runB, err := r.Start(context.Background(), hookB, []byte("p"), http.Header{}, "")
	require.NoError(t, err)

	// While A still holds the slot, B is queued, not running.
	assert.Equal(t, runs.StatusPending, runB.Status())

	r.Wait()
	assert.Equal(t, runs.StatusSuccess, runA.Status())
	assert.Equal(t, runs.StatusSuccess, runB.Status(),
		"a queued run must not burn its timeout while waiting in the queue")
}

// TestRunnerUnknownGroupFailsClosed verifies that a hook referencing an
// undeclared group fails the run with an error instead of running unbounded.
func TestRunnerUnknownGroupFailsClosed(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Groups:  concurrency.NewManager(nil), // no groups declared
	})
	hook := diskHook(t, dir, &hooks.Hook{ID: "h", Command: []string{"x"}, ConcurrencyGroup: "ghost"})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()
	assert.Equal(t, runs.StatusError, run.Status())
	assert.Contains(t, run.Error(), "ghost")
}

// TestRunnerCancelWhileQueued verifies a run can be cancelled while it sits
// in the queue, before its container ever starts.
func TestRunnerCancelWhileQueued(t *testing.T) {
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)
	tracker := runs.NewTracker()
	mgr := concurrency.NewManager(&concurrency.Config{
		Groups: map[string]concurrency.Group{"g": {Limit: 1}},
	})
	r := New(Options{
		Tracker: tracker,
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  docker,
		Groups:  mgr,
	})

	hookA := diskHook(t, dir, &hooks.Hook{ID: "a", Command: []string{"SLEEP_30"}, ConcurrencyGroup: "g"})
	runA, err := r.Start(context.Background(), hookA, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	waitStatus(t, runA, runs.StatusRunning, 2*time.Second)

	hookB := diskHook(t, dir, &hooks.Hook{ID: "b", Command: []string{"echo", "b"}, ConcurrencyGroup: "g"})
	runB, err := r.Start(context.Background(), hookB, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	assert.Equal(t, runs.StatusPending, runB.Status())

	runB.RequestCancel()
	select {
	case <-runB.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("queued run did not observe cancel")
	}
	assert.Equal(t, runs.StatusCancelled, runB.Status())
	assert.Empty(t, runB.Snapshot(-1).Output, "a cancelled queued run never starts its container")

	// Free the runner: cancel A so r.Wait returns promptly.
	runA.RequestCancel()
	r.Wait()
}
