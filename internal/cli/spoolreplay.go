package cli

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/spool"
)

// replaySpooledDeliveries runs the deliveries the previous process parked
// while it was draining. Called ONCE, after the first load populates the
// registry — a replay needs its hook to exist.
//
// Each entry becomes an ordinary run: same body, same headers, so the hook
// cannot tell a replay from the live delivery it would have received. An
// entry whose hook is gone from the tree is dropped LOUDLY (it can never
// succeed, and keeping it would block the queue forever); everything else
// that fails is kept for the next boot by the Replay contract.
func replaySpooledDeliveries(
	sp *spool.Store,
	registry *hooks.Registry,
	rn *runner.Runner,
	rec *events.Recorder,
	logger *slog.Logger,
) {
	if sp == nil {
		return
	}
	pending := sp.Len()
	if pending == 0 {
		return
	}
	logger.Info("replaying deliveries parked during the last shutdown", "count", pending)

	replayed := sp.Replay(func(e spool.Entry) error {
		hook, ok := registry.Get(e.HookID)
		if !ok {
			// Not an error to the Replay contract: returning nil deletes it.
			// A hook that no longer exists can never run this delivery, and
			// silently keeping it forever would wedge every later replay.
			logger.Error("spooled delivery dropped: its hook is no longer in the tree",
				"hook", e.HookID, "spool_id", e.ID, "received", e.Received)
			rec.Record("spool.dropped",
				"parked delivery for "+e.HookID+" dropped on replay — that hook is no longer in the served tree",
				map[string]string{"hook": e.HookID})
			return nil
		}
		headers := http.Header(e.Headers)
		if _, err := rn.Start(context.Background(), hook, e.Body, headers, e.Title); err != nil {
			return err
		}
		return nil
	})

	logger.Info("spooled deliveries replayed", "replayed", replayed, "remaining", sp.Len())
	rec.Record("spool.replayed",
		"replayed "+strconv.Itoa(replayed)+" delivery/deliveries parked during the last shutdown",
		map[string]string{"replayed": strconv.Itoa(replayed)})
}
