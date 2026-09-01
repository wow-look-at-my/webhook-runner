package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingDocker writes a fake docker that answers `inspect` with a fixed
// entrypoint/cmd and APPENDS a line to a counter file every time it is
// invoked that way. Counting real process launches is the only honest way
// to prove the round trip is gone: asserting on the returned argv would
// pass just as well with no cache at all.
func countingDocker(t *testing.T, dir string) (dockerBin, counter string) {
	t.Helper()
	counter = filepath.Join(dir, "inspect-count")
	path := filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "inspect" ]; then
  echo "call" >> %q
  printf '["/entry"]\n["arg1","arg2"]\n'
  exit 0
fi
exit 0
`, counter)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path, counter
}

func inspectCalls(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func TestImageCommandCachesPerTag(t *testing.T) {
	dir := t.TempDir()
	docker, counter := countingDocker(t, dir)

	// A content-hash tag: its ENTRYPOINT/CMD cannot change, so exactly inspect should ever run for it however many runs the hook has.
	tag := "whr-hook/h:" + t.Name()
	first, err := imageCommand(docker, tag, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"/entry", "arg1", "arg2"}, first)
	require.Equal(t, 1, inspectCalls(t, counter))

	for range 25 {
		got, err := imageCommand(docker, tag, nil)
		require.NoError(t, err)
		assert.Equal(t, first, got)
	}
	assert.Equal(t, 1, inspectCalls(t, counter),
		"the tag is a content hash: 26 runs must cost exactly one inspect")
}

func TestImageCommandCacheKeyedOnCommandToo(t *testing.T) {
	dir := t.TempDir()
	docker, counter := countingDocker(t, dir)
	tag := "whr-hook/h:" + t.Name()

	base, err := imageCommand(docker, tag, nil)
	require.NoError(t, err)
	override, err := imageCommand(docker, tag, []string{"other"})
	require.NoError(t, err)

	// Different command overrides are different answers and must not share a cache entry, even though they share a tag.
	assert.Equal(t, []string{"/entry", "arg1", "arg2"}, base)
	assert.Equal(t, []string{"/entry", "other"}, override)
	assert.Equal(t, 2, inspectCalls(t, counter))
}

func TestImageCommandCacheReturnsIndependentSlices(t *testing.T) {
	dir := t.TempDir()
	docker, _ := countingDocker(t, dir)
	tag := "whr-hook/h:" + t.Name()

	first, err := imageCommand(docker, tag, nil)
	require.NoError(t, err)
	// The runner appends the shim's argv onto this result.
	_ = append(first, "injected") //nolint:gocritic // deliberately abusing spare capacity

	second, err := imageCommand(docker, tag, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"/entry", "arg1", "arg2"}, second,
		"a caller mutating one result must not poison the cache")
}

func TestImageCommandDoesNotCacheFailures(t *testing.T) {
	dir := t.TempDir()
	// A docker that always fails inspect: a transient daemon condition is not a property of the tag, so it must be retried, never memoized.
	path := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755))

	tag := "whr-hook/h:" + t.Name()
	_, err := imageCommand(path, tag, nil)
	require.Error(t, err)

	// The same tag now inspects successfully; the earlier failure must not have been cached as an answer.
	docker, counter := countingDocker(t, dir)
	got, err := imageCommand(docker, tag, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"/entry", "arg1", "arg2"}, got)
	assert.Equal(t, 1, inspectCalls(t, counter))
}
