package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Per-entity settings: the hook's OWN configuration, kept in one `settings`
// object and separated from the keys this runner parses.
//
// The flaw this replaces: hook-specific config lived in `env`, a runner-parsed
// key, so every hook's private configuration was mixed into the runner's own
// manifest surface, spelled as strings, and validated by nobody. A hook read it
// back out of the process environment (or worse, opened hook.json itself) and
// discovered a missing or misspelled value at RUN time — usually as a silent
// default rather than a failure.
//
// Now: `settings` is an arbitrary JSON object the runner never interprets, and
// each entity ships a settings.schema.json next to its manifest describing what
// it accepts. The runner validates one against the other AT LOAD, so a hook
// whose configuration is wrong never runs at all — it is dropped with the
// schema's own error message, the same fail-closed rule as an undeclared
// concurrency group.
//
// Declaring settings without a schema is itself an error: config with no
// contract is exactly the state this is meant to end.
const SettingsSchemaFile = "settings.schema.json"

// EmptySettings is what a hook that declares none is handed — a hook always
// gets a readable, parseable settings document.
var EmptySettings = json.RawMessage("{}")

// SettingsJSON is the document handed to the container: the declared settings,
// or an empty object.
func (h *Hook) SettingsJSON() []byte {
	if len(h.Settings) == 0 {
		return []byte(EmptySettings)
	}
	return h.Settings
}

// settingsSchemaPath is the schema next to the entity's manifest.
func settingsSchemaPath(sourcePath string) string {
	return filepath.Join(filepath.Dir(sourcePath), SettingsSchemaFile)
}

// ValidateSettings enforces the settings contract for one entity:
//
//   - schema present -> the settings document (or {} when absent) MUST validate
//     against it. A schema with required properties therefore fails a hook that
//     declares nothing, which is the point: "unconfigured" is a load error, not
//     a hook that starts and no-ops.
//   - settings declared with no schema -> error.
//   - neither -> fine, the hook has no configuration.
//
// A schema that is missing, unreadable, or not a valid JSON Schema is an error
// too: an unreadable contract must never degrade into "no contract".
func (h *Hook) ValidateSettings() error {
	if len(h.Settings) > 0 {
		var probe any
		if err := json.Unmarshal(h.Settings, &probe); err != nil {
			return fmt.Errorf("settings is not valid JSON: %w", err)
		}
		if _, ok := probe.(map[string]any); !ok {
			return errors.New("settings must be a JSON object")
		}
		// ${settings:...} references resolve HERE, before the schema runs, so
		// the schema validates real values rather than reference text -- and a
		// typo'd path is a load error instead of a surprise mid-run. The
		// expanded document is what the container is handed. ${env:...} is
		// left for the run path (see settingsref.go).
		expanded, err := ExpandSettingsSelfRefs(h.Settings)
		if err != nil {
			return fmt.Errorf("settings references: %w", err)
		}
		h.Settings = expanded
	}

	path := settingsSchemaPath(h.SourcePath)
	raw, err := readSettingsSchema(path)
	if err != nil {
		return err
	}
	if raw == nil {
		if len(h.Settings) > 0 {
			return fmt.Errorf("settings is declared but %s is missing: hook configuration must ship the schema that describes it", SettingsSchemaFile)
		}
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

// readSettingsSchema reads the entity's schema file. (nil, nil) means the
// entity ships none — distinct from an unreadable one, which is an error:
// a contract that cannot be read must never degrade into "no contract".
func readSettingsSchema(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", SettingsSchemaFile, err)
	}
	return raw, nil
}

// SettingsSchemaJSON returns the entity's raw settings.schema.json (comments
// stripped, so it parses as JSON), or nil when it ships none. The admin API
// serves this to the dashboard, which builds its form from it — the schema is
// the single source of truth for types, ranges, enums and descriptions, and
// nothing about the form is duplicated server-side.
func (h *Hook) SettingsSchemaJSON() ([]byte, error) {
	raw, err := readSettingsSchema(settingsSchemaPath(h.SourcePath))
	if err != nil || raw == nil {
		return nil, err
	}
	stripped, err := io.ReadAll(stripComments(raw))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", SettingsSchemaFile, err)
	}
	if !json.Valid(stripped) {
		return nil, fmt.Errorf("%s is not valid JSON", SettingsSchemaFile)
	}
	return stripped, nil
}

func compileSettingsSchema(path string, raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(stripComments(raw))
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", SettingsSchemaFile, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(path, doc); err != nil {
		return nil, fmt.Errorf("%s is not a valid JSON Schema: %w", SettingsSchemaFile, err)
	}
	schema, err := c.Compile(path)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid JSON Schema: %w", SettingsSchemaFile, err)
	}
	return schema, nil
}
