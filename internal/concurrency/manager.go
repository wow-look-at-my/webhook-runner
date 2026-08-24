package concurrency

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Manager enforces per-group concurrency limits with one buffered-channel semaphore per declared group. It is safe for concurrent use and can be reconfigured on a hooks reload via Update.
type Manager struct {
	mu        sync.Mutex
	groups    map[string]*groupSem
	overrides map[string]int // group -> operator limit override (>= 1)
	// queues is the advisory holder/waiter bookkeeping, keyed by group name (NOT by groupSem: it must survive semaphore swaps).
	queues map[string]*groupQueue
}

// groupQueue is one group's advisory holder/waiter bookkeeping.
type groupQueue struct {
	holders []holderRec  // runs currently holding slots, acquire order
	waiting []*waiterRec // runs blocked on a slot, registration order
}

type holderRec struct {
	id    string
	since time.Time
}

type waiterRec struct {
	id     string
	since  time.Time
	notify func(QueueState)
}

// QueueState is a queued run's live place in its group's queue, delivered to the Acquire onQueue callback: who holds the slots right now and.
type QueueState struct {
	Holders  []string
	Position int
}

// GroupRun is one run in a group's advisory queue detail: a holder (Since =
// when it took the slot) or a waiter (Since = when it started waiting).
type GroupRun struct {
	ID    string
	Since time.Time
}

// QueueDetail returns a group's advisory holder and waiter lists (holders
// in acquire order, waiters in registration order). Copies, safe to retain.
func (m *Manager) QueueDetail(group string) (holders, waiting []GroupRun) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[group]
	if q == nil {
		return nil, nil
	}
	for _, h := range q.holders {
		holders = append(holders, GroupRun{ID: h.id, Since: h.since})
	}
	for _, w := range q.waiting {
		waiting = append(waiting, GroupRun{ID: w.id, Since: w.since})
	}
	return holders, waiting
}

// queueFor returns (creating if needed) the advisory record for a group.
// Caller holds m.mu.
func (m *Manager) queueFor(group string) *groupQueue {
	q := m.queues[group]
	if q == nil {
		q = &groupQueue{}
		m.queues[group] = q
	}
	return q
}

// pruneQueue drops a group's advisory record once it is empty. Caller
// holds m.mu.
func (m *Manager) pruneQueue(group string) {
	if q := m.queues[group]; q != nil && len(q.holders) == 0 && len(q.waiting) == 0 {
		delete(m.queues, group)
	}
}

// addHolder / dropHolder / addWaiter / dropWaiter maintain the advisory
// lists. Empty run IDs (callers without an identity) are not tracked.
// Callers hold m.mu.
func (m *Manager) addHolder(group, id string) {
	if id == "" {
		return
	}
	m.queueFor(group).holders = append(m.queueFor(group).holders, holderRec{id: id, since: time.Now().UTC()})
}

func (m *Manager) dropHolder(group, id string) {
	q := m.queues[group]
	if q == nil || id == "" {
		return
	}
	for i, h := range q.holders {
		if h.id == id {
			q.holders = append(q.holders[:i], q.holders[i+1:]...)
			break
		}
	}
	m.pruneQueue(group)
}

func (m *Manager) dropWaiter(group string, w *waiterRec) {
	q := m.queues[group]
	if q == nil {
		return
	}
	for i, cand := range q.waiting {
		if cand == w {
			q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
			break
		}
	}
	m.pruneQueue(group)
}

// queueState builds the QueueState a waiter sees right now. Caller holds
// m.mu; the holder slice is a fresh copy.
func (m *Manager) queueState(group string, w *waiterRec) QueueState {
	q := m.queues[group]
	st := QueueState{Position: 0}
	if q == nil {
		return st
	}
	st.Holders = make([]string, 0, len(q.holders))
	for _, h := range q.holders {
		st.Holders = append(st.Holders, h.id)
	}
	for i, cand := range q.waiting {
		if cand == w {
			st.Position = i + 1
			break
		}
	}
	return st
}

// notifyWaiters re-delivers fresh QueueStates to every waiter of a group whose view changed (a holder came or went, or the line moved).
func (m *Manager) notifyWaiters(group string) {
	q := m.queues[group]
	if q == nil {
		return
	}
	for _, w := range q.waiting {
		if w.notify != nil {
			w.notify(m.queueState(group, w))
		}
	}
}

type groupSem struct {
	declared   int  // limit from concurrency.json
	limit      int  // effective limit (declared, or the operator override)
	overridden bool // limit came from an operator override
	// ch is the semaphore: capacity == limit, a token per active slot.
	ch chan struct{}
	// retired is closed — exactly once, when this groupSem is replaced by a limit change or its group is removed from the config — as the signal.
	retired chan struct{}
	waiting atomic.Int64 // runs currently blocked waiting for a slot
}

// newGroupSem is the sole groupSem constructor: every swap site goes through
// it so retired is never nil (a nil channel would silently disable waiter
// re-binding).
func newGroupSem(declared, limit int, overridden bool) *groupSem {
	return &groupSem{declared: declared, limit: limit, overridden: overridden, ch: make(chan struct{}, limit), retired: make(chan struct{})}
}

// NewManager builds a Manager from the given config (nil cfg = no groups).
func NewManager(cfg *Config) *Manager {
	m := &Manager{groups: map[string]*groupSem{}, overrides: map[string]int{}, queues: map[string]*groupQueue{}}
	m.Update(cfg)
	return m
}

// Update reconfigures the declared groups, applying any operator limit overrides on top (an override wins over the declared limit — a reload re-applies it rather than silently reverting the operator's change). Groups whose effective limit is unchanged keep their existing semaphore so in-flight slot accounting survives the reload; new or limit-changed groups get a fresh semaphore, and removed groups are dropped (their overrides stay stored, inert, and re-apply if the group is re-declared). Runs already holding a slot release into the exact channel they acquired from (the release closure captures it), so a reload never loses or double-counts a token. Runs already QUEUED re-bind to the group's new semaphore (see groupSem.retired), so a changed limit takes effect for them immediately; a queued run whose group is removed fails its Acquire with an error instead of blocking forever.
func (m *Manager) Update(cfg *Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]*groupSem{}
	if cfg != nil {
		for name, g := range cfg.Groups {
			declared := g.Limit
			if declared < 1 {
				declared = DefaultLimit
			}
			effective, overridden := declared, false
			if ov, ok := m.overrides[name]; ok {
				effective, overridden = ov, true
			}
			if old := m.groups[name]; old != nil && old.limit == effective {
				// Same effective limit: keep the semaphore (and its slot accounting); refresh the reporting fields, which are only ever read under m.mu.
				old.declared = declared
				old.overridden = overridden
				next[name] = old
				continue
			}
			next[name] = newGroupSem(declared, effective, overridden)
		}
	}
	// Retire every semaphore not carried into the new config — the replaced sem of a limit-changed group AND a removed group's — so blocked waiters.
	for name, old := range m.groups {
		if next[name] != old {
			close(old.retired)
		}
	}
	m.groups = next
}

// SetLimitOverride records an operator override for group's limit and, when the group is currently declared, swaps its semaphore live. limit must be >= 1: a 0 limit would leave queued runs blocked forever (disable the hooks instead). Recording is unconditional — an override for a group not currently declared stays inert and takes effect if a later Update declares it (the "override survives the group briefly disappearing from a reload" contract).
func (m *Manager) SetLimitOverride(group string, limit int) error {
	if limit < 1 {
		return fmt.Errorf("concurrency override for %q: limit must be >= 1, got %d", group, limit)
	}
	if m == nil {
		return fmt.Errorf("concurrency manager not configured")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overrides[group] = limit
	gs := m.groups[group]
	if gs == nil {
		return nil // not currently declared: inert until a reload declares it
	}
	if gs.limit == limit {
		gs.overridden = true // effective limit unchanged: keep the semaphore
		return nil
	}
	m.groups[group] = newGroupSem(gs.declared, limit, true)
	close(gs.retired)
	return nil
}

// ClearLimitOverride removes group's operator override, reverting the group
// (when currently declared) to its declared limit with the same live-swap
// semantics as SetLimitOverride. Clearing an absent override is a no-op.
func (m *Manager) ClearLimitOverride(group string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.overrides, group)
	gs := m.groups[group]
	if gs == nil {
		return
	}
	if gs.limit == gs.declared {
		gs.overridden = false // effective limit unchanged: keep the semaphore
		return
	}
	m.groups[group] = newGroupSem(gs.declared, gs.declared, false)
	close(gs.retired)
}

// Declared reports whether group is currently declared, and its declared
// (pre-override) limit.
func (m *Manager) Declared(group string) (limit int, ok bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	gs := m.groups[group]
	if gs == nil {
		return 0, false
	}
	return gs.declared, true
}

// Acquire reserves a slot in the named group, blocking until one is free
// or cancel fires. It returns a release func (call it exactly once when the
// run finishes) and acquired=true on success.
//
//   - An empty group means "unbounded": it returns immediately with a
//     no-op release.
//   - cancel firing before a slot frees returns acquired=false, err=nil
//     (the caller should treat the run as cancelled-before-start).
//   - A group not declared in the current config returns an error; the
//     runner fails such a run closed rather than running it unbounded.
//   - runID identifies the caller in the group's advisory queue bookkeeping
//     (QueueDetail, QueueState.Holders). Empty is allowed: the caller gates
//     normally but stays invisible in the queue views.
//   - onQueue, if non-nil, is called the moment the run has to actually
//     queue (no slot was immediately available) — callers use that first
//     call to record a one-time "queued" event — and again every time the
//     queued run's view changes (a holder came or went, the line moved),
//     each time with a fresh QueueState. Calls are serialized under the
//     Manager's mutex: they must be fast and must not call back into the
//     Manager. A run that gets its slot immediately never sees onQueue.
//   - A limit change while queued (reload or operator override) re-binds
//     the waiter to the group's new semaphore, so a raised limit admits
//     queued runs immediately; if the group itself is removed from the
//     config while queued, Acquire fails with an error (the runner surfaces
//     that as a failed run — loud beats silently stranded).
func (m *Manager) Acquire(group, runID string, cancel <-chan struct{}, onQueue func(QueueState)) (release func(), acquired bool, err error) {
	if group == "" {
		return func() {}, true, nil
	}
	if m == nil {
		return nil, false, fmt.Errorf("concurrency group %q is not configured", group)
	}
	m.mu.Lock()
	gs := m.groups[group]
	if gs == nil {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("unknown concurrency group %q", group)
	}

	// Fast path: grab a free slot without reporting a queue wait. Taken
	// under m.mu so the holder registration is atomic with the grab.
	select {
	case gs.ch <- struct{}{}:
		m.addHolder(group, runID)
		m.mu.Unlock()
		return m.releaser(group, runID, gs.ch), true, nil
	default:
	}

	// Queue: register in the advisory wait line and tell the caller it is actually waiting (first onQueue call = the old one-shot onWait).
	w := &waiterRec{id: runID, since: time.Now().UTC(), notify: onQueue}
	if runID != "" {
		m.queueFor(group).waiting = append(m.queueFor(group).waiting, w)
	}
	if onQueue != nil {
		onQueue(m.queueState(group, w))
	}
	m.mu.Unlock()

	// Block for a slot. The loop re-arms on every semaphore swap: a limit
	// change (reload or operator override) closes the retired channel of
	// the semaphore it replaces, and each blocked waiter re-binds to the
	// group's CURRENT semaphore — so a raised limit admits already-queued
	// runs immediately, and a lowered one has them contend at the new limit.
	// The per-sem waiting counter is managed per iteration so it always
	// tracks the semaphore this waiter is actually blocked on.
	for {
		gs.waiting.Add(1)
		select {
		case gs.ch <- struct{}{}:
			gs.waiting.Add(-1)
			m.mu.Lock()
			m.dropWaiter(group, w)
			m.addHolder(group, runID)
			// Everyone still waiting sees a new holder and a shorter line.
			m.notifyWaiters(group)
			m.mu.Unlock()
			return m.releaser(group, runID, gs.ch), true, nil
		case <-cancel:
			gs.waiting.Add(-1)
			m.mu.Lock()
			m.dropWaiter(group, w)
			m.notifyWaiters(group)
			m.mu.Unlock()
			return nil, false, nil
		case <-gs.retired:
			// The semaphore this waiter was blocked on has been replaced (limit change) or dropped (group removed). Re-bind.
			gs.waiting.Add(-1)
			m.mu.Lock()
			next := m.groups[group]
			if next == nil {
				m.dropWaiter(group, w)
				m.notifyWaiters(group)
				m.mu.Unlock()
				return nil, false, fmt.Errorf("concurrency group %q was removed while queued", group)
			}
			gs = next
			// A raised limit usually means the new semaphore has room: take
			// a slot right here, atomically with the holder registration.
			select {
			case gs.ch <- struct{}{}:
				m.dropWaiter(group, w)
				m.addHolder(group, runID)
				m.notifyWaiters(group)
				m.mu.Unlock()
				return m.releaser(group, runID, gs.ch), true, nil
			default:
			}
			// Still full at the new limit: stay in the advisory line (position and holder views re-derive from the name-keyed bookkeeping) and re-block.
			m.mu.Unlock()
		}
	}
}

// releaser returns a single-use release closure for the given channel.
func (m *Manager) releaser(group, runID string, ch chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-ch
			m.mu.Lock()
			m.dropHolder(group, runID)
			m.notifyWaiters(group)
			m.mu.Unlock()
		})
	}
}

// GroupStatus is a snapshot of one group's live utilization.
type GroupStatus struct {
	Name       string `json:"name"`
	Limit      int    `json:"limit"`
	Declared   int    `json:"declared"`
	Overridden bool   `json:"overridden"`
	Active     int    `json:"active"`
	Waiting    int    `json:"waiting"`
}

// Status returns the live utilization of every declared group, sorted by
// name. Useful for the admin port to answer "is ollama-local saturated,
// and how many runs are queued behind it?".
func (m *Manager) Status() []GroupStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]GroupStatus, 0, len(m.groups))
	for name, gs := range m.groups {
		out = append(out, GroupStatus{
			Name:       name,
			Limit:      gs.limit,
			Declared:   gs.declared,
			Overridden: gs.overridden,
			Active:     len(gs.ch),
			Waiting:    int(gs.waiting.Load()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
