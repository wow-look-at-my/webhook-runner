package runner

import (
	"log/slog"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// TmpDirHazardMessage is the containerized-without-TMPDIR misconfiguration
// text — shared by the startup log/event (WarnIfContainerized) and the
// attention aggregator's boot-scoped "server" entry, so every surface says
// the same thing.
const TmpDirHazardMessage = "running inside a container with no TMPDIR set: per-run payload mounts resolve on the docker HOST, so hook runs will fail (EISDIR) — set TMPDIR to a directory bind-mounted from the host at the same absolute path"

// WarnIfContainerized records a loud misconfiguration event when this server
// appears to run inside a container (any marker file exists — /.dockerenv
// for docker, /run/.containerenv for podman) without TMPDIR set, and
// reports whether the hazard is present so the caller can also store the
// verdict (the attention aggregator holds it for the process's lifetime —
// a running process's environment can't change, so it cannot clear without
// a restart).
//
// It lives here because the hazard is this package's mounting model: per-run
// payload/header files are bind-mounted into hook containers BY HOST PATH
// (the docker CLI talks to the host daemon). A temp dir private to the
// server's own container doesn't exist on the host, docker silently creates
// a directory at the mount source, and every run dies reading its payload
// (EISDIR) — that exact failure shipped once, masked behind an unrelated
// error. A containerized server must therefore write per-run files under a
// directory shared with the host at the same absolute path; setting TMPDIR
// is the deployment's declaration that this is done (the host side can't be
// verified from in here, so a set TMPDIR is trusted). Image builds are
// immune: the CLI streams the build context over the socket.
func WarnIfContainerized(logger *slog.Logger, rec *events.Recorder, markers ...string) bool {
	if os.Getenv("TMPDIR") != "" {
		return false
	}
	for _, m := range markers {
		if _, err := os.Stat(m); err == nil {
			logger.Error(TmpDirHazardMessage)
			rec.Record("server.misconfigured", TmpDirHazardMessage, nil)
			return true
		}
	}
	return false
}
