package runs

import "time"

// Phase is one instrumentation mark on a run's lifecycle: the wall-clock
// instant the run reached a named point. Marks are stamped by whoever
// observes the transition (the runner for everything host-side, the
// in-container shim for ContainerEntry) and are purely observational —
// nothing branches on them.
//
// They exist because the cost of running a hook in a container was argued
// about from first principles and never measured. A mark set is the
// evidence: with them, "docker overhead" is a number per run, split from
// the hook runtime's own cold start, instead of an estimate.
type Phase string

const (
	// PhaseImageReady is when EnsureImage returned — the content-hash image
	// exists. Usually a cache hit; a real build lands here too.
	PhaseImageReady Phase = "image_ready"

	// PhaseSlotAcquired is when the run holds both its concurrency-group
	// slot and a global slot. Everything before it is queue, not container.
	PhaseSlotAcquired Phase = "slot_acquired"

	// PhaseInspected is when the extra `docker inspect` that reconstructs a
	// state hook's real argv returned (see runner.imageCommand). Only state
	// hooks pay it; it is marked separately because it is a whole extra
	// docker CLI + daemon round trip on the critical path.
	PhaseInspected Phase = "inspected"

	// PhaseSpawned is when exec.Command.Start returned for `docker run` —
	// the host has handed off. Everything after it and before
	// PhaseContainerEntry is Docker's own create/namespace/overlay cost.
	PhaseSpawned Phase = "spawned"

	// PhaseContainerEntry is the first instruction executed INSIDE the
	// container, stamped when the injected shim reports in over the state
	// socket before it execs the hook's real command. Present for state
	// hooks only — nothing is injected into other hooks, so they have no
	// in-container vantage point and their boot cost is only bounded from
	// above (see PhaseFirstOutput).
	PhaseContainerEntry Phase = "container_entry"

	// PhaseFirstOutput is when the run's first stdout/stderr line reached
	// the server. For a state hook it closes the runtime-start span
	// (interpreter boot, imports, whatever the hook does before printing);
	// for every other hook it is the only upper bound on boot available.
	PhaseFirstOutput Phase = "first_output"

	// PhaseExited is when cmd.Wait returned — the container is gone and
	// `--rm` teardown is done. Finished−Exited is the runner's own
	// bookkeeping tail.
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

// Mark stamps p with the current time. FIRST WRITE WINS: a phase is a point
// the run passed through once, and both output streams race to stamp
// PhaseFirstOutput. Marking an unknown phase, or a run that already
// finished, is a no-op — history is immutable once persisted.
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
	// Deliberately no notifyChange: marks land in bursts around container
	// launch, they are carried by the deltas the surrounding lifecycle
	// transitions already emit (SetRunning, Finish), and a per-mark SSE
	// frame would multiply stream traffic to say nothing new.
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

// BootDuration is the container's own startup cost: `docker run` spawned →
// first instruction inside the container. exact is true only when the
// in-container mark is present (state hooks). Otherwise it falls back to
// spawned → first output, which is an UPPER BOUND: it also contains the
// hook runtime's cold start. Callers must not present an inexact value as
// docker's cost — that conflation is the whole reason for the split.
func (s RunState) BootDuration() (d time.Duration, exact bool, ok bool) {
	if d, ok := s.Span(PhaseSpawned, PhaseContainerEntry); ok {
		return d, true, true
	}
	if d, ok := s.Span(PhaseSpawned, PhaseFirstOutput); ok {
		return d, false, true
	}
	return 0, false, false
}

// RuntimeStartDuration is the hook runtime's cold start: first instruction
// inside the container → first byte of output. State hooks only (it needs
// the in-container mark); ok is false otherwise.
func (s RunState) RuntimeStartDuration() (time.Duration, bool) {
	return s.Span(PhaseContainerEntry, PhaseFirstOutput)
}

// InspectDuration is the cost of the extra argv-reconstruction `docker
// inspect` state hooks pay before their container starts.
func (s RunState) InspectDuration() (time.Duration, bool) {
	return s.Span(PhaseSlotAcquired, PhaseInspected)
}
