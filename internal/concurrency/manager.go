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
type Manager struct {
	mu     sync.Mutex
	groups map[string]*groupSem
}

type groupSem struct {
	limit   int
	ch      chan struct{} // capacity == limit; a token per active slot
	waiting atomic.Int64  // runs currently blocked waiting for a slot
}

// NewManager builds a Manager from the given config (nil cfg = no groups).
func NewManager(cfg *Config) *Manager {
	m := &Manager{groups: map[string]*groupSem{}}
	m.Update(cfg)
	return m
}

// Update reconfigures the declared groups. Groups whose limit is unchanged
// keep their existing semaphore so in-flight slot accounting survives the
// reload; new or limit-changed groups get a fresh semaphore, and removed
// groups are dropped. Runs already holding a slot release into the exact
// channel they acquired from (the release closure captures it), so a
// reload never loses or double-counts a token.
func (m *Manager) Update(cfg *Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]*groupSem{}
	if cfg != nil {
		for name, g := range cfg.Groups {
			limit := g.Limit
			if limit < 1 {
				limit = DefaultLimit
			}
			if old := m.groups[name]; old != nil && old.limit == limit {
				next[name] = old
				continue
			}
			next[name] = &groupSem{limit: limit, ch: make(chan struct{}, limit)}
		}
	}
	m.groups = next
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

// GroupStatus is a snapshot of one group's live utilization.
type GroupStatus struct {
	Name    string `json:"name"`
	Limit   int    `json:"limit"`
	Active  int    `json:"active"`
	Waiting int    `json:"waiting"`
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
			Name:    name,
			Limit:   gs.limit,
			Active:  len(gs.ch),
			Waiting: int(gs.waiting.Load()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
