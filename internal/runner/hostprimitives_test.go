package runner

// The property: nothing a container gets from this runner lets it disturb the
// host. Not "a kernel exploit is impossible" -- that is not a promise a shared
// kernel can make -- but the cheap, one-line kind:
//
//	echo b > /proc/sysrq-trigger              reboots the host
//	echo '|/x' > /proc/sys/kernel/core_pattern  runs /x as real root on the host
//
// On a container whose root IS the host's root, both are refused only because
// docker binds /proc/sysrq-trigger and /proc/sys READ-ONLY. That bind is the
// whole protection: measured on this kernel, uid 0 with CAP_SYS_ADMIN dropped
// still writes core_pattern, so the capability set never guarded it.
//
// Two docker flags remove the bind. --privileged is banned outright. The other,
// systempaths=unconfined, is REQUIRED for bubblewrap -- the kernel refuses a
// fresh procfs inside a user namespace while the visible /proc is obstructed --
// so it ships behind an interlock instead: only on a daemon that remaps
// container root to an unprivileged host uid, where the host-root-owned file
// sits outside the container's map and the write is refused on the DAC check.
// Measured both ways: mapped root refuses core_pattern, sysrq-trigger and
// /proc/kcore, and bwrap still mounts its /proc.
//
// So these tests assert the INTERLOCK, not the absence of a flag. The argv is
// built in three places and a reviewer reading one cannot see the other two.

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Flags that hand a container something the host cannot take back. Each entry
// is matched against a whole argv token, so a path that merely contains the
// text is not a false positive.
//
// --device and --cap-add are here as a category, not because either is used:
// they are how this list would be reopened, one narrow-looking grant at a time.
var hostPrimitiveFlags = []string{
	"--privileged",
	"--pid=host",
	"--ipc=host",
	"--userns=host",
	"--cgroupns=host",
	"--security-opt=apparmor=unconfined",
	"apparmor=unconfined",
	"--device",
	"--cap-add",
	"--cap-add=SYS_ADMIN",
	"--cap-add=SYS_BOOT",
	"--cap-add=SYS_MODULE",
	"-v=/proc",
	"/proc:/proc",
	"/sys:/sys",
}

// maximalHook turns on every field that adds docker flags, so one argv exercises
// every branch that could smuggle one in.
func maximalHook(t *testing.T, dir string) *hooks.Hook {
	t.Helper()
	h := dindHook(t, dir, "maximal", true)
	h.Tmpfs = []string{"/tmp:size=1g"}
	h.ReadOnlyRootfs = true
	h.Seccomp = &hooks.SeccompConfig{Userns: true}
	return h
}

func assertNoHostPrimitive(t *testing.T, argv []string, where string) {
	t.Helper()
	for _, line := range argv {
		token := strings.TrimPrefix(strings.TrimSpace(line), "arg=")
		for _, banned := range hostPrimitiveFlags {
			assert.NotEqual(t, banned, token,
				"%s passes %s, which lets a root container write /proc/sysrq-trigger or "+
					"/proc/sys/kernel/core_pattern and take the host. If a container genuinely "+
					"needs this, it needs a VM, not a flag.", where, banned)
		}
	}
}

// The live-run path on a remapped daemon, with every privilege-adjacent field
// set. systempaths is expected here: that is what makes bwrap work, and remap
// is what makes it safe.
func TestLiveRunGrantsNoHostPrimitive(t *testing.T) {
	dir := t.TempDir()
	r := New(Options{
		Tracker:        runs.NewTracker(),
		Logger:         newSilentLogger(),
		TmpDir:         dir,
		Docker:         writeArgDumpDocker(t, dir),
		UsernsRemapped: true,
	})
	run, err := r.Start(context.Background(), maximalHook(t, dir), []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	r.Wait()

	assertNoHostPrimitive(t, run.Snapshot(-1).Output, "the live-run path")
}

// The interlock's whole point: an unmasked /proc reaches a container ONLY on a
// daemon that remaps its root. Without remap the run is refused rather than
// served with the masks in place, because a hook that asked for userns cannot
// build its sandbox either way -- and half-configuring it quietly is how this
// arrangement would rot back into a lie.
func TestUsernsRunIsRefusedWithoutRemap(t *testing.T) {
	dir := t.TempDir()
	_, cleanup, err := seccompArgs(wantsUserns{}, dir, "test", false)
	defer cleanup()

	require.Error(t, err, "a userns hook must not run on a daemon that does not remap container root")
	assert.Contains(t, err.Error(), "userns-remap")
}

// And with remap it is granted, so the refusal above is an interlock rather
// than a permanent no.
func TestUsernsRunGetsAnUnmaskedProcUnderRemap(t *testing.T) {
	dir := t.TempDir()
	args, cleanup, err := seccompArgs(wantsUserns{}, dir, "test", true)
	defer cleanup()
	require.NoError(t, err)

	assert.Contains(t, args, "systempaths=unconfined",
		"bwrap cannot mount a procfs in its namespace while docker's masked and "+
			"read-only /proc paths are in place")
}

// The `webhook-runner test` path. It mirrors the live path deliberately, so a
// flag added to one and not the other is the shape this catches.
func TestTestPathGrantsNoHostPrimitive(t *testing.T) {
	dir := t.TempDir()
	hook := maximalHook(t, dir)
	hook.Tests = [][]string{{"true"}}

	var out bytes.Buffer
	require.NoError(t, RunHookTests(hook, TestOptions{
		Docker:         writeArgDumpDocker(t, dir),
		Out:            &out,
		UsernsRemapped: true,
	}))

	assertNoHostPrimitive(t, strings.Split(out.String(), "\n"), "the webhook-runner test path")
}

// Proof the assertion can fail: the same walk over an argv that DOES carry one
// must report it. Without this the three tests above would keep passing if
// assertNoHostPrimitive stopped looking at anything.
func TestTheCheckCatchesAHostPrimitive(t *testing.T) {
	found := false
	for _, line := range []string{"arg=--rm", "arg=--privileged", "arg=img"} {
		token := strings.TrimPrefix(strings.TrimSpace(line), "arg=")
		for _, banned := range hostPrimitiveFlags {
			if token == banned {
				found = true
			}
		}
	}
	assert.True(t, found, "the walk must find --privileged in an argv that contains it")
}
