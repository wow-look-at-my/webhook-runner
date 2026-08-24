package runner

import (
	"context"
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

// writeBuildAwareDocker drops a mock docker that understands the image
// lifecycle: `image inspect` reports not-built, `build` records its args
// to a file, `image ls` lists nothing, and `run` dumps args like
// writeArgDumpDocker.
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
	// The container runs the built tag with no command (image CMD) and no
	// code mount.
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
	// secrets.sops.env entries are still injected as environment; they are the
	// runner's own mechanism, not hook config (which now rides settings.json).
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
// (staying "pending", not "running") and its timeout clock only starts once
// it actually runs — so it does not time out while waiting in the queue.
// This is also the integration proof of the watchdog arming rule: the
// (activity-based) timeout arms only at container launch, so a queued run
// never ticks.
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

	// B has a tiny timeout and a fast command. Started while A holds the
	// slot, it must queue (stay pending) and only start its timeout once it
	// runs — so despite waiting ~1s (>> its 300ms timeout) it succeeds
	// rather than timing out.
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

// The runner registers each run's watchdog reset via SetActivityTouch when
// it arms the watchdog — the seam the state API's declared waits (/wait)
// use. While something keeps calling TouchActivity, a completely silent run
// outlives an idle timeout far shorter than its silence; the identical
// untouched run dies (TestRunnerTimeout above proves the control case).
