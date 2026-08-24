package runner

// What these cover: that the userns profile actually permits the operations
// a sandbox performs, and that it relaxes nothing else.
//
// The bug that motivated them: usernsSyscalls listed only the
// namespace-CREATING calls, so a container got a user namespace and then
// could not mount anything inside it. bwrap reports that as "Creating new
// namespace failed: Operation not permitted" -- pointing at the step that
// actually succeeded -- so the profile looked correct from the error alone
// and only a real sandbox run disproved it.

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

// profileAllowsUngated reports whether the profile ends with a rule that
// allows the syscall for ANY container, i.e. not gated on a capability the
// hook container does not hold. Later rules win, so the LAST matching rule
// is the one that decides.
func profileAllowsUngated(t *testing.T, profile []byte, syscall string) bool {
	t.Helper()
	var doc struct {
		Syscalls []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			Includes struct {
				Caps []string `json:"caps"`
			} `json:"includes"`
		} `json:"syscalls"`
	}
	require.NoError(t, json.Unmarshal(profile, &doc))

	allowed := false
	for _, rule := range doc.Syscalls {
		for _, name := range rule.Names {
			if name != syscall {
				continue
			}
			allowed = rule.Action == "SCMP_ACT_ALLOW" && len(rule.Includes.Caps) == 0
		}
	}
	return allowed
}

// The operations bubblewrap performs, split by the phase that misled us:
// creating the namespaces is not the same as being able to furnish them.
var (
	namespaceCalls = []string{"unshare", "clone", "clone3", "setns"}
	sandboxCalls   = []string{"mount", "umount2", "pivot_root"}
)

func TestUsernsProfileAllowsTheNamespaceCalls(t *testing.T) {
	profile, err := usernsProfile()
	require.Nil(t, err)

	for _, call := range namespaceCalls {
		assert.True(t, profileAllowsUngated(t, profile, call),
			"%s must be allowed ungated: bwrap cannot create its namespace without it", call)
	}
}

// The regression test proper. Every one of these is gated on CAP_SYS_ADMIN in
// docker's default profile (pivot_root is not named there at all, so it hits
// the default deny), and a hook container holds no such capability -- so
// without an ungated allow the sandbox is built and then cannot be furnished.
func TestUsernsProfileAllowsTheSandboxToBeBuilt(t *testing.T) {
	profile, err := usernsProfile()
	require.Nil(t, err)

	for _, call := range sandboxCalls {
		assert.True(t, profileAllowsUngated(t, profile, call),
			"%s must be allowed ungated: the namespace is created and then bwrap cannot "+
				"bind /, mount its private /tmp, or pivot into the sandbox (reported "+
				"misleadingly as \"Creating new namespace failed\")", call)
	}
}

// Proof that the assertion above can actually FAIL -- that it is a regression
// test and not a tautology. It rebuilds the profile from the PRE-FIX syscall
// list (namespaces only, the exact bug) and checks that the sandbox calls come
// back denied. Without this, the test above would keep passing if someone
// removed the mount syscalls from usernsSyscalls AND the assertion's own
// helper broke: here the two disagree on purpose.
func TestTheOldNamespaceOnlyListWouldNotBuildASandbox(t *testing.T) {
	saved := usernsSyscalls
	t.Cleanup(func() { usernsSyscalls = saved })

	// The namespace-only list: enough to CREATE a user namespace, and not
	// enough to furnish one, which is how a gha-runner container fails.
	usernsSyscalls = []string{"unshare", "clone", "clone3", "setns"}
	profile, err := usernsProfile()
	require.Nil(t, err)

	for _, call := range namespaceCalls {
		assert.True(t, profileAllowsUngated(t, profile, call),
			"%s: the namespace-only list does allow the namespace calls -- that "+
				"half works, which is what makes the failure hard to read", call)
	}
	for _, call := range sandboxCalls {
		assert.False(t, profileAllowsUngated(t, profile, call),
			"%s: the namespace-only list must NOT allow this, or this file is "+
				"not testing the gap it claims to test", call)
	}
}

// The other half of the contract: this is a NARROW relaxation, so a syscall
// the default profile blocks for reasons unrelated to sandboxing must stay
// blocked. If this ever fails, the allow-list grew something it should not
// have.
func TestUsernsProfileRelaxesNothingElse(t *testing.T) {
	profile, err := usernsProfile()
	require.Nil(t, err)

	// Kernel-module loading, kexec, and the BPF/perf surface have nothing to
	// do with building a sandbox; ptrace of other containers likewise.
	for _, call := range []string{"init_module", "finit_module", "delete_module", "kexec_load", "bpf", "perf_event_open"} {
		assert.False(t, profileAllowsUngated(t, profile, call))

	}
}

// The seccomp profile is only half the opt-in. Docker binds several paths
// under /proc read-only, and the kernel refuses a fresh procfs inside an
// unprivileged user namespace while the visible one is obstructed -- so a
// container with every syscall allowed still cannot build a sandbox. Measured:
// remounting /proc/sys read-only in a mount namespace reproduces the exact
// bwrap message, "Can't mount proc on /newroot/proc: Operation not permitted".
func TestUsernsOptInAlsoUnobstructsProc(t *testing.T) {
	args, cleanup, err := seccompArgs(wantsUserns{}, t.TempDir(), "test")
	require.Nil(t, err)
	defer cleanup()

	assert.Contains(t, args, "systempaths=unconfined",
		"the userns opt-in must also drop docker's read-only and masked /proc "+
			"paths, or bwrap gets its namespace and cannot mount /proc in it")
}

type wantsUserns struct{}

func (wantsUserns) UsernsAllowed() bool { return true }

// The default is untouched: a hook that did not opt in gets no flags at all,
// so its container keeps docker's builtin profile and its command line is
// byte-identical to before the feature existed.
func TestSeccompArgsAreEmptyWithoutTheOptIn(t *testing.T) {
	args, cleanup, err := seccompArgs(noUserns{}, "", "test")
	require.Nil(t, err)

	defer cleanup()
	assert.Equal(t, 0, len(args))

}

type noUserns struct{}

func (noUserns) UsernsAllowed() bool { return false }
