package runs

import "time"

// Phase is one instrumentation mark on a run's lifecycle: the wall-clock instant the run reached a named point.
type Phase string

const (
	// PhaseImageReady is when EnsureImage returned — the content-hash image exists. Usually a cache hit; a real build lands here too.
	PhaseImageReady Phase = "image_ready"

	// PhaseSlotAcquired is when the run holds both its concurrency-group slot and a global slot. Everything before it is queue, not container.
	PhaseSlotAcquired Phase = "slot_acquired"

	// PhaseInspected is when the extra `docker inspect` that reconstructs a state hook's real argv returned (see runner.imageCommand).
	PhaseInspected Phase = "inspected"

	// PhaseSpawned is when exec.Command.Start returned for `docker run` — the host has handed off.
	PhaseSpawned Phase = "spawned"

	// PhaseContainerEntry is the first instruction executed INSIDE the container, stamped when the injected shim reports in over the state.
	PhaseContainerEntry Phase = "container_entry"

	// PhaseFirstOutput is when the run's first stdout/stderr line reached the server.
	PhaseFirstOutput Phase = "first_output"

	// PhaseExited is when cmd.Wait returned — the container is gone and `--rm` teardown is done.
	PhaseExited Phase = "exited"
)

// KnownPhase reports whether p is a mark the runner actually stamps. The
// state-API route validates against it so a typo is a 400 rather than an
// unbounded map key.
func KnownPhase(p Phase) bool {
	switch p {
	case PhaseImageReady, PhaseSlotAcquired, PhaseInspected, PhaseSpawned,
		PhaseContainerEntry, PhaseFirstOutput, PhaseExited:
		return true
	}
	return false
}

// Mark stamps p with the current time.
func (r *Run) Mark(p Phase) { r.markAt(p, time.Now().UTC()) }

// markAt is Mark with an explicit instant (tests, and AppendOutput reusing
// the timestamp it already took under the same lock).
func (r *Run) markAt(p Phase, at time.Time) {
	if !KnownPhase(p) {
		return
	}
	r.mu.Lock()
	if !r.state.Finished.IsZero() {
		r.mu.Unlock()
		return
	}
	if _, seen := r.state.Phases[p]; seen {
		r.mu.Unlock()
		return
	}
	if r.state.Phases == nil {
		r.state.Phases = make(map[Phase]time.Time, 4)
	}
	r.state.Phases[p] = at
	r.mu.Unlock()
	// No notifyChange: SetRunning/Finish already carry these, so a per-mark frame would only multiply stream traffic.
}

// markAtLocked is markAt for callers already holding r.mu.
func (r *Run) markAtLocked(p Phase, at time.Time) {
	if _, seen := r.state.Phases[p]; seen {
		return
	}
	if r.state.Phases == nil {
		r.state.Phases = make(map[Phase]time.Time, 4)
	}
	r.state.Phases[p] = at
}

// Phase returns the stamped instant for p, zero when unmarked.
func (r *Run) Phase(p Phase) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Phases[p]
}

// Span returns the duration between two marks and whether both were
// stamped. A missing mark yields (0, false) — never a duration measured
// against the zero time.
func (s RunState) Span(from, to Phase) (time.Duration, bool) {
	a, aok := s.Phases[from]
	b, bok := s.Phases[to]
	if !aok || !bok || a.IsZero() || b.IsZero() || b.Before(a) {
		return 0, false
	}
	return b.Sub(a), true
}

// BootDuration is the container's own startup cost: `docker run` spawned → first instruction inside the container. exact is true only when the in-container mark is present (state hooks).
func (s RunState) BootDuration() (d time.Duration, exact bool, ok bool) {
	if d, ok := s.Span(PhaseSpawned, PhaseContainerEntry); ok {
		return d, true, true
	}
	if d, ok := s.Span(PhaseSpawned, PhaseFirstOutput); ok {
		return d, false, true
	}
	return 0, false, false
}

// RuntimeStartDuration is the hook runtime's cold start: first instruction inside the container → first byte of output.
func (s RunState) RuntimeStartDuration() (time.Duration, bool) {
	return s.Span(PhaseContainerEntry, PhaseFirstOutput)
}

// InspectDuration is the cost of the extra argv-reconstruction `docker
// inspect` state hooks pay before their container starts.
func (s RunState) InspectDuration() (time.Duration, bool) {
	return s.Span(PhaseSlotAcquired, PhaseInspected)
}
