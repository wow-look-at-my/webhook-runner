package runner

import (
	"bytes"
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
		Image:      "node:24-alpine",
		Command:    []string{"node", "/app/x.ts"},
		Env:        map[string]string{"HOOK_ONLY_SECRET": "live-runs-only"},
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

	got := out.String()
	for _, want := range []string{
		"arg=run", "arg=--rm",
		"arg=HOOK_ID=myhook",
		"arg=node:24-alpine",
		"arg=--test", "arg=x.test.ts",
	} {
		assert.Contains(t, got, want)
	}
	// Live-run plumbing must not leak into test containers: no payload or
	// headers, no code mount, no hook.json env, and not the hook's own
	// command.
	assert.NotContains(t, got, "HOOK_PAYLOAD_FILE")
	assert.NotContains(t, got, "HOOK_HEADERS_FILE")
	assert.NotContains(t, got, "HOOK_RUN_ID")
	assert.NotContains(t, got, "HOOK_ONLY_SECRET")
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

func TestRunHookTestsStockImageNeedsNoSourceDir(t *testing.T) {
	// A stock-image hook's tests run with no mounts at all, so even a hook
	// not loaded from disk can run them.
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	hook := &hooks.Hook{ID: "h", Image: "alpine", Command: []string{"x"}, Tests: [][]string{{"true"}}}
	err := RunHookTests(hook, TestOptions{Docker: docker})
	require.NoError(t, err)
}

func TestRunHookTestsTimeout(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	// exec replaces the fake-docker shell with sleep, so the fallback
	// Process.Kill genuinely reaps it (no orphan holding the output pipe).
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
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
