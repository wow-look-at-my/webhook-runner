package runner

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// dindHook builds an on-disk hook with the given dind setting, mirroring
// stateHook — a real source dir so the image tag / content hash resolve.
func dindHook(t *testing.T, dir, id string, dind bool) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(dir, id)
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	return &hooks.Hook{
		ID:         id,
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Command:    []string{"x"},
		Dind:       dind,
	}
}

func indexOfArg(lines []string, want string) int {
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
}

// A dind hook's live run gets --privileged and the anonymous /var/lib/docker
// volume, both before the image argument. Removing the flag is what took the
// wow-dind fleet down, so this pins it present rather than absent.
func TestRunnerDindAddsPrivilegeAndVolume(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  writeArgDumpDocker(t, dir),
	})
	hook := dindHook(t, dir, "dinder", true)
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	tag, err := ImageTag(hook)
	require.NoError(t, err)

	mnt := indexOfArg(out, "arg=--mount")
	val := indexOfArg(out, "arg=type=volume,dst=/var/lib/docker")
	img := indexOfArg(out, "arg="+tag)
	priv := indexOfArg(out, "arg=--privileged")

	require.NotEqual(t, -1, img, "image argument must be present")
	require.NotEqual(t, -1, mnt, "--mount must be present")
	require.NotEqual(t, -1, val, "the mount value must be present")
	require.NotEqual(t, -1, priv,
		"--privileged must be present: a nested dockerd must write /proc/sys and "+
			"/sys/fs/cgroup, and without it every dind run dies at cgroup-prep")
	// The --mount value must be the token immediately after the flag.
	assert.Equal(t, mnt+1, val, "--mount value immediately follows --mount")
	assert.Less(t, mnt, img, "--mount must precede the image")
	assert.Less(t, val, img, "the mount value must precede the image")
	assert.Less(t, priv, img, "--privileged must precede the image")
}

// A hook without dind gets neither flag.
func TestRunnerNoDindNoPrivilegedNoVolume(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  writeArgDumpDocker(t, dir),
	})
	run, err := r.Start(context.Background(), dindHook(t, dir, "plain", false), []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	assert.NotContains(t, out, "arg=--privileged")
	assert.NotContains(t, out, "arg=--mount")
	assert.NotContains(t, out, "arg=type=volume,dst=/var/lib/docker")
}

// The `webhook-runner test` path applies the SAME flags before the image for a dind hook -- run/test parity.
func TestRunHookTestsDindAddsPrivilegeAndVolume(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	hook := dindHook(t, dir, "dinder", true)
	hook.Tests = [][]string{{"node", "--test", "x.test.ts"}}

	var out bytes.Buffer
	err := RunHookTests(hook, TestOptions{Docker: docker, Out: &out})
	require.NoError(t, err)

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	got := out.String()

	assert.Contains(t, got, "arg=--privileged",
		"the test path must grant --privileged exactly as a live run does, or a dind hook's tests cannot start a daemon")
	assert.Contains(t, got, "arg=--mount")
	assert.Contains(t, got, "arg=type=volume,dst=/var/lib/docker")
	// The flag and its value precede the image argument.
	imgIdx := strings.Index(got, "arg="+tag)
	require.NotEqual(t, -1, imgIdx)
	assert.Less(t, strings.Index(got, "arg=--mount"), imgIdx, "--mount must precede the image")
	assert.Less(t, strings.Index(got, "arg=type=volume,dst=/var/lib/docker"), imgIdx, "the mount value must precede the image")
}

// The test path adds neither flag for a non-dind hook.
func TestRunHookTestsNoDindNoPrivilegedNoVolume(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	hook := dindHook(t, dir, "plain", false)
	hook.Tests = [][]string{{"true"}}

	var out bytes.Buffer
	err := RunHookTests(hook, TestOptions{Docker: docker, Out: &out})
	require.NoError(t, err)

	got := out.String()
	assert.NotContains(t, got, "--privileged")
	assert.NotContains(t, got, "type=volume,dst=/var/lib/docker")
}
