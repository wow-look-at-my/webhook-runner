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
	// Completed counts runs that reached a terminal status BY DOING WORK;
	// active (pending/running) runs and skipped runs appear in Tracked and
	// ByStatus but are excluded from the rate and duration figures below.
	Completed int `json:"completed"`
	// Skipped counts terminal skip_if matches — deliveries answered without
	// booting a container. A distinct bucket on purpose: no work was done,
	// so folding skips into Completed would dilute SuccessRate and the
	// duration/wait figures with zero-length non-runs. They still show in
	// ByStatus (and can be LastRun).
	Skipped int `json:"skipped,omitempty"`
	// SuccessRate is successes/Completed over the window. It is 0 when
	// Completed is 0 — check Completed before displaying it.
	SuccessRate float64 `json:"success_rate"`
	// AvgDurationMS/MaxDurationMS cover completed runs' PROCESSING span
	// (StartedAt→Finished): container time only, queue wait excluded. Runs
	// persisted before StartedAt existed fall back to Started→Finished (the
	// old queued-inclusive span); runs that never started contribute
	// nothing. See ComputeStats for the exact rules.
	AvgDurationMS int64 `json:"avg_duration_ms"`
	MaxDurationMS int64 `json:"max_duration_ms"`
	// AvgWaitMS/MaxWaitMS cover completed runs' QUEUE WAIT
	// (Started→StartedAt): accepted until the container launched — time
	// spent waiting for a concurrency-group slot (plus secrets decrypt and
	// image build). Only the WaitSampled runs that recorded a StartedAt
	// count; pre-upgrade history is excluded.
	AvgWaitMS int64 `json:"avg_wait_ms"`
	MaxWaitMS int64 `json:"max_wait_ms"`
	// WaitSampled is how many completed runs back the wait figures. 0 means
	// no wait data in the window (e.g. only pre-upgrade history) — display
	// "no data", not a zero wait.
	WaitSampled int `json:"wait_sampled"`
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
	var successes, durSampled int
	var totalDur, maxDur, totalWait, maxWait time.Duration
	for i := range states {
		snap := states[i]
		stats.ByStatus[snap.Status]++
		if stats.LastRun == nil || snap.Started.After(stats.LastRun.Started) {
			stats.LastRun = &LastRun{ID: snap.ID, Status: snap.Status, Started: snap.Started, Finished: snap.Finished}
		}
		if !snap.Status.Terminal() {
			continue
		}
		// Skips are terminal but did no work: their own counter, and none of
		// the completion/rate/duration/wait aggregation below.
		if snap.Status == StatusSkipped {
			stats.Skipped++
			continue
		}
		// Terminal implies Finished is set: Finish records both under one
		// lock, and Snapshot reads under the same lock.
		stats.Completed++
		if snap.Status == StatusSuccess {
			successes++
		}
		// Duration is processing-only (StartedAt→Finished) and wait is
		// queue time (Started→StartedAt). Runs without a recorded StartedAt
		// split by status:
		//  - success/failure/timeout: the container certainly ran, but the
		//    state predates the split (pre-upgrade persisted history) — the
		//    duration EXPLICITLY falls back to Finished−Started, the old
		//    queued-inclusive span, rather than dropping the history.
		//  - cancelled/error: the container may never have launched
		//    (cancelled while queued, failed before start) — there is no
		//    processing time, so nothing is aggregated.
		// Either way a run without StartedAt is excluded from the wait
		// figures: its queue wait is unknowable.
		if !snap.StartedAt.IsZero() {
			d := snap.Finished.Sub(snap.StartedAt)
			totalDur += d
			durSampled++
			if d > maxDur {
				maxDur = d
			}
			w := snap.StartedAt.Sub(snap.Started)
			totalWait += w
			stats.WaitSampled++
			if w > maxWait {
				maxWait = w
			}
		} else if snap.Status == StatusSuccess || snap.Status == StatusFailure || snap.Status == StatusTimeout {
			d := snap.Finished.Sub(snap.Started) // legacy fallback: queued-inclusive
			totalDur += d
			durSampled++
			if d > maxDur {
				maxDur = d
			}
		}
	}
	if stats.Completed > 0 {
		stats.SuccessRate = float64(successes) / float64(stats.Completed)
	}
	if durSampled > 0 {
		stats.AvgDurationMS = (totalDur / time.Duration(durSampled)).Milliseconds()
		stats.MaxDurationMS = maxDur.Milliseconds()
	}
	if stats.WaitSampled > 0 {
		stats.AvgWaitMS = (totalWait / time.Duration(stats.WaitSampled)).Milliseconds()
		stats.MaxWaitMS = maxWait.Milliseconds()
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
