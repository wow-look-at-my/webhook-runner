package runs

// Parent attribution for spawned runs (the state API's POST /spawn) —
// split from runs.go for the 750-line cap.

// SpawnedBy names the parent run that started a run via the state API's
// POST /spawn.
type SpawnedBy struct {
	RunID  string `json:"run_id"`
	HookID string `json:"hook_id"`
}

// SetSpawnedBy records the parent that started this run through the state
// API's POST /spawn. The runner stamps it at run creation (right after
// tracker.New, like SetTitle), before the run can plausibly be terminal —
// but mirror SetTitle's finished-run refusal anyway: the terminal snapshot
// already flowed into the run store, so a late stamp would diverge live
// state from history. Empty IDs are a no-op (an unspawned run stays
// unattributed rather than half-attributed).
func (r *Run) SetSpawnedBy(parentHookID, parentRunID string) {
	if parentHookID == "" || parentRunID == "" {
		return
	}
	r.mu.Lock()
	if !r.state.Finished.IsZero() {
		r.mu.Unlock()
		return
	}
	r.state.SpawnedBy = &SpawnedBy{RunID: parentRunID, HookID: parentHookID}
	r.mu.Unlock()
	r.notifyChange()
}

// SpawnedBy returns a copy of the run's parent attribution, nil when the
// run was not spawned by another run.
func (r *Run) SpawnedBy() *SpawnedBy {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.SpawnedBy == nil {
		return nil
	}
	cp := *r.state.SpawnedBy
	return &cp
}
