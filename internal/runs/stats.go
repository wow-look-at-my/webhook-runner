package runs

import "time"

// HookRunStats aggregates one hook's runs over a caller-defined window: the
// tracker's bounded in-memory window, or that window merged with the
// persisted run-store history. MaxTracked and Retention label the window.
type HookRunStats struct {
	// Tracked is how many runs the window holds.
	Tracked int `json:"tracked"`
	// MaxTracked is the live tracker's per-hook retention bound.
	MaxTracked int `json:"max_tracked"`
	// Retention is the run store's configured retention (e.g. "48h"), when set. Empty means memory-only.
	Retention string `json:"retention,omitempty"`
	// ByStatus counts every run in the window, including active ones.
	ByStatus map[Status]int `json:"by_status,omitempty"`
	// Completed counts runs that reached a terminal status by doing work; excludes active and skipped runs.
	Completed int `json:"completed"`
	// Skipped counts terminal skip_if matches. Kept separate so SuccessRate and duration figures are not diluted by zero-length non-runs.
	Skipped int `json:"skipped,omitempty"`
	// SuccessRate is successes/Completed. It is 0 when Completed is 0 -- check Completed before displaying it.
	SuccessRate float64 `json:"success_rate"`
	// AvgDurationMS/MaxDurationMS cover completed runs' processing span (StartedAt->Finished). See ComputeStats for the fallback rules.
	AvgDurationMS int64 `json:"avg_duration_ms"`
	MaxDurationMS int64 `json:"max_duration_ms"`
	// AvgWaitMS/MaxWaitMS cover completed runs' queue wait (Started->StartedAt).
	AvgWaitMS int64 `json:"avg_wait_ms"`
	MaxWaitMS int64 `json:"max_wait_ms"`
	// WaitSampled is how many completed runs back the wait figures. 0 means no wait data -- display "no data", not a zero wait.
	WaitSampled int `json:"wait_sampled"`
	// LastRun is the newest run by start time, whatever its status -- an in-flight run counts as the latest.
	LastRun *LastRun `json:"last_run,omitempty"`

	// Overhead aggregates the container-startup instrumentation over the same window. nil when no run carries phase marks.
	Overhead *OverheadStats `json:"overhead,omitempty"`
}

// OverheadStats reports container-startup cost for a window of runs. Each
// span carries its own sample count -- see docs/internals/run-phases.md.
type OverheadStats struct {
	// BootAvgMS/BootMaxMS/BootSampled: exact container startup (spawn to first instruction), only for runs with the in-container mark.
	BootAvgMS   int64 `json:"boot_avg_ms"`
	BootMaxMS   int64 `json:"boot_max_ms"`
	BootSampled int   `json:"boot_sampled"`

	// BoundAvgMS/BoundMaxMS/BoundSampled: upper bound (spawn to first output) for runs with no in-container mark. Never quote as exact.
	BoundAvgMS   int64 `json:"bound_avg_ms"`
	BoundMaxMS   int64 `json:"bound_max_ms"`
	BoundSampled int   `json:"bound_sampled"`

	// RuntimeStartAvgMS is the hook runtime's own cold start, same sample set as the exact boot figures.
	RuntimeStartAvgMS int64 `json:"runtime_start_avg_ms"`
	RuntimeStartMaxMS int64 `json:"runtime_start_max_ms"`

	// InspectAvgMS is the argv-reconstruction docker inspect cost state hooks pay before their container starts.
	InspectAvgMS   int64 `json:"inspect_avg_ms"`
	InspectMaxMS   int64 `json:"inspect_max_ms"`
	InspectSampled int   `json:"inspect_sampled"`
}

// LastRun identifies one run for HookRunStats without dragging along output.
type LastRun struct {
	ID       string    `json:"id"`
	Status   Status    `json:"status"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// ComputeStats aggregates snapshotted states of one hook. The caller fills
// the window metadata (MaxTracked, Retention) afterward.
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
		// Newest by start time; ties (same clock tick) favor the run that has
		// not finished, since the dashboard wants "what is running right now".
		newer := stats.LastRun == nil ||
			snap.Started.After(stats.LastRun.Started) ||
			(snap.Started.Equal(stats.LastRun.Started) && stats.LastRun.Finished.IsZero() == snap.Finished.IsZero() && snap.ID > stats.LastRun.ID) ||
			(snap.Started.Equal(stats.LastRun.Started) && !stats.LastRun.Finished.IsZero() && snap.Finished.IsZero())
		if newer {
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
		// Terminal implies Finished is set (Finish records both under one lock).
		stats.Completed++
		if snap.Status == StatusSuccess {
			successes++
		}
		// Duration is StartedAt->Finished; wait is Started->StartedAt. A run
		// missing StartedAt falls back to Finished-Started for success/failure/
		// timeout (pre-upgrade history); cancelled/error contribute nothing,
		// since the container may never have launched.
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
	stats.Overhead = computeOverhead(states)
	return stats
}

// computeOverhead aggregates phase marks across a window, including runs
// that never reached a terminal status, so a real boot is never dropped.
func computeOverhead(states []RunState) *OverheadStats {
	var o OverheadStats
	var bootTotal, bootMax, boundTotal, boundMax time.Duration
	var rtTotal, rtMax, inspTotal, inspMax time.Duration
	for i := range states {
		snap := states[i]
		if len(snap.Phases) == 0 {
			continue
		}
		if d, exact, ok := snap.BootDuration(); ok {
			if exact {
				bootTotal += d
				o.BootSampled++
				if d > bootMax {
					bootMax = d
				}
			} else {
				boundTotal += d
				o.BoundSampled++
				if d > boundMax {
					boundMax = d
				}
			}
		}
		if d, ok := snap.RuntimeStartDuration(); ok {
			rtTotal += d
			if d > rtMax {
				rtMax = d
			}
		}
		if d, ok := snap.InspectDuration(); ok {
			inspTotal += d
			o.InspectSampled++
			if d > inspMax {
				inspMax = d
			}
		}
	}
	if o.BootSampled == 0 && o.BoundSampled == 0 && o.InspectSampled == 0 {
		return nil
	}
	if o.BootSampled > 0 {
		o.BootAvgMS = (bootTotal / time.Duration(o.BootSampled)).Milliseconds()
		o.BootMaxMS = bootMax.Milliseconds()
		// Shares the exact-boot sample set: both need the in-container mark.
		o.RuntimeStartAvgMS = (rtTotal / time.Duration(o.BootSampled)).Milliseconds()
		o.RuntimeStartMaxMS = rtMax.Milliseconds()
	}
	if o.BoundSampled > 0 {
		o.BoundAvgMS = (boundTotal / time.Duration(o.BoundSampled)).Milliseconds()
		o.BoundMaxMS = boundMax.Milliseconds()
	}
	if o.InspectSampled > 0 {
		o.InspectAvgMS = (inspTotal / time.Duration(o.InspectSampled)).Milliseconds()
		o.InspectMaxMS = inspMax.Milliseconds()
	}
	return &o
}

// StatsByHook aggregates the tracker's retained runs of one hook. An unknown
// hook and a hook with no runs both yield zeroed stats -- callers wanting a
// 404 must consult the registry.
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
