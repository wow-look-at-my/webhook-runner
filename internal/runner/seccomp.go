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
	"errors"
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
// TWO obstructions stand between a container and a working bubblewrap, and
// this clears both -- but only together with a daemon that remaps container
// root, because clearing the second one alone hands the job the host.
//
// SYSCALLS. The default profile permits unshare/clone/clone3/setns and
// mount/umount2/pivot_root only for a container holding CAP_SYS_ADMIN, which a
// hook container does not. The vendored profile adds an ungated allow.
//
// PATHS. Docker also binds /proc/bus, /proc/fs, /proc/irq, /proc/sys and
// /proc/sysrq-trigger read-only and masks paths under /proc, and the kernel
// refuses a fresh procfs mount inside a user namespace while the visible /proc
// carries those. bwrap dies on "Can't mount proc on /newroot/proc: Operation
// not permitted" with every syscall it needs allowed.
// systempaths=unconfined clears them, and docker offers no finer control.
//
// WHY THAT IS SAFE ONLY UNDER REMAP. On a container whose root IS the host's
// root, an unmasked /proc means a writable /proc/sys/kernel/core_pattern -- a
// global file whose helper the HOST kernel runs as real root on any core dump
// -- and a scratch mount already supplies a writable host path to aim it at.
// Dropping CAP_SYS_ADMIN does not refuse that write; only the read-only bind
// does. Under userns-remap the container's root is an unprivileged host uid,
// the host-root-owned file is owned by a uid OUTSIDE the container's map, so
// the namespace's capabilities do not reach it and the write is refused on the
// DAC check. Measured both ways: mapped-root refuses core_pattern,
// sysrq-trigger and /proc/kcore while bwrap still mounts its /proc.
//
// So the two flags ship together or not at all, and a hook that asked for
// userns on a daemon without remap FAILS rather than running half-configured:
// without systempaths its sandbox cannot be built, and with systempaths and no
// remap it would own the machine.
//
// see docs/internals/runner-isolation.md
//
// A plain function, not a Runner method: the `webhook-runner test` path
// (runOneTest) has no Runner, and run/test parity means both paths must go
// through this exact code.
func seccompArgs(hook seccompHook, tmpDir, runID string, remapped bool) (args []string, cleanup func(), err error) {
	if !hook.UsernsAllowed() {
		return nil, func() {}, nil
	}
	if !remapped {
		return nil, func() {}, errors.New(UsernsNeedsRemapMessage)
	}
	path, cleanup, err := writeUsernsProfile(tmpDir, runID)
	if err != nil {
		return nil, func() {}, err
	}
	return []string{
		"--security-opt", "seccomp=" + path,
		"--security-opt", "systempaths=unconfined",
	}, cleanup, nil
}

// UsernsNeedsRemapMessage is why a userns run is refused, shared by every path
// so the log, the run output and the test runner all say the same thing.
const UsernsNeedsRemapMessage = "this entity declares seccomp.userns, which needs docker's read-only and masked /proc paths cleared " +
	"(systempaths=unconfined) before bubblewrap can mount a procfs in its namespace. That is only safe on a daemon that " +
	"remaps container root to an unprivileged host uid, and this daemon does not: without remap the same flag makes " +
	"/proc/sys/kernel/core_pattern writable, which is host code execution. " +
	"Set {\"userns-remap\": \"default\"} in /etc/docker/daemon.json and restart dockerd (see deploy/pool-storage/)"

// seccompHook is the slice of *hooks.Hook seccompArgs needs, so the test
// path and the live-run path can share one implementation.
type seccompHook interface {
	UsernsAllowed() bool
}
