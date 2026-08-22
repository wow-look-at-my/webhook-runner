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
		assert.True(t, profileAllowsUngated(t, profile, call))

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
		assert.True(t, profileAllowsUngated(t, profile, call))

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
