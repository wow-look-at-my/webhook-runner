package runner

import (
	"log/slog"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// WarnIfContainerized records a loud misconfiguration event when this server
// appears to run inside a container (any marker file exists — /.dockerenv
// for docker, /run/.containerenv for podman) without TMPDIR set.
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
func WarnIfContainerized(logger *slog.Logger, rec *events.Recorder, markers ...string) {
	if os.Getenv("TMPDIR") != "" {
		return
	}
	for _, m := range markers {
		if _, err := os.Stat(m); err == nil {
			const msg = "running inside a container with no TMPDIR set: per-run payload mounts resolve on the docker HOST, so hook runs will fail (EISDIR) — set TMPDIR to a directory bind-mounted from the host at the same absolute path"
			logger.Error(msg)
			rec.Record("server.misconfigured", msg, nil)
			return
		}
	}
}
