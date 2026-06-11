package hooks

import (
	"sort"
	"sync"
)

// Registry is a concurrency-safe mapping from hook ID to the loaded hook
// definition. The watcher updates it as hook.json files appear, change, or
// disappear; the HTTP server reads it to dispatch requests.
type Registry struct {
	mu    sync.RWMutex
	hooks map[string]*Hook
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{hooks: make(map[string]*Hook)}
}

// Replace atomically replaces the entire hook set. This is the path used
// after a full reload of the hooks directory.
func (r *Registry) Replace(hooks map[string]*Hook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = hooks
}

// Set inserts or replaces a single hook.
func (r *Registry) Set(h *Hook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[h.ID] = h
}

// Delete removes a hook by ID. No-op when absent.
func (r *Registry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hooks, id)
}

// Get returns the hook with the given ID, or nil/false if absent.
func (r *Registry) Get(id string) (*Hook, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.hooks[id]
	return h, ok
}

// Summary represents the public-facing description of a hook (no secrets).
type Summary struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Synchronous bool   `json:"synchronous,omitempty"`
}

// All returns every loaded hook, alphabetically sorted by ID. Callers
// must treat the hooks as read-only.
func (r *Registry) All() []*Hook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Hook, 0, len(r.hooks))
	for _, h := range r.hooks {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// List returns a stable, alphabetically-sorted list of hook summaries.
func (r *Registry) List() []Summary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Summary, 0, len(r.hooks))
	for id, h := range r.hooks {
		out = append(out, Summary{ID: id, Description: h.Description, Synchronous: h.Synchronous})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
