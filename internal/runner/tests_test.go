package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

func testsHook(t *testing.T, dir string, tests [][]string) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(dir, "myhook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	return &hooks.Hook{
		ID:         "myhook",
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Command:    []string{"node", "/app/x.ts"},
		Settings:   json.RawMessage(`{"live_runs_only":true}`),
		Tests:      tests,
	}
}

func TestRunHookTestsDockerInvocation(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	hook := testsHook(t, dir, [][]string{{"node", "--test", "x.test.ts"}})

	var out bytes.Buffer
	err := RunHookTests(hook, TestOptions{Docker: docker, Out: &out})
	require.NoError(t, err)

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	got := out.String()
	for _, want := range []string{
		"arg=run", "arg=--rm",
		"arg=HOOK_ID=myhook",
		"arg=" + tag,
		"arg=--test", "arg=x.test.ts",
	} {
		assert.Contains(t, got, want)
	}
	// Live-run plumbing must not leak into test containers: no payload or headers, no code mount, no settings, and not the hook's own command.
	assert.NotContains(t, got, "HOOK_PAYLOAD_FILE")
	assert.NotContains(t, got, "HOOK_HEADERS_FILE")
	// Tests are self-contained by contract: no payload, no secrets, and no settings either -- a suite must never depend on deployed.
	assert.NotContains(t, got, "HOOK_SETTINGS_FILE")
	assert.NotContains(t, got, "live_runs_only")
	assert.NotContains(t, got, "HOOK_RUN_ID")
	assert.NotContains(t, got, "arg=-v")
	assert.NotContains(t, got, "x.ts\n")
	assert.Contains(t, got, "passed")
}

func TestRunHookTestsBuildsDockerfileHookImage(t *testing.T) {
	dir := t.TempDir()
	docker, buildLog := writeBuildAwareDocker(t, dir)
	hook := dockerfileHook(t, dir)
	hook.Tests = [][]string{{"node", "--test", "x.test.ts"}}

	var out bytes.Buffer
	err := RunHookTests(hook, TestOptions{Docker: docker, Out: &out})
	require.NoError(t, err)

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	built, err := os.ReadFile(buildLog)
	require.NoError(t, err)
	assert.Contains(t, string(built), "buildarg="+tag)
	// Tests run in the built image, not a stock one.
	assert.Contains(t, out.String(), "arg="+tag)
}

func TestRunHookTestsDockerfileBuildFailure(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$1" in
  image) exit 1 ;;
  build) echo "broken Dockerfile" >&2; exit 1 ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	hook := dockerfileHook(t, dir)
	hook.Tests = [][]string{{"true"}}

	err := RunHookTests(hook, TestOptions{Docker: docker})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "docker build")
}

func TestRunHookTestsRunsAllAndAggregatesFailures(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	// Fails any command whose argv mentions FAIL, succeeds otherwise.
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
echo "ran: $@"
case "$@" in *FAIL*) exit 7;; esac
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	hook := testsHook(t, dir, [][]string{{"first-ok"}, {"FAIL-second"}, {"third-ok"}})

	var out bytes.Buffer
	err := RunHookTests(hook, TestOptions{Docker: docker, Out: &out})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test 2 (FAIL-second)")
	assert.NotContains(t, err.Error(), "first-ok")
	assert.NotContains(t, err.Error(), "third-ok")
	// A failure must not stop later commands from running.
	assert.Equal(t, 3, strings.Count(out.String(), "ran:"))
}

func TestRunHookTestsNoTestsIsNoop(t *testing.T) {
	dir := t.TempDir()
	hook := testsHook(t, dir, nil)
	// Docker binary deliberately bogus: it must never be invoked.
	err := RunHookTests(hook, TestOptions{Docker: filepath.Join(dir, "missing")})
	require.NoError(t, err)
}

func TestRunHookTestsRequiresSourceDir(t *testing.T) {
	// Every hook resolves its image from its directory's content hash; a hook not loaded from disk can't.
	hook := &hooks.Hook{ID: "h", Command: []string{"x"}, Tests: [][]string{{"true"}}}
	err := RunHookTests(hook, TestOptions{Docker: "/bin/true"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no source directory")
}

func TestRunHookTestsTimeout(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	// exec replaces the fake-docker shell with sleep, so the fallback Process.Kill genuinely reaps it (no orphan holding the output pipe).
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
exec sleep 30
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	hook := testsHook(t, dir, [][]string{{"slow"}})

	start := time.Now()
	err := RunHookTests(hook, TestOptions{Docker: docker, Timeout: 100 * time.Millisecond})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Less(t, time.Since(start), 10*time.Second)
}

// The build is capped too, by the same clock. A RUN step prints nothing while
// it runs, so a wedged build is indistinguishable from a slow layer: it took a
// runner container with it, twice, and neither run left a log to read.
func TestRunHookTestsCapTheBuildToo(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = "image" ]; then exit 1; fi
if [ "$1" = "build" ]; then exec sleep 30; fi
exit 0
`
	require.NoError(t, os.WriteFile(docker, []byte(script), 0o755))
	hook := testsHook(t, dir, [][]string{{"whatever"}})

	start := time.Now()
	err := RunHookTests(hook, TestOptions{Docker: docker, Timeout: 100 * time.Millisecond})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "myhook", "the wedged entity must be named")
	assert.Contains(t, err.Error(), "produced nothing")
	assert.Less(t, time.Since(start), 10*time.Second)
}
