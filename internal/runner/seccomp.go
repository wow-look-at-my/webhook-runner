package runner

// Seccomp profile plumbing for the hook.json `seccomp.userns` opt-in.
//
// WHY A FILE AT ALL: docker's --security-opt seccomp= accepts exactly two
// kinds of value -- the literal string "unconfined" (no syscall filtering
// whatsoever) or a PATH to a profile document. There is no CLI syntax for
// "the daemon's default profile, but also allow syscall X". Relaxing ONE
// syscall therefore requires handing docker a complete profile.
//
// WHY THE PROFILE IS VENDORED: the daemon's default profile is compiled
// into the docker binary (`docker info` reports profile=builtin); it is not
// on disk and no API returns it. To express "the default, plus unshare" we
// must own a copy of the default. seccomp/moby-default-<version>.json is
// that copy, taken verbatim from moby at the tagged release. It is embedded
// in the binary rather than deployed to hosts so a fleet machine cannot
// drift from -- or be missing -- the file a run depends on.
//
// MAINTENANCE: the vendored copy pins a moby version and does NOT track the
// daemon in use. If the daemon's builtin profile gains a rule the vendored
// copy lacks, a userns hook runs under the older policy. That is a real,
// accepted cost of the docker CLI having no compose-a-profile syntax;
// TestVendoredDefaultMatchesUpstream documents the provenance so a refresh
// is a deliberate, reviewable bump.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed seccomp/moby-default-v27.5.1.json
var mobyDefaultSeccomp []byte

// usernsSyscalls are the calls bubblewrap (and anything else creating an
// unprivileged user namespace) needs. The vendored default already allows
// most of them, but ONLY for a container holding CAP_SYS_ADMIN -- an
// ordinary hook container has no such capability, so the gated rule never
// matches and the call falls through to the profile's SCMP_ACT_ERRNO
// default. Appending an UNGATED allow for just these names is the whole
// relaxation: last rule wins in libseccomp's evaluation for a given
// syscall, and every other syscall keeps whatever the default profile said.
//
// CREATING the namespace is only half of what a sandbox does. Inside its
// new user + mount namespace bubblewrap builds the filesystem it promised
// -- the read-only bind of /, the private /tmp, the fresh /proc -- and
// those are mount/umount2/pivot_root, which the default profile gates on
// CAP_SYS_ADMIN exactly as it gates unshare (pivot_root it does not name at
// all, so that one hits the default deny). Allowing only the namespace
// calls produced a container where `unshare --user` succeeded and bwrap
// still failed, reporting the misleading "Creating new namespace failed:
// Operation not permitted" -- the namespace was made; the first mount in it
// was refused.
//
// These are namespaced operations, not host ones: a mount inside an
// unprivileged user namespace can only affect that namespace's own mount
// table, which is the isolation the sandbox exists to build.
var usernsSyscalls = []string{
	// Make the namespaces.
	"unshare", "clone", "clone3", "setns",
	// Furnish them.
	"mount", "umount2", "pivot_root",
}

// usernsProfile returns the vendored default profile with an ungated allow
// for the user-namespace syscalls appended. It parses and re-marshals
// rather than string-splicing so a malformed vendored file fails loudly
// here instead of inside the docker daemon.
func usernsProfile() ([]byte, error) {
	var profile map[string]any
	if err := json.Unmarshal(mobyDefaultSeccomp, &profile); err != nil {
		return nil, fmt.Errorf("parse vendored seccomp profile: %w", err)
	}
	calls, ok := profile["syscalls"].([]any)
	if !ok {
		return nil, fmt.Errorf("vendored seccomp profile has no syscalls array")
	}
	names := make([]any, 0, len(usernsSyscalls))
	for _, n := range usernsSyscalls {
		names = append(names, n)
	}
	profile["syscalls"] = append(calls, map[string]any{
		"names":  names,
		"action": "SCMP_ACT_ALLOW",
	})
	out, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal userns seccomp profile: %w", err)
	}
	return out, nil
}

// writeUsernsProfile materializes the userns profile under tmpDir (empty =
// the OS default) and returns its path plus a cleanup. The file is read by
// the DOCKER DAEMON on the host, not by the container, so it is never
// bind-mounted -- it just has to exist and be readable while `docker run`
// starts.
func writeUsernsProfile(tmpDir, runID string) (path string, cleanup func(), err error) {
	profile, err := usernsProfile()
	if err != nil {
		return "", func() {}, err
	}
	dir, err := os.MkdirTemp(tmpDir, "wh-seccomp-"+runID+"-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "userns.json")
	if err := os.WriteFile(path, profile, 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// seccompArgs returns the docker flags implementing a hook's seccomp block,
// plus a cleanup for anything it had to materialize. A hook that did not
// opt in gets NO flags at all, so its container keeps the daemon's builtin
// profile and the docker command line is byte-identical to before.
//
// A plain function, not a Runner method: the `webhook-runner test` path
// (runOneTest) has no Runner, and run/test parity means both paths must go
// through this exact code.
func seccompArgs(hook seccompHook, tmpDir, runID string) (args []string, cleanup func(), err error) {
	if !hook.UsernsAllowed() {
		return nil, func() {}, nil
	}
	path, cleanup, err := writeUsernsProfile(tmpDir, runID)
	if err != nil {
		return nil, func() {}, err
	}
	return []string{"--security-opt", "seccomp=" + path}, cleanup, nil
}

// seccompHook is the slice of *hooks.Hook seccompArgs needs, so the test
// path and the live-run path can share one implementation.
type seccompHook interface {
	UsernsAllowed() bool
}
