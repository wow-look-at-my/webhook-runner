package runner

import (
	"log/slog"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// TmpDirHazardMessage is the containerized-without-TMPDIR misconfiguration
const TmpDirHazardMessage = "running inside a container with no TMPDIR set: per-run payload mounts resolve on the docker HOST, so hook runs will fail (EISDIR) — set TMPDIR to a directory bind-mounted from the host at the same absolute path"

// WarnIfContainerized records a misconfiguration event when a container
// marker is present but TMPDIR is unset, and reports whether it fired.
// see docs/internals/hooks-images-and-reload.md
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
