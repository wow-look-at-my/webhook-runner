package runner

// Spawned-run dispatch (the state API's POST /spawn) — split from
// runner.go for the -line cap.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// StartSpawned is Start for runs requested by ANOTHER run through the state API's POST /spawn: the identical dispatch pipeline (tracked, group-gated, KV-enabled, dashboard-visible, persisted via OnFinish), plus parent attribution — the spawned run's RunState.
func (r *Runner) StartSpawned(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header, title, parentHookID, parentRunID string) (*runs.Run, error) {
	return r.start(parent, hook, payload, headers, title, parentHookID, parentRunID)
}

// spawnNote annotates an activity-feed message with the parent that spawned the run via the state API's POST /spawn — ", spawned by <hook> run <id>" — or "".
func spawnNote(run *runs.Run) string {
	if sb := run.SpawnedBy(); sb != nil {
		return fmt.Sprintf(", spawned by %s run %s", sb.HookID, sb.RunID)
	}
	return ""
}
