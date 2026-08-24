package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDockerInfo writes a stub `docker` that prints root for
// `info --format {{.DockerRootDir}}`, or exits non-zero when root is empty
// (the unreachable-daemon case).
func fakeDockerInfo(t *testing.T, root string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nexit 1\n"
	if root != "" {
		script = "#!/bin/sh\necho " + root + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestCheckDataRootAcceptsStoreUnderTheExpectedPath(t *testing.T) {
	for _, root := range []string{"/mnt/pool/docker", "/mnt/pool", "/mnt/pool/docker/deeper"} {
		assert.Empty(t, CheckDataRoot(fakeDockerInfo(t, root), "/mnt/pool", newSilentLogger(), nil),
			"%s is under /mnt/pool", root)
	}
}

// The comparison is by path SEGMENT: a sibling directory whose name merely
// starts with the expected string is a DIFFERENT filesystem, and passing it
// would report the wrong disk as correct — the one thing this check exists to
// prevent.
func TestCheckDataRootRejectsSiblingPrefix(t *testing.T) {
	msg := CheckDataRoot(fakeDockerInfo(t, "/mnt/pool2/docker"), "/mnt/pool", newSilentLogger(), nil)
	require.NotEmpty(t, msg)
	assert.Contains(t, msg, "/mnt/pool2/docker")
	assert.Contains(t, msg, "data-root")
}

func TestCheckDataRootRejectsAnotherFilesystem(t *testing.T) {
	msg := CheckDataRoot(fakeDockerInfo(t, "/var/lib/docker"), "/mnt/pool", newSilentLogger(), nil)
	require.NotEmpty(t, msg)
	assert.Contains(t, msg, "/var/lib/docker")
}

// No expectation declared means the deployment makes no claim: the location is
// logged for the operator and nothing is reported.
func TestCheckDataRootSilentWithoutAnExpectation(t *testing.T) {
	assert.Empty(t, CheckDataRoot(fakeDockerInfo(t, "/var/lib/docker"), "", newSilentLogger(), nil))
	assert.Empty(t, CheckDataRoot(fakeDockerInfo(t, ""), "", newSilentLogger(), nil))
}

// A declared expectation that could not be CHECKED is a failure, not a pass:
// "unverified" and "correct" look identical from here, and only one of them is
// safe to assume about the disk taking the writes.
func TestCheckDataRootFailsWhenTheDaemonCannotBeAsked(t *testing.T) {
	msg := CheckDataRoot(fakeDockerInfo(t, ""), "/mnt/pool", newSilentLogger(), nil)
	assert.Equal(t, DataRootUnknownMessage, msg)
}

func TestUnderPath(t *testing.T) {
	assert.True(t, underPath("/mnt/pool", "/mnt/pool"))
	assert.True(t, underPath("/mnt/pool/", "/mnt/pool"))
	assert.True(t, underPath("/mnt/pool/docker", "/mnt/pool/"))
	assert.False(t, underPath("/mnt/pool2", "/mnt/pool"))
	assert.False(t, underPath("/mnt", "/mnt/pool"))
}
