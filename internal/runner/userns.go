package runner

// Whether the daemon confines a container's root to an unprivileged host user.
//
// Without userns-remap, container uid 0 IS host uid 0. Every protection then
// rests on docker's masks and its dropped capability set, and those are thinner
// than they look: uid 0 with CAP_SYS_ADMIN dropped still writes
// /proc/sys/kernel/core_pattern -- measured -- so a single missing bind mount
// is the whole distance between a container and a root shell on the host. With
// remap, that write is refused because the process is not uid 0 in the initial
// user namespace, and the same holds for /proc/sysrq-trigger, /sys and every
// other global the DAC check guards.
//
// This is DEFENCE IN DEPTH, reported and not enforced. The property that
// actually keeps a container from disrupting the host -- no writable
// /proc/sysrq-trigger, no writable /proc/sys -- comes from docker's default
// masks, which hostprimitives_test.go pins. Remap is what still holds when one
// of those is wrong. A refusal here would take down every hook over a hardening
// step, which is a bigger outage than the risk.
// see docs/internals/runner-isolation.md

import (
	"log/slog"
	"os/exec"
	"strings"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// usernsSecurityOption is what `docker info` reports for a daemon started with
// userns-remap. Matched as a whole list ENTRY, never as a substring: a
// substring test would accept a future "name=usernsomething" and turn the
// refusal below into a silent pass.
const usernsSecurityOption = "name=userns"

// UsernsMissingMessage is the advisory text, shared by the startup log, the
// activity feed and the attention surface, so every surface says the same thing.
const UsernsMissingMessage = "docker is NOT userns-remapped, so a container's root is the host's root. " +
	"Nothing is broken by this on its own -- docker's default masks still keep /proc/sysrq-trigger and " +
	"/proc/sys read-only -- but it is the layer that holds when one of those is wrong. " +
	"Set {\"userns-remap\": \"default\"} in /etc/docker/daemon.json and restart dockerd (see deploy/pool-storage/)"

// UsernsUnknownMessage is the verdict when the daemon could not be asked.
// Unverified and correct look identical from here, and only one is safe to
// assume.
const UsernsUnknownMessage = "could not read docker's security options, so whether a container's root is confined to an " +
	"unprivileged host user is UNVERIFIED"

// IsUsernsRemapped asks the daemon whether it remaps container root. An
// unreachable daemon answers false: unverified and correct look identical from
// here, and only one of them is safe to act on.
//
// Exported because the `webhook-runner test` path needs the same fact with no
// server, no logger and no activity feed around it.
func IsUsernsRemapped(dockerBin string) bool {
	opts, err := readSecurityOptions(dockerBin)
	return err == nil && hasSecurityOption(opts, usernsSecurityOption)
}

func readSecurityOptions(dockerBin string) (string, error) {
	out, err := exec.Command(dockerBin, "info", "--format", "{{.SecurityOptions}}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CheckUsernsRemap asks the daemon whether it remaps container root, ALWAYS
// logging what it saw, and returns a non-empty message when the answer is no or
// unavailable. The message is an attention entry; the caller keeps serving.
func CheckUsernsRemap(dockerBin string, logger *slog.Logger, rec *events.Recorder) string {
	opts, err := readSecurityOptions(dockerBin)
	if err != nil {
		logger.Warn(UsernsUnknownMessage, "err", err)
		rec.Record("server.misconfigured", UsernsUnknownMessage, nil)
		return UsernsUnknownMessage
	}
	remapped := hasSecurityOption(opts, usernsSecurityOption)
	logger.Info("docker security options", "options", opts, "userns_remap", remapped)
	if remapped {
		return ""
	}
	logger.Warn(UsernsMissingMessage)
	rec.Record("server.misconfigured", UsernsMissingMessage, nil)
	return UsernsMissingMessage
}

// hasSecurityOption reports whether opts names want as a whole entry. docker
// renders the list as "[a b c]" and has changed the punctuation between
// releases, so the brackets are trimmed and the rest is split on whitespace
// rather than pattern-matched.
func hasSecurityOption(opts, want string) bool {
	for _, entry := range strings.Fields(strings.Trim(opts, "[]")) {
		if entry == want {
			return true
		}
	}
	return false
}
