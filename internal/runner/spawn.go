package runner

// Spawned-run dispatch (the state API's POST /spawn) — split from
// runner.go for the 750-line cap.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// StartSpawned is Start for runs requested by ANOTHER run through the state
// API's POST /spawn: the identical dispatch pipeline (tracked, group-gated,
// KV-enabled, dashboard-visible, persisted via OnFinish), plus parent
// attribution — the spawned run's RunState carries spawned_by
// {run_id, hook_id} and its run.started/run.finished activity events name
// the parent inline (the runRef message convention, never a new event
// schema). Authorization and validation are the spawn handler's job
// (internal/server/spawn.go — the runner-side allowlist): by the time this
// is called the target is a known, effectively-enabled hook.
func (r *Runner) StartSpawned(parent context.Context, hook *hooks.Hook, payload []byte, headers http.Header, title, parentHookID, parentRunID string) (*runs.Run, error) {
	return r.start(parent, hook, payload, headers, title, parentHookID, parentRunID)
}

// spawnNote annotates an activity-feed message with the parent that spawned
// the run via the state API's POST /spawn — ", spawned by <hook> run <id>"
// — or "" for ordinary delivery/schedule runs. Like titles, attribution
// rides the existing run.started/run.finished message strings (the runRef
// convention), never a new event schema.
func spawnNote(run *runs.Run) string {
	if sb := run.SpawnedBy(); sb != nil {
		return fmt.Sprintf(", spawned by %s run %s", sb.HookID, sb.RunID)
	}
	return ""
}
