package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Operator settings overrides: the dashboard's settings editor writes one
// field at a time (internal/overrides holds them, keyed by RFC 6901 JSON
// Pointer), and this is where they meet the manifest.
//
// The rules that make an override safe to apply to a running fleet:
//
//   - SPARSE. An override pins one field. Everything else keeps coming from
//     hook.json, so a manifest edit to an untouched field still lands.
//   - The pointer must ALREADY RESOLVE in the manifest document. An override
//     can only change a field the entity declares — never invent one. A
//     schema may permit properties the manifest omits, but a value with no
//     manifest counterpart has no revert target and no reviewed default, and
//     that is exactly the config that rots.
//   - The merged document is re-validated against settings.schema.json.
//     An override that violates the schema is refused; the entity keeps its
//     manifest settings and says so loudly.
//   - No reference syntax. `${settings:...}` resolves at load and `${env:...}`
//     at container start; letting an override introduce either would mean a
//     value the operator typed is not the value they get. Overrides are
//     literals.

// SettingsRefPrefix is the marker both reference forms share. An override
// value containing it is refused (see ApplySettingsOverrides).
const SettingsRefPrefix = "${"

// ApplySettingsOverrides overlays ptrs onto the entity's settings document
// and re-validates the result against its settings.schema.json. On success
// h.Settings is the merged document — what the container will be handed.
//
// On ANY error h.Settings is left EXACTLY as it was: a rejected override
// never half-applies, so the caller's degrade ("keep serving the manifest
// values") is a real fallback rather than a partially-mutated document.
func (h *Hook) ApplySettingsOverrides(ptrs map[string]json.RawMessage) error {
	if len(ptrs) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(h.SettingsJSON(), &doc); err != nil {
		return fmt.Errorf("settings is not valid JSON: %w", err)
	}
	// Sorted so a multi-field failure always names the same field first:
	// map order would make the same broken override report differently run
	// to run, which is miserable to debug from a log line.
	for _, ptr := range sortedPointers(ptrs) {
		raw := ptrs[ptr]
		if err := refuseReferenceSyntax(ptr, raw); err != nil {
			return err
		}
		var val any
		if err := json.Unmarshal(raw, &val); err != nil {
			return fmt.Errorf("override %s: value is not valid JSON: %w", ptr, err)
		}
		if err := setAtPointer(doc, ptr, val); err != nil {
			return err
		}
	}
	merged, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("merge settings overrides: %w", err)
	}
	prev := h.Settings
	h.Settings = merged
	if err := h.validateSettingsAgainstSchema(); err != nil {
		h.Settings = prev
		return err
	}
	return nil
}

// validateSettingsAgainstSchema re-runs ONLY the schema half of the settings
// contract. ValidateSettings also expands ${settings:...} references, which
// must not run twice: by the time overrides are applied the manifest's
// references are already resolved to values, and re-expanding a document
// that legitimately contains a literal "${" would corrupt it.
func (h *Hook) validateSettingsAgainstSchema() error {
	path := settingsSchemaPath(h.SourcePath)
	raw, err := readSettingsSchema(path)
	if err != nil {
		return err
	}
	if raw == nil {
		// No schema. ValidateSettings already refused settings-without-schema
		// at load, so reaching here means the entity declares no settings and
		// there is nothing an override could legally pin.
		return nil
	}
	schema, err := compileSettingsSchema(path, raw)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(h.SettingsJSON(), &doc); err != nil {
		return fmt.Errorf("settings is not valid JSON: %w", err)
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("settings does not match %s: %w", SettingsSchemaFile, err)
	}
	return nil
}

// refuseReferenceSyntax rejects an override value that carries `${...}`.
func refuseReferenceSyntax(ptr string, raw json.RawMessage) error {
	if !strings.Contains(string(raw), SettingsRefPrefix) {
		return nil
	}
	return fmt.Errorf("override %s: values may not contain %s references — "+
		"an override is a literal value, and a reference would resolve to something other than what was typed "+
		"(put references in the manifest, where their resolution is reviewed)", ptr, SettingsRefPrefix)
}

func sortedPointers(ptrs map[string]json.RawMessage) []string {
	out := make([]string, 0, len(ptrs))
	for ptr := range ptrs {
		out = append(out, ptr)
	}
	sort.Strings(out)
	return out
}

// SettingsPointerValue reads the value at an RFC 6901 pointer in the
// entity's settings document. ok=false when the pointer does not resolve —
// which is how the API reports "this override has no manifest counterpart
// any more" instead of inventing a null.
func (h *Hook) SettingsPointerValue(pointer string) (json.RawMessage, bool) {
	var doc any
	if err := json.Unmarshal(h.SettingsJSON(), &doc); err != nil {
		return nil, false
	}
	val, err := getAtPointer(doc, pointer)
	if err != nil {
		return nil, false
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// parsePointer splits an RFC 6901 pointer into its decoded tokens. The
// escape order is load-bearing and specified: ~1 -> "/" BEFORE ~0 -> "~",
// otherwise "~01" decodes to "~" + "1" instead of "~1".
func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, errors.New("pointer must name a field")
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("%q is not a JSON Pointer (must start with %q)", pointer, "/")
	}
	parts := strings.Split(pointer[1:], "/")
	out := make([]string, len(parts))
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		out[i] = strings.ReplaceAll(p, "~0", "~")
	}
	return out, nil
}

// getAtPointer walks doc to the pointer's target.
func getAtPointer(doc any, pointer string) (any, error) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	cur := doc
	for i, tok := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, fmt.Errorf("%s: no such field %q", pointer, strings.Join(tokens[:i+1], "/"))
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("%s: %q is not a valid index into an array of %d", pointer, tok, len(node))
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("%s: %q is not inside an object or array", pointer, tok)
		}
	}
	return cur, nil
}

// setAtPointer writes val at the pointer's target, which MUST already
// exist. Creating missing intermediate objects is deliberately not
// supported: an override that can invent structure can invent a whole
// config the manifest never declared, and there would be nothing to revert
// to. The error says which token was missing so the UI can be specific.
func setAtPointer(doc any, pointer string, val any) error {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return fmt.Errorf("override %s: %w", pointer, err)
	}
	parent := doc
	for i, tok := range tokens[:len(tokens)-1] {
		switch node := parent.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return fmt.Errorf("override %s: the manifest has no field %q, so there is nothing to override "+
					"(add it to hook.json first — an override pins a declared field, it does not create one)",
					pointer, "/"+strings.Join(tokens[:i+1], "/"))
			}
			parent = v
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return fmt.Errorf("override %s: %q is not a valid index into an array of %d", pointer, tok, len(node))
			}
			parent = node[idx]
		default:
			return fmt.Errorf("override %s: %q is not inside an object or array", pointer, tok)
		}
	}
	last := tokens[len(tokens)-1]
	switch node := parent.(type) {
	case map[string]any:
		if _, ok := node[last]; !ok {
			return fmt.Errorf("override %s: the manifest has no field %q, so there is nothing to override "+
				"(add it to hook.json first — an override pins a declared field, it does not create one)", pointer, pointer)
		}
		node[last] = val
	case []any:
		idx, err := strconv.Atoi(last)
		if err != nil || idx < 0 || idx >= len(node) {
			return fmt.Errorf("override %s: %q is not a valid index into an array of %d", pointer, last, len(node))
		}
		node[idx] = val
	default:
		return fmt.Errorf("override %s: %q is not inside an object or array", pointer, last)
	}
	return nil
}
