package runner

// What these cover: the advisory fires on a daemon that does not remap, stays
// quiet on one that does, and treats an unreachable daemon as unverified rather
// than fine.
//
// The measurement behind the check, reproduced on this kernel: uid 0 with
// CAP_SYS_ADMIN dropped still writes /proc/sys/kernel/core_pattern. Docker's
// capability set never protected that file; only the read-only /proc bind does.
// So "container root is host root" is one missing mount away from host code
// execution, and remap is what removes the premise.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDockerSecurityOptions writes a stub `docker` printing opts for
// `info --format {{.SecurityOptions}}`, or exiting non-zero when opts is empty
// (the unreachable-daemon case).
func fakeDockerSecurityOptions(t *testing.T, opts string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nexit 1\n"
	if opts != "" {
		script = "#!/bin/sh\necho '" + opts + "'\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestUsernsRemappedDaemonIsAccepted(t *testing.T) {
	docker := fakeDockerSecurityOptions(t, "[name=seccomp,profile=builtin name=userns name=cgroupns]")
	assert.Empty(t, CheckUsernsRemap(docker, newSilentLogger(), nil))
}

// A stock daemon reports every other security option and simply omits this one,
// so the absence is what has to be caught -- there is no negative entry to look
// for.
func TestPlainDaemonIsReported(t *testing.T) {
	docker := fakeDockerSecurityOptions(t, "[name=seccomp,profile=builtin name=cgroupns]")
	msg := CheckUsernsRemap(docker, newSilentLogger(), nil)
	require.NotEmpty(t, msg, "a daemon without userns-remap must be reported")
	assert.Contains(t, msg, "userns-remap")
}

// A near-miss. An entry whose text merely CONTAINS the option is a different
// option, and accepting it would turn this refusal into a silent pass the day
// docker names something else that way.
func TestSimilarlyNamedOptionIsReported(t *testing.T) {
	docker := fakeDockerSecurityOptions(t, "[name=seccomp name=usernsomething]")
	assert.NotEmpty(t, CheckUsernsRemap(docker, newSilentLogger(), nil),
		"name=usernsomething is not name=userns")
}

// Unverified is not a pass. A daemon that cannot be asked looks exactly like a
// correctly configured one from here.
func TestUnreachableDaemonIsReported(t *testing.T) {
	msg := CheckUsernsRemap(fakeDockerSecurityOptions(t, ""), newSilentLogger(), nil)
	require.NotEmpty(t, msg)
	assert.Contains(t, msg, "UNVERIFIED")
}
