package concurrency

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Manager enforces per-group concurrency limits with one buffered-channel
// semaphore per declared group. It is safe for concurrent use and can be
// reconfigured on a hooks reload via Update.
//
// The operator can override a group's limit at runtime (SetLimitOverride /
// ClearLimitOverride, driven by the admin port). Overrides are held inside
// the Manager and re-applied by Update itself, so a hooks reload can never
// open a window where the declared limit is briefly back in effect — the
// effective limit changes atomically with the reload.
type Manager struct {
	mu        sync.Mutex
	groups    map[string]*groupSem
	overrides map[string]int // group -> operator limit override (>= 1)
}

type groupSem struct {
	declared   int  // limit from concurrency.json
	limit      int  // effective limit (declared, or the operator override)
	overridden bool // limit came from an operator override
	// ch is the semaphore: capacity == limit, a token per active slot. It is
	// immutable for the life of this groupSem — a limit change (reload or
	// operator override) swaps in a whole new groupSem, and in-flight runs
	// release into the exact channel they acquired from (the release closure
	// captures it), so a swap never loses or double-counts a token.
	ch      chan struct{}
	waiting atomic.Int64 // runs currently blocked waiting for a slot
}

// NewManager builds a Manager from the given config (nil cfg = no groups).
func NewManager(cfg *Config) *Manager {
	m := &Manager{groups: map[string]*groupSem{}, overrides: map[string]int{}}
	m.Update(cfg)
	return m
}

// Update reconfigures the declared groups, applying any operator limit
// overrides on top (an override wins over the declared limit — a reload
// re-applies it rather than silently reverting the operator's change).
// Groups whose effective limit is unchanged keep their existing semaphore
// so in-flight slot accounting survives the reload; new or limit-changed
// groups get a fresh semaphore, and removed groups are dropped (their
// overrides stay stored, inert, and re-apply if the group is re-declared).
// Runs already holding a slot release into the exact channel they acquired
// from (the release closure captures it), so a reload never loses or
// double-counts a token.
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
				// Same effective limit: keep the semaphore (and its slot
				// accounting); refresh the reporting fields, which are only
				// ever read under m.mu.
				old.declared = declared
				old.overridden = overridden
				next[name] = old
				continue
			}
			next[name] = &groupSem{declared: declared, limit: effective, overridden: overridden, ch: make(chan struct{}, effective)}
		}
	}
	m.groups = next
}

// SetLimitOverride records an operator override for group's limit and, when
// the group is currently declared, swaps its semaphore live. limit must be
// >= 1: a 0 limit would leave queued runs blocked forever (disable the
// hooks instead). Recording is unconditional — an override for a group not
// currently declared stays inert and takes effect if a later Update
// declares it (the "override survives the group briefly disappearing from
// a reload" contract).
//
// The swap is safe with runs in flight: each run's release closure captured
// the channel it acquired from, so runs holding slots in the old semaphore
// release into it (never into the new one), and new acquires see only the
// new semaphore. No token is lost or double-counted. Like a reload that
// changes a limit, runs already active beyond a lowered limit finish
// normally; the new limit gates new acquires immediately.
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
	m.groups[group] = &groupSem{declared: gs.declared, limit: limit, overridden: true, ch: make(chan struct{}, limit)}
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
	m.groups[group] = &groupSem{declared: gs.declared, limit: gs.declared, ch: make(chan struct{}, gs.declared)}
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
//   - onWait, if non-nil, is called at most once — the moment the run has
//     to actually queue (no slot was immediately available). Callers use
//     it to record a "queued" event without spamming one per run.
func (m *Manager) Acquire(group string, cancel <-chan struct{}, onWait func()) (release func(), acquired bool, err error) {
	if group == "" {
		return func() {}, true, nil
	}
	if m == nil {
		return nil, false, fmt.Errorf("concurrency group %q is not configured", group)
	}
	m.mu.Lock()
	gs := m.groups[group]
	m.mu.Unlock()
	if gs == nil {
		return nil, false, fmt.Errorf("unknown concurrency group %q", group)
	}

	// Fast path: grab a free slot without reporting a queue wait.
	select {
	case gs.ch <- struct{}{}:
		return releaser(gs.ch), true, nil
	default:
	}

	if onWait != nil {
		onWait()
	}
	gs.waiting.Add(1)
	defer gs.waiting.Add(-1)
	select {
	case gs.ch <- struct{}{}:
		return releaser(gs.ch), true, nil
	case <-cancel:
		return nil, false, nil
	}
}

// releaser returns a single-use release closure for the given channel.
func releaser(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { <-ch }) }
}

// GroupStatus is a snapshot of one group's live utilization. Limit is the
// EFFECTIVE limit (what actually gates runs right now); Declared is the
// limit from concurrency.json, and Overridden marks the two differing
// because of an operator override.
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
