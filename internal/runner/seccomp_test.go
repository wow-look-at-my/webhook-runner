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
	if err := json.Unmarshal(profile, &doc); err != nil {
		t.Fatalf("parse profile: %v", err)
	}
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
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	for _, call := range namespaceCalls {
		if !profileAllowsUngated(t, profile, call) {
			t.Errorf("%s must be allowed ungated: bwrap cannot create its namespace without it", call)
		}
	}
}

// The regression test proper. Every one of these is gated on CAP_SYS_ADMIN in
// docker's default profile (pivot_root is not named there at all, so it hits
// the default deny), and a hook container holds no such capability -- so
// without an ungated allow the sandbox is built and then cannot be furnished.
func TestUsernsProfileAllowsTheSandboxToBeBuilt(t *testing.T) {
	profile, err := usernsProfile()
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	for _, call := range sandboxCalls {
		if !profileAllowsUngated(t, profile, call) {
			t.Errorf("%s must be allowed ungated: the namespace is created and then "+
				"bwrap cannot bind /, mount its private /tmp, or pivot into the sandbox "+
				"(reported misleadingly as \"Creating new namespace failed\")", call)
		}
	}
}

// The other half of the contract: this is a NARROW relaxation, so a syscall
// the default profile blocks for reasons unrelated to sandboxing must stay
// blocked. If this ever fails, the allow-list grew something it should not
// have.
func TestUsernsProfileRelaxesNothingElse(t *testing.T) {
	profile, err := usernsProfile()
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	// Kernel-module loading, kexec, and the BPF/perf surface have nothing to
	// do with building a sandbox; ptrace of other containers likewise.
	for _, call := range []string{"init_module", "finit_module", "delete_module", "kexec_load", "bpf", "perf_event_open"} {
		if profileAllowsUngated(t, profile, call) {
			t.Errorf("%s must NOT be allowed ungated: the userns opt-in is for sandboxing, not general privilege", call)
		}
	}
}

// The default is untouched: a hook that did not opt in gets no flags at all,
// so its container keeps docker's builtin profile and its command line is
// byte-identical to before the feature existed.
func TestSeccompArgsAreEmptyWithoutTheOptIn(t *testing.T) {
	args, cleanup, err := seccompArgs(noUserns{}, "", "test")
	if err != nil {
		t.Fatalf("seccompArgs: %v", err)
	}
	defer cleanup()
	if len(args) != 0 {
		t.Errorf("a hook without seccomp.userns must get no docker flags, got %v", args)
	}
}

type noUserns struct{}

func (noUserns) UsernsAllowed() bool { return false }
