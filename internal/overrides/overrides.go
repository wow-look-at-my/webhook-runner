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
//
// The hook kill switch is TRI-STATE: per hook the store holds either no
// override (the hook.json `enable` default applies) or an EXPLICIT
// enabled/disabled override. Explicit both ways matters because a hook may
// default to disabled (`"enable": false` in hook.json): enabling it must
// persist a positive override, not merely remove a disable.
type Store struct {
	mu         sync.Mutex
	path       string
	hookEnable map[string]bool // hook ID -> explicit override (true=enabled, false=disabled); absent = no override
	limits     map[string]int  // concurrency group -> operator limit override
}

// fileFormat is the on-disk JSON shape.
type fileFormat struct {
	// DisabledHooks is the legacy shape (a plain kill-switch set) and is
	// still written — the IDs whose override is an explicit disable — so a
	// binary downgrade keeps honoring kill switches (old readers ignore
	// hook_enable). On read it seeds explicit-disable overrides;
	// HookEnable entries win over it.
	DisabledHooks []string `json:"disabled_hooks,omitempty"`
	// HookEnable is the authoritative tri-state map: an entry is an
	// explicit operator override (true=enabled, false=disabled) that takes
	// precedence over the hook's hook.json `enable` default; an absent
	// hook has no override and its default applies.
	HookEnable        map[string]bool `json:"hook_enable,omitempty"`
	ConcurrencyLimits map[string]int  `json:"concurrency_limits,omitempty"`
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
		path:       path,
		hookEnable: map[string]bool{},
		limits:     map[string]int{},
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
	// Legacy compatibility: a pre-tri-state file carries only the disabled
	// set — each entry reads back as an explicit-disable override, so an
	// upgrade never loses a persisted kill switch. hook_enable entries
	// (written alongside by this version) win over it.
	for _, id := range f.DisabledHooks {
		s.hookEnable[id] = false
	}
	for id, enabled := range f.HookEnable {
		s.hookEnable[id] = enabled
	}
	for g, n := range f.ConcurrencyLimits {
		s.limits[g] = n
	}
	return s, nil
}

// HookOverride returns the operator's explicit enable/disable override for
// the hook: (state, true) when one exists, (_, false) when the operator has
// never overridden this hook and its hook.json `enable` default applies.
func (s *Store) HookOverride(id string) (enabled, ok bool) {
	if s == nil {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	enabled, ok = s.hookEnable[id]
	return enabled, ok
}

// HookDisabled reports the hook's EFFECTIVE kill-switch state: the
// operator's explicit override when one exists, else the hook.json default
// the caller passes (defaultEnabled — hooks.Hook.EnabledByDefault; callers
// without a loaded hook to read it from pass true, so a hook that failed to
// load counts as enabled unless explicitly overridden off).
func (s *Store) HookDisabled(id string, defaultEnabled bool) bool {
	if enabled, ok := s.HookOverride(id); ok {
		return !enabled
	}
	return !defaultEnabled
}

// HookOverrides returns a copy of every explicit hook override
// (true=enabled, false=disabled).
func (s *Store) HookOverrides() map[string]bool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.hookEnable))
	for id, enabled := range s.hookEnable {
		out[id] = enabled
	}
	return out
}

// DisabledHooks returns the sorted hook IDs with an explicit DISABLE
// override (the same list the legacy disabled_hooks file field persists).
func (s *Store) DisabledHooks() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabledLocked()
}

func (s *Store) disabledLocked() []string {
	var out []string
	for id, enabled := range s.hookEnable {
		if !enabled {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// SetHookDisabled records an EXPLICIT override for one hook's kill switch
// and persists immediately: disabled=true pins the hook off, disabled=false
// pins it on — either way overriding the hook.json `enable` default from
// then on. It is idempotent: changed=false means the same explicit override
// was already stored (and nothing was written). A persist failure rolls the
// mutation back and returns the error.
func (s *Store) SetHookDisabled(id string, disabled bool) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	enable := !disabled
	prev, had := s.hookEnable[id]
	if had && prev == enable {
		return false, nil
	}
	s.hookEnable[id] = enable
	if err := s.persistLocked(); err != nil {
		if had {
			s.hookEnable[id] = prev
		} else {
			delete(s.hookEnable, id)
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
// (same discipline as internal/kv). Callers hold s.mu. Both hook fields are
// written: hook_enable (the authoritative tri-state) and disabled_hooks
// (its explicit-disable subset, kept for binary downgrades — sorted, like
// the map keys json emits, for stable human-diffable output).
func (s *Store) persistLocked() error {
	f := fileFormat{ConcurrencyLimits: s.limits}
	if len(s.hookEnable) > 0 {
		f.HookEnable = s.hookEnable
	}
	f.DisabledHooks = s.disabledLocked()
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
