package overrides

// Per-entity SETTINGS overrides: the values the operator pins from the
// dashboard's settings editor, keyed by entity id and RFC 6901 JSON Pointer.
//
// Split out of overrides.go because it is a distinct surface (a field pin,
// not a switch) with its own rules, and because keeping it there pushed that
// file past the file-length gate.
//
// This package deliberately knows NOTHING about schemas or entities: it
// stores what it is told and persists it atomically. Whether a value is
// legal for a given hook is a question only the loader and the entity's
// settings.schema.json can answer, and both live elsewhere — see
// hooks.ApplySettingsOverrides (the merge + re-validation) and
// server.handleSettingsOverrideSet (the write-time gate).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SettingsOverrides returns a copy of one entity's settings overrides
// (JSON Pointer -> value), or nil when the operator has overridden nothing.
func (s *Store) SettingsOverrides(id string) map[string]json.RawMessage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ptrs, ok := s.settings[id]
	if !ok || len(ptrs) == 0 {
		return nil
	}
	return copyPointerMap(ptrs)
}

// AllSettingsOverrides returns a copy of every entity's settings overrides.
func (s *Store) AllSettingsOverrides() map[string]map[string]json.RawMessage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]json.RawMessage, len(s.settings))
	for id, ptrs := range s.settings {
		if len(ptrs) == 0 {
			continue
		}
		out[id] = copyPointerMap(ptrs)
	}
	return out
}

// SetSettingOverride pins one settings field of one entity and persists
// immediately. pointer is an RFC 6901 JSON Pointer into the entity's
// settings document ("/ai/model"); value is the raw JSON to store there.
// Idempotent (changed=false when the identical value was already stored);
// a persist failure rolls the mutation back and returns the error.
//
// This does NOT validate against the entity's settings.schema.json — the
// store holds no schemas and knows no entities. The caller validates the
// MERGED document before calling (server.handleSettingsOverrideSet), which
// is the only place the schema and the manifest are both in hand.
func (s *Store) SetSettingOverride(id, pointer string, value json.RawMessage) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	if id == "" {
		return false, errors.New("overrides: entity id required")
	}
	if err := validPointer(pointer); err != nil {
		return false, err
	}
	if !json.Valid(value) {
		return false, fmt.Errorf("overrides: value for %s%s is not valid JSON", id, pointer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, hadEntity := s.settings[id]
	if hadEntity {
		if old, had := prev[pointer]; had && bytes.Equal(old, value) {
			return false, nil
		}
	}
	// Snapshot for rollback BEFORE mutating: a persist failure must leave
	// memory exactly as it was, the same rule the other setters follow.
	var restore map[string]json.RawMessage
	if hadEntity {
		restore = copyPointerMap(prev)
	} else {
		s.settings[id] = map[string]json.RawMessage{}
	}
	s.settings[id][pointer] = append(json.RawMessage(nil), value...)
	if err := s.persistLocked(); err != nil {
		if hadEntity {
			s.settings[id] = restore
		} else {
			delete(s.settings, id)
		}
		return false, err
	}
	return true, nil
}

// ClearSettingOverride drops one pinned field, reverting it to whatever the
// manifest says. Idempotent; a persist failure rolls the removal back.
func (s *Store) ClearSettingOverride(id, pointer string) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ptrs, ok := s.settings[id]
	if !ok {
		return false, nil
	}
	prev, had := ptrs[pointer]
	if !had {
		return false, nil
	}
	restore := copyPointerMap(ptrs)
	delete(ptrs, pointer)
	if len(ptrs) == 0 {
		delete(s.settings, id)
	}
	if err := s.persistLocked(); err != nil {
		s.settings[id] = restore
		s.settings[id][pointer] = prev
		return false, err
	}
	return true, nil
}

// ClearSettingOverrides drops every pinned field for one entity (the
// "revert all to the manifest" button). Idempotent; rolls back on failure.
func (s *Store) ClearSettingOverrides(id string) (changed bool, err error) {
	if s == nil {
		return false, errors.New("overrides: store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.settings[id]
	if !ok || len(prev) == 0 {
		return false, nil
	}
	restore := copyPointerMap(prev)
	delete(s.settings, id)
	if err := s.persistLocked(); err != nil {
		s.settings[id] = restore
		return false, err
	}
	return true, nil
}

// validPointer enforces RFC 6901's shape: empty (the whole document) is
// REFUSED here on purpose — an override is a field pin, and replacing the
// entire settings document would defeat the sparseness that lets manifest
// edits still land. Otherwise a pointer must start with "/".
func validPointer(pointer string) error {
	if pointer == "" {
		return errors.New("overrides: pointer must name a field (the empty pointer would pin the whole document)")
	}
	if !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("overrides: %q is not a JSON Pointer (must start with %q)", pointer, "/")
	}
	return nil
}
