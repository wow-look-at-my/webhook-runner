package concurrency

import "fmt"

// The GLOBAL run cap: one server-wide ceiling on how many hook executions
// may run their containers SIMULTANEOUSLY, across every hook — bounding
// total Docker-container concurrency (each container holds a bridge-network
// IPv4 address; an unbounded delivery flood exhausts the pool: `docker: …
// no available IPv4 addresses on this network's address pools`). At the
// cap, excess executions QUEUE (status pending, watchdog unarmed) and run
// as slots free — never dropped, never errored.
//
// The cap is a Manager pseudo-group under the hood: Global owns a PRIVATE
// Manager declaring exactly one group, so it reuses the proven semaphore
// machinery verbatim — the retired-channel waiter re-bind (a live limit
// change applies to already-queued runs immediately; a raise admits them
// at once), release closures bound to the exact channel they acquired
// from, and the advisory holder/waiter bookkeeping the dashboard renders.
// Because the pseudo-group lives in its own Manager instance, its name can
// never collide with a group declared in concurrency.json.
//
// Configuration precedence (resolved by the caller — cli/serve):
// persisted dashboard override (overrides.Store.GlobalRunLimit) >
// WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS > DefaultGlobalLimit.

// DefaultGlobalLimit is the global run cap when nothing configures one.
const DefaultGlobalLimit = 64

// globalGroup names the pseudo-group inside Global's private Manager. It is
// internal to that Manager instance — declared concurrency.json groups live
// in the server's other Manager, so no collision is possible.
const globalGroup = "global"

// GlobalWaitKey is the waiting_on key a run queued on the global cap
// carries (kind "group") — what the dashboard/timeline show as the thing
// the run waits for. Not a declared group name; display only.
const GlobalWaitKey = globalGroup

// Global is the server-wide run cap. A nil *Global applies no cap (every
// Acquire succeeds immediately) so tests and callers without one need no
// checks; the write methods on nil return an error, never a silent no-op.
type Global struct {
	def int
	mgr *Manager
}

// NewGlobal builds a Global capped at def (values < 1 fall back to
// DefaultGlobalLimit — the cap must never be able to deadlock every run).
func NewGlobal(def int) *Global {
	if def < 1 {
		def = DefaultGlobalLimit
	}
	return &Global{
		def: def,
		mgr: NewManager(&Config{Groups: map[string]Group{globalGroup: {Limit: def}}}),
	}
}

// Default returns the configured default limit — what ClearLimitOverride
// reverts to (the env value, or the built-in DefaultGlobalLimit).
func (g *Global) Default() int {
	if g == nil {
		return 0
	}
	return g.def
}

// Acquire reserves one global run slot, blocking until one is free or
// cancel fires (acquired=false — the caller treats the run as
// cancelled-before-start, exactly like a group acquire). The returned
// release must be called exactly once when the run finishes. onQueue
// follows Manager.Acquire's contract: first call = the run actually has to
// wait, later calls = the queue view changed.
func (g *Global) Acquire(runID string, cancel <-chan struct{}, onQueue func(QueueState)) (release func(), acquired bool) {
	if g == nil {
		return func() {}, true
	}
	release, acquired, err := g.mgr.Acquire(globalGroup, runID, cancel, onQueue)
	if err != nil {
		// Unreachable: the pseudo-group is always declared in the private
		// Manager and never removed by any code path. Fail OPEN (run
		// uncapped) rather than fail a production run over an impossible
		// state.
		return func() {}, true
	}
	return release, acquired
}

// SetLimitOverride swaps the effective cap live (>= 1 enforced by the
// underlying Manager; already-queued runs re-bind immediately — a raise
// admits them at once, holders above a lowered cap finish normally).
func (g *Global) SetLimitOverride(limit int) error {
	if g == nil {
		return fmt.Errorf("global run cap not configured")
	}
	return g.mgr.SetLimitOverride(globalGroup, limit)
}

// ClearLimitOverride reverts the cap to its configured default with the
// same live-swap semantics.
func (g *Global) ClearLimitOverride() {
	if g == nil {
		return
	}
	g.mgr.ClearLimitOverride(globalGroup)
}

// GlobalStatus is the cap's live utilization snapshot: Limit is what gates
// runs right now, Default what ClearLimitOverride reverts to, Overridden
// whether an operator override is in effect.
type GlobalStatus struct {
	Limit      int  `json:"limit"`
	Default    int  `json:"default"`
	Overridden bool `json:"overridden"`
	Active     int  `json:"active"`
	Waiting    int  `json:"waiting"`
}

// Status reports the cap's live utilization. Zero value on nil.
func (g *Global) Status() GlobalStatus {
	if g == nil {
		return GlobalStatus{}
	}
	st := g.mgr.Status()[0] // exactly one group by construction
	return GlobalStatus{
		Limit:      st.Limit,
		Default:    st.Declared,
		Overridden: st.Overridden,
		Active:     st.Active,
		Waiting:    st.Waiting,
	}
}

// QueueDetail returns the cap's advisory holder and waiter lists (the
// dashboard drill-down), with Manager.QueueDetail's semantics.
func (g *Global) QueueDetail() (holders, waiting []GroupRun) {
	if g == nil {
		return nil, nil
	}
	return g.mgr.QueueDetail(globalGroup)
}
