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

func devicesHook(t *testing.T, dir, id string, devices []string) *hooks.Hook {
	t.Helper()
	hookDir := filepath.Join(dir, id)
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	return &hooks.Hook{
		ID:         id,
		SourcePath: filepath.Join(hookDir, "hook.json"),
		Command:    []string{"x"},
		Devices:    devices,
	}
}

// A hook declaring devices gets --device flag per entry, before the image.
func TestRunnerDevicesAddsDeviceFlags(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  writeArgDumpDocker(t, dir),
	})
	hook := devicesHook(t, dir, "deviced", []string{"/dev/net/tun"})
	run, err := r.Start(context.Background(), hook, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	out := run.Snapshot(-1).Output
	tag, err := ImageTag(hook)
	require.NoError(t, err)

	dev := indexOfArg(out, "arg=--device")
	val := indexOfArg(out, "arg=/dev/net/tun")
	img := indexOfArg(out, "arg="+tag)

	require.NotEqual(t, -1, img, "image argument must be present")
	require.NotEqual(t, -1, dev, "--device must be present")
	require.NotEqual(t, -1, val, "the device value must be present")
	assert.Equal(t, dev+1, val, "--device value immediately follows --device")
	assert.Less(t, dev, img, "--device must precede the image")
}

// A hook without devices gets no --device flag.
func TestRunnerNoDevicesNoDeviceFlag(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker: runs.NewTracker(),
		Logger:  newSilentLogger(),
		TmpDir:  dir,
		Docker:  writeArgDumpDocker(t, dir),
	})
	run, err := r.Start(context.Background(), devicesHook(t, dir, "plain", nil), []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	assert.NotContains(t, run.Snapshot(-1).Output, "arg=--device")
}

// The `webhook-runner test` path applies the same --device flags.
func TestRunHookTestsDevicesAddsDeviceFlags(t *testing.T) {
	dir := t.TempDir()
	docker := writeArgDumpDocker(t, dir)
	hook := devicesHook(t, dir, "deviced", []string{"/dev/net/tun"})
	hook.Tests = [][]string{{"node", "--test", "x.test.ts"}}

	var out bytes.Buffer
	err := RunHookTests(hook, TestOptions{Docker: docker, Out: &out})
	require.NoError(t, err)

	tag, err := ImageTag(hook)
	require.NoError(t, err)
	got := out.String()

	imgIdx := strings.Index(got, "arg="+tag)
	require.NotEqual(t, -1, imgIdx)
	devIdx := strings.Index(got, "arg=--device")
	require.NotEqual(t, -1, devIdx, "--device must be present")
	assert.Contains(t, got, "arg=/dev/net/tun")
	assert.Less(t, devIdx, imgIdx, "--device must precede the image")
}
