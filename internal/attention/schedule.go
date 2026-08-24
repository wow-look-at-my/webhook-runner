package attention

import (
	"fmt"
	"sort"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// KeyStale is the one entry key CheckStaleSchedules ever reports (one hook, at most one "its schedule went quiet" problem).
const KeyStale = "stale"

// ScheduleStaleMultiplier and ScheduleStaleFloor set the staleness threshold together: interval*ScheduleStaleMultiplier, floored.
const ScheduleStaleMultiplier = 3

const ScheduleStaleFloor = 15 * time.Minute

// CheckStaleSchedules derives the "schedule" source: a hook declaring a
// `schedule` interval whose last SUCCESSFUL run (among the tracked
// history `recent` returns, newest-first) is older than its staleness
// threshold — or that has never once succeeded, once enough time has
// passed for that to be meaningful rather than "hasn't had a chance yet".
//
// This exists because a scheduled tick is typically the reliability
// BACKSTOP for whatever a hook manages (a periodic reconcile pass being
// the motivating shape): when the backstop itself stops succeeding —
// its credentials broke, an upstream dependency started refusing it,
// the runner stopped invoking it — nothing else notices, because from
// the outside a hook that has stopped succeeding looks identical to one
// that simply has nothing to do. Every other attention source here
// covers a load-time or request-time defect; this is the one source
// that watches a hook's own track record over time.
//
// `recent` returns the hook's tracked runs, newest-first (internal/runs.
// Tracker.ListByHook). A hook with NO tracked runs yet is skipped
// entirely — schedules fire immediately when first seen, so an empty
// result is a brief startup window, not a standing problem.
func CheckStaleSchedules(schedules map[string]time.Duration, recent func(hookID string) []*runs.Run, now time.Time) []Entry {
	ids := make([]string, 0, len(schedules))
	for id := range schedules {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		interval := schedules[id]
		if interval <= 0 {
			continue
		}
		threshold := interval * ScheduleStaleMultiplier
		if threshold < ScheduleStaleFloor {
			threshold = ScheduleStaleFloor
		}

		tracked := recent(id)
		if len(tracked) == 0 {
			continue
		}

		var lastSuccess time.Time
		for _, r := range tracked {
			if r.Status() == runs.StatusSuccess && r.Started().After(lastSuccess) {
				lastSuccess = r.Started()
			}
		}

		if !lastSuccess.IsZero() {
			if now.Sub(lastSuccess) < threshold {
				continue // succeeded recently enough
			}
			entries = append(entries, Entry{
				Hook: id,
				Key:  KeyStale,
				Message: fmt.Sprintf(
					"scheduled every %s, but its last successful run was %s ago (exceeds the %s staleness threshold) — the schedule may have stopped firing, or every recent attempt has failed",
					interval, now.Sub(lastSuccess).Round(time.Second), threshold,
				),
			})
			continue
		}

		// Never succeeded within the tracked window.
		oldest := tracked[len(tracked)-1].Started()
		if now.Sub(oldest) < threshold {
			continue
		}
		entries = append(entries, Entry{
			Hook: id,
			Key:  KeyStale,
			Message: fmt.Sprintf(
				"scheduled every %s, but no successful run is on record in its tracked history (oldest tracked attempt %s ago, exceeding the %s staleness threshold) — its tick may have stopped firing, or every run may be failing before completing",
				interval, now.Sub(oldest).Round(time.Second), threshold,
			),
		})
	}
	return entries
}
