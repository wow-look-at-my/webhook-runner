package runs

import "time"

// HookRunStats aggregates the tracker's retained runs for one hook. Run
// history is memory-only and bounded (the MaxTracked newest runs per hook),
// so these are recent-window figures, not lifetime totals — MaxTracked ships
// in the payload so clients can label the window honestly.
type HookRunStats struct {
	// Tracked is how many runs are currently retained (≤ MaxTracked).
	Tracked int `json:"tracked"`
	// MaxTracked is the tracker's per-hook retention bound (the window size).
	MaxTracked int `json:"max_tracked"`
	// ByStatus counts every retained run, including active ones.
	ByStatus map[Status]int `json:"by_status,omitempty"`
	// Completed counts runs with a terminal status; active (pending/running)
	// runs appear in Tracked and ByStatus but are excluded from the rate and
	// duration figures below.
	Completed int `json:"completed"`
	// SuccessRate is successes/Completed over the retained window. It is 0
	// when Completed is 0 — check Completed before displaying it.
	SuccessRate float64 `json:"success_rate"`
	// AvgDurationMS/MaxDurationMS cover completed runs' Started→Finished
	// span. That span includes any time spent queued behind a concurrency
	// group — the tracker records no separate processing-start time.
	AvgDurationMS int64 `json:"avg_duration_ms"`
	MaxDurationMS int64 `json:"max_duration_ms"`
	// LastRun is the newest retained run by start time, whatever its status
	// — an in-flight run is deliberately included, it IS the latest.
	LastRun *LastRun `json:"last_run,omitempty"`
}

// LastRun identifies one run for HookRunStats without dragging along output.
type LastRun struct {
	ID       string    `json:"id"`
	Status   Status    `json:"status"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// StatsByHook aggregates the retained runs of one hook. A hook with no
// retained runs yields zeroed stats (Tracked 0, no LastRun) — the tracker
// cannot tell "unknown hook" from "no runs yet", so callers wanting a 404
// must consult the registry.
func (t *Tracker) StatsByHook(hookID string) HookRunStats {
	stats := HookRunStats{MaxTracked: t.maxByHook}
	src := t.ListByHook(hookID, 0) // newest first
	if len(src) == 0 {
		return stats
	}
	stats.Tracked = len(src)
	stats.ByStatus = make(map[Status]int)
	var successes int
	var totalDur, maxDur time.Duration
	for i, r := range src {
		snap := r.Snapshot(0)
		stats.ByStatus[snap.Status]++
		if i == 0 {
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
