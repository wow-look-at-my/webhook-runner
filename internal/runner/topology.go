package runner

import (
	"log/slog"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// TmpDirHazardMessage is the containerized-without-TMPDIR misconfiguration text — shared by the startup log/event (WarnIfContainerized) and the attention aggregator's boot-scoped "server" entry, so every surface says the same.
const TmpDirHazardMessage = "running inside a container with no TMPDIR set: per-run payload mounts resolve on the docker HOST, so hook runs will fail (EISDIR) — set TMPDIR to a directory bind-mounted from the host at the same absolute path"

// WarnIfContainerized records a loud misconfiguration event when this server appears to run inside a container (any marker file exists — /.dockerenv for docker, /run/.containerenv for podman) without TMPDIR set, and reports whether the hazard is present so the caller can also store the verdict (the attention aggregator holds it for the.
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
