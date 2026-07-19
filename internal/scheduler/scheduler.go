// Package scheduler fires hooks on a fixed per-hook interval, reusing the
// normal run pipeline so a scheduled run looks like any other run (tracked,
// gated by its concurrency group, KV-enabled, visible on the dashboard).
//
// Like internal/concurrency's Manager, this is a PURE component: it owns only
// timing. The actual run dispatch is a caller-supplied Fire callback, so the
// scheduler needs no dependency on the runner, registry, or run tracker and is
// trivially testable with an injected clock. cli/serve.go wires Fire to look
// up the hook, apply skip-if-already-running overlap protection, and call
// runner.Start with context.Background() (like other async runs).
//
// A hook opts in via hook.json's "schedule" (a Go duration, e.g. "5m"). The
// set of scheduled hooks is reconciled through Update on every hooks reload —
// the same buildLoadAndApply closure that updates the registry and the
// concurrency manager — so schedules never drift from the loaded hooks.
//
// Missed-tick / restart behavior: a newly seen schedule (server startup, a
// newly added hook, or a changed interval) fires IMMEDIATELY, then every
// interval thereafter; an unrelated reload preserves an unchanged schedule's
// next-fire time, so it is not re-fired spuriously. A restart or deploy is
// exactly when a catch-up sweep is wanted, and scheduled work is expected to
// be idempotent, so fire-immediately is the safe default.
package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"
)

// DefaultTick is how often the run loop checks for due hooks. Schedules are
// minute-scale in practice, so one-second resolution is plenty and cheap; it
// also naturally caps the effective firing rate at ~1/s regardless of how
// short an interval a hook declares.
const DefaultTick = time.Second

// Scheduler fires hooks on fixed intervals via a caller-supplied callback.
type Scheduler struct {
	fire func(hookID string)
	now  func() time.Time
	tick time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	interval time.Duration
	next     time.Time
}

// Options configure a Scheduler. Fire is required.
type Options struct {
	// Fire dispatches a run for the given hook. It must not block for long
	// (it is called inline on the tick goroutine); the serve.go wiring kicks
	// off an async run and returns immediately.
	Fire func(hookID string)
	// Now is an injectable clock for tests; defaults to time.Now.
	Now func() time.Time
	// Tick is the run-loop resolution; defaults to DefaultTick.
	Tick time.Duration
}

// New constructs a Scheduler. Fire must be non-nil.
func New(opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	return &Scheduler{
		fire:    opts.Fire,
		now:     opts.Now,
		tick:    opts.Tick,
		entries: map[string]*entry{},
	}
}

// Update reconciles the scheduled hooks against the given map of hook ID ->
// interval. A new schedule (or one whose interval changed) is set to fire
// immediately; an unchanged schedule keeps its existing next-fire time so an
// unrelated reload never re-fires it. Non-positive intervals and removed
// hooks are dropped.
func (s *Scheduler) Update(schedules map[string]time.Duration) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]*entry, len(schedules))
	for id, iv := range schedules {
		if iv <= 0 {
			continue
		}
		if old := s.entries[id]; old != nil && old.interval == iv {
			next[id] = old // preserve next-fire across an unrelated reload
			continue
		}
		next[id] = &entry{interval: iv, next: now} // new or changed -> fire immediately
	}
	s.entries = next
}

// Run drives the scheduler until ctx is cancelled. Cancelling ctx stops
// scheduling but never affects runs already dispatched (Fire uses
// context.Background()).
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.fireDue()
		}
	}
}

// fireDue fires every hook whose next-fire time has passed and advances each
// by its interval. The next fire is computed from "now", not the missed
// deadline, so a long pause (a slept/restarted process) fires a hook once and
// resumes one interval out rather than bursting a backlog of missed ticks.
func (s *Scheduler) fireDue() {
	now := s.now()
	var due []string
	s.mu.Lock()
	for id, e := range s.entries {
		if !now.Before(e.next) {
			due = append(due, id)
			e.next = now.Add(e.interval)
		}
	}
	s.mu.Unlock()
	// Deterministic order so behavior (and tests) don't depend on map
	// iteration order when several hooks come due on the same tick.
	sort.Strings(due)
	for _, id := range due {
		s.fire(id)
	}
}

// Schedules returns a snapshot of the current schedule (hook ID -> interval).
func (s *Scheduler) Schedules() map[string]time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Duration, len(s.entries))
	for id, e := range s.entries {
		out[id] = e.interval
	}
	return out
}
