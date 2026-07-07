package runs

import "time"

// HookRunStats aggregates one hook's runs over a window the caller defines:
// the tracker's bounded in-memory window (the MaxTracked newest runs, reset
// on restart), or — when a run store is configured — that window merged with
// the persisted completed-run history. MaxTracked and Retention ship in the
// payload so clients can label the window honestly.
type HookRunStats struct {
	// Tracked is how many runs the window holds.
	Tracked int `json:"tracked"`
	// MaxTracked is the live tracker's per-hook retention bound.
	MaxTracked int `json:"max_tracked"`
	// Retention, when set, is the persisted-history window backing these
	// stats (the run store's configured retention, compacted — e.g. "48h"):
	// the figures then cover live runs plus completed runs persisted for
	// that long, surviving restarts. Empty means memory-only.
	Retention string `json:"retention,omitempty"`
	// ByStatus counts every run in the window, including active ones.
	ByStatus map[Status]int `json:"by_status,omitempty"`
	// Completed counts runs with a terminal status; active (pending/running)
	// runs appear in Tracked and ByStatus but are excluded from the rate and
	// duration figures below.
	Completed int `json:"completed"`
	// SuccessRate is successes/Completed over the window. It is 0 when
	// Completed is 0 — check Completed before displaying it.
	SuccessRate float64 `json:"success_rate"`
	// AvgDurationMS/MaxDurationMS cover completed runs' Started→Finished
	// span. That span includes any time spent queued behind a concurrency
	// group — the tracker records no separate processing-start time.
	AvgDurationMS int64 `json:"avg_duration_ms"`
	MaxDurationMS int64 `json:"max_duration_ms"`
	// LastRun is the newest run by start time, whatever its status — an
	// in-flight run is deliberately included, it IS the latest.
	LastRun *LastRun `json:"last_run,omitempty"`
}

// LastRun identifies one run for HookRunStats without dragging along output.
type LastRun struct {
	ID       string    `json:"id"`
	Status   Status    `json:"status"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// ComputeStats aggregates already-snapshotted states of one hook, in any
// order (LastRun is found by start time, not position). It fills everything
// except the window metadata — MaxTracked and Retention are the caller's to
// set, since only the caller knows which window the states came from.
func ComputeStats(states []RunState) HookRunStats {
	var stats HookRunStats
	if len(states) == 0 {
		return stats
	}
	stats.Tracked = len(states)
	stats.ByStatus = make(map[Status]int)
	var successes int
	var totalDur, maxDur time.Duration
	for i := range states {
		snap := states[i]
		stats.ByStatus[snap.Status]++
		if stats.LastRun == nil || snap.Started.After(stats.LastRun.Started) {
			stats.LastRun = &LastRun{ID: snap.ID, Status: snap.Status, Started: snap.Started, Finished: snap.Finished}
		}
		if !snap.Status.Terminal() {
			continue
		}
		// Terminal implies Finished is set: Finish records both under one
		// lock, and Snapshot reads under the same lock.
		stats.Completed++
		if snap.Status == StatusSuccess {
			successes++
		}
		d := snap.Finished.Sub(snap.Started)
		totalDur += d
		if d > maxDur {
			maxDur = d
		}
	}
	if stats.Completed > 0 {
		stats.SuccessRate = float64(successes) / float64(stats.Completed)
		stats.AvgDurationMS = (totalDur / time.Duration(stats.Completed)).Milliseconds()
		stats.MaxDurationMS = maxDur.Milliseconds()
	}
	return stats
}

// StatsByHook aggregates the tracker's retained runs of one hook (the
// memory-only window). A hook with no retained runs yields zeroed stats
// (Tracked 0, no LastRun) — the tracker cannot tell "unknown hook" from "no
// runs yet", so callers wanting a 404 must consult the registry.
func (t *Tracker) StatsByHook(hookID string) HookRunStats {
	src := t.ListByHook(hookID, 0)
	states := make([]RunState, 0, len(src))
	for _, r := range src {
		states = append(states, r.Snapshot(0))
	}
	stats := ComputeStats(states)
	stats.MaxTracked = t.maxByHook
	return stats
}
