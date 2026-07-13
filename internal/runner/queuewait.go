package runner

// Concurrency-group queue plumbing: slot acquisition plus the observer
// that mirrors a queued run's live place in the line into its waiting_on
// (kind "group") — split from runner.go for the 750-line cap.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// acquireSlot reserves a concurrency-group slot for the run, recording a
// one-time "queued" activity event the moment the run actually has to wait
// (not when it gets a slot immediately). A hook with no concurrency_group
// returns instantly with a no-op release. The returned release must be
// called exactly once when the run finishes; clearQueued must be called
// once the slot is acquired (it clears the waiting_on the queue observer
// stamped — a no-op if the run never actually queued).
func (r *Runner) acquireSlot(hook *hooks.Hook, run *runs.Run) (release func(), acquired bool, clearQueued func(), err error) {
	onQueue, clearQueued := r.groupQueueObserver(hook, run)
	release, acquired, err = r.groups.Acquire(hook.ConcurrencyGroup, run.ID(), run.Cancelled(), onQueue)
	return release, acquired, clearQueued, err
}

// groupQueueObserver builds the concurrency.Manager onQueue callback that
// mirrors a queued run's live place in its group's queue into the run's
// waiting_on — {kind: "group", key: <group>, holder_run_ids, position} —
// plus the matching clear for when the wait ends. The first call also
// records the one-time run.queued event (the Manager only invokes onQueue
// when the run actually has to wait, so a free slot never flickers a wait
// note). Calls arrive serialized under the Manager's mutex; consecutive
// identical states are deduped so re-notifications that change nothing
// don't churn the run's wait sequence.
func (r *Runner) groupQueueObserver(hook *hooks.Hook, run *runs.Run) (onQueue func(concurrency.QueueState), clearQueued func()) {
	var mu sync.Mutex
	var seq uint64
	var lastKey string
	queued := false
	onQueue = func(qs concurrency.QueueState) {
		mu.Lock()
		defer mu.Unlock()
		if !queued {
			queued = true
			r.log.Info("hook run queued",
				"hook", hook.ID, "run", run.ID(), "group", hook.ConcurrencyGroup)
			r.events.Record("run.queued",
				fmt.Sprintf("%s run %s queued on concurrency group %q", hook.ID, run.ID(), hook.ConcurrencyGroup),
				map[string]string{"hook": hook.ID, "run": run.ID(), "group": hook.ConcurrencyGroup})
		}
		key := fmt.Sprintf("%d|%s", qs.Position, strings.Join(qs.Holders, ","))
		if key == lastKey {
			return
		}
		lastKey = key
		seq = run.SetWaitingOn(runs.WaitingOn{
			Kind:         runs.WaitingOnGroup,
			Key:          hook.ConcurrencyGroup,
			HolderRunIDs: qs.Holders,
			Position:     qs.Position,
		})
	}
	clearQueued = func() {
		mu.Lock()
		defer mu.Unlock()
		run.ClearWaitingOn(seq) // seq 0 (never queued) is a no-op by contract
	}
	return onQueue, clearQueued
}
