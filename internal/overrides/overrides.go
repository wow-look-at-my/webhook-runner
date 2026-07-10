// Package overrides persists the operator's runtime overrides — the "big
// red switch" on the admin dashboard: per-hook kill switches (reject
// deliveries, skip scheduled runs) and concurrency-group limit overrides.
//
// Overrides are OPERATIONAL state, not hooks-repo config: they live in one
// JSON file under the data dir (next to runs.db and kv/), survive restarts,
// and are re-applied on top of every hooks-repo reload — flipping a switch
// never requires a config PR, and a config push never silently wipes a
// switch. An override whose hook/group disappears from the repo is kept
// inert (announced via an `override.orphaned` event, never dropped) and
// re-applies if the target comes back.
//
// Persistence follows internal/kv's rules: writes are atomic (temp+rename)
// and a persist failure rolls the in-memory mutation back so memory never
// diverges from disk — callers surface the error loudly (HTTP 500 + an
// activity event), never a quiet degrade.
package overrides

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store holds the operator overrides, mirrored to one JSON file. A nil
// *Store is valid for the read methods (no overrides), so servers built
// without one — tests, mostly — need no nil checks on the hot paths; the
// write methods on a nil Store return an error (never a silent no-op).
type Store struct {
	mu       sync.Mutex
	path     string
	disabled map[string]struct{} // hook IDs the operator has switched off
	limits   map[string]int      // concurrency group -> operator limit override
}

// fileFormat is the on-disk JSON shape.
type fileFormat struct {
	DisabledHooks     []string       `json:"disabled_hooks,omitempty"`
	ConcurrencyLimits map[string]int `json:"concurrency_limits,omitempty"`
}

// Open loads the overrides file at path, creating the parent directory if
// needed. A missing file is the zero state (no overrides). A file that
// exists but does not parse is a hard error: booting with the operator's
// kill switches silently dropped is exactly the failure this package
// exists to prevent, so startup fails loudly instead.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("overrides: path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("overrides: create dir: %w", err)
	}
	s := &Store{
		path:     path,
		disabled: map[string]struct{}{},
		limits:   map[string]int{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("overrides: read %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("overrides: parse %s (refusing to start with operator overrides unreadable): %w", path, err)
	}
	for _, id := range f.DisabledHooks {
		s.disabled[id] = struct{}{}
	}
	for g, n := range f.ConcurrencyLimits {
		s.limits[g] = n
	}
	return s, nil
}

// HookDisabled reports whether the operator has switched the hook off.
func (s *Store) HookDisabled(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, off := s.disabled[id]
	return off
}

// DisabledHooks returns the sorted hook IDs currently switched off.
func (s *Store) DisabledHooks() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.disabled))
	for id := range s.disabled {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SetHookDisabled flips one hook's kill switch and persists immediately.
// It is idempotent: changed=false means the switch was already in that
// position (and nothing was written). A persist failure rolls the flip
// back and returns the error.
func (s *Store) SetHookDisabled(id string, disabled bool) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cur := s.disabled[id]
	if cur == disabled {
		return false, nil
	}
	if disabled {
		s.disabled[id] = struct{}{}
	} else {
		delete(s.disabled, id)
	}
	if err := s.persistLocked(); err != nil {
		if disabled {
			delete(s.disabled, id)
		} else {
			s.disabled[id] = struct{}{}
		}
		return false, err
	}
	return true, nil
}

// ConcurrencyLimit returns the operator's limit override for group, if set.
func (s *Store) ConcurrencyLimit(group string) (int, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.limits[group]
	return n, ok
}

// ConcurrencyLimits returns a copy of every group limit override.
func (s *Store) ConcurrencyLimits() map[string]int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.limits))
	for g, n := range s.limits {
		out[g] = n
	}
	return out
}

// SetConcurrencyLimit records a limit override for group and persists
// immediately. limit must be >= 1 — a zero limit would leave queued runs
// waiting forever (disable the hooks instead). Idempotent (changed=false
// when the same override was already set); a persist failure rolls the
// mutation back and returns the error.
func (s *Store) SetConcurrencyLimit(group string, limit int) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	if limit < 1 {
		return false, fmt.Errorf("overrides: concurrency limit for %q must be >= 1, got %d", group, limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.limits[group]
	if had && prev == limit {
		return false, nil
	}
	s.limits[group] = limit
	if err := s.persistLocked(); err != nil {
		if had {
			s.limits[group] = prev
		} else {
			delete(s.limits, group)
		}
		return false, err
	}
	return true, nil
}

// ClearConcurrencyLimit removes group's limit override and persists
// immediately. Idempotent (changed=false when no override was set); a
// persist failure rolls the removal back and returns the error.
func (s *Store) ClearConcurrencyLimit(group string) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.limits[group]
	if !had {
		return false, nil
	}
	delete(s.limits, group)
	if err := s.persistLocked(); err != nil {
		s.limits[group] = prev
		return false, err
	}
	return true, nil
}

// persistLocked atomically rewrites the overrides file via temp+rename
// (same discipline as internal/kv). Callers hold s.mu.
func (s *Store) persistLocked() error {
	f := fileFormat{ConcurrencyLimits: s.limits}
	for id := range s.disabled {
		f.DisabledHooks = append(f.DisabledHooks, id)
	}
	sort.Strings(f.DisabledHooks) // stable output for humans and diffs
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("overrides: marshal: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".overrides.json.tmp-*")
	if err != nil {
		return fmt.Errorf("overrides: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("overrides: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("overrides: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("overrides: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("overrides: rename: %w", err)
	}
	return nil
}
