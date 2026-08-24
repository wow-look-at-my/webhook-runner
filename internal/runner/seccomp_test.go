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
	"slices"
	"strings"
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

	// The list exactly as it shipped, and exactly as it failed on a real
	// gha-runner container.
	usernsSyscalls = []string{"unshare", "clone", "clone3", "setns"}
	profile, err := usernsProfile()
	require.Nil(t, err)

	for _, call := range namespaceCalls {
		assert.True(t, profileAllowsUngated(t, profile, call),
			"%s: the old list did allow the namespace calls -- that half worked, "+
				"which is what made the bug so hard to read", call)
	}
	for _, call := range sandboxCalls {
		assert.False(t, profileAllowsUngated(t, profile, call),
			"%s: the old list must NOT allow this, or this file is not testing "+
				"the bug it claims to test", call)
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

// The two opt-ins are INDEPENDENT, and that is the point of them being two
// fields. Building a sandbox needs both -- docker masks parts of /proc in
// every container, and the kernel refuses a fresh procfs mount inside a user
// namespace while anything obscures the one already there, so bwrap creates
// its namespace and then fails on the first mount -- but they widen different
// things, so declaring one must never quietly grant the other.
func TestSeccompFlagsFollowTheirOwnOptIns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hook    seccompHook
		profile bool
		unmask  bool
	}{
		{"neither", fakeSeccompHook{}, false, false},
		{"userns only", fakeSeccompHook{userns: true}, true, false},
		{"systempaths only", fakeSeccompHook{unmask: true}, false, true},
		{"both (what a sandbox needs)", fakeSeccompHook{userns: true, unmask: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, cleanup, err := seccompArgs(tc.hook, t.TempDir(), "test")
			require.Nil(t, err)
			t.Cleanup(cleanup)

			profile := false
			for _, a := range args {
				if strings.HasPrefix(a, "seccomp=") {
					profile = true
				}
			}
			assert.Equal(t, tc.profile, profile, "seccomp profile flag, args = %v", args)
			assert.Equal(t, tc.unmask, slices.Contains(args, "systempaths=unconfined"),
				"systempaths flag, args = %v", args)

			// An audited grant emits exactly what was asked for: a third
			// flag here is a widening that must be argued for on purpose.
			want := 0
			if tc.profile {
				want += 2
			}
			if tc.unmask {
				want += 2
			}
			assert.Equal(t, want, len(args), "args = %v", args)
		})
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

func (noUserns) UsernsAllowed() bool       { return false }
func (noUserns) SystemPathsUnmasked() bool { return false }

type fakeSeccompHook struct{ userns, unmask bool }

func (h fakeSeccompHook) UsernsAllowed() bool       { return h.userns }
func (h fakeSeccompHook) SystemPathsUnmasked() bool { return h.unmask }
