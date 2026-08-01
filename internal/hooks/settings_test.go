package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole point of the settings contract is that WRONG CONFIG DOES NOT LOAD.
// Every case below is a hook that must be refused, or a hook whose config is
// proven good before a container ever starts.
func writeSettingsFixture(t *testing.T, schema string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644))
	if schema != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, SettingsSchemaFile), []byte(schema), 0o644))
	}
	return filepath.Join(dir, "hook.json")
}

const settingsSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["app_id"],
  "properties": {
    "app_id": {"type": "string", "minLength": 1},
    "pacing_ms": {"type": "integer", "minimum": 0},
    "features": {"type": "array", "items": {"type": "string"}}
  }
}`

func TestSettingsValidatesAgainstTheHooksOwnSchema(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"app_id":"42","pacing_ms":1000,"features":["a"]}`)}
	require.NoError(t, h.ValidateSettings())
}

func TestSettingsMissingRequiredKeyIsALoadError(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"pacing_ms":1000}`)}
	err := h.ValidateSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app_id")
}

// A schema with required properties refuses a hook that declares NO settings:
// "unconfigured" is a load error, never a hook that starts and no-ops.
func TestSettingsAbsentStillCheckedAgainstSchema(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	h := &Hook{ID: "h", SourcePath: src}
	require.Error(t, h.ValidateSettings())
}

func TestSettingsWrongTypeIsALoadError(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	// A string where the schema says integer: the classic "everything was a
	// string in env" bug, now caught before the hook loads.
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"app_id":"42","pacing_ms":"1000"}`)}
	require.Error(t, h.ValidateSettings())
}

func TestSettingsUnknownKeyIsALoadErrorWhenTheSchemaSaysSo(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"app_id":"42","typoed_key":true}`)}
	err := h.ValidateSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "typoed_key")
}

// Config with no contract is the state this feature exists to end.
func TestSettingsWithoutASchemaIsRefused(t *testing.T) {
	src := writeSettingsFixture(t, "")
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"anything":1}`)}
	err := h.ValidateSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), SettingsSchemaFile)
}

// A hook with neither is fine -- plenty of hooks need no configuration.
func TestNoSettingsAndNoSchemaIsFine(t *testing.T) {
	h := &Hook{ID: "h", SourcePath: writeSettingsFixture(t, "")}
	require.NoError(t, h.ValidateSettings())
}

// An unreadable contract must never degrade into "no contract".
func TestBrokenSettingsSchemaIsALoadError(t *testing.T) {
	src := writeSettingsFixture(t, `{"type": "object", "properties": {"a": {"type": 12}}}`)
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"a":"x"}`)}
	require.Error(t, h.ValidateSettings())
}

func TestSettingsMustBeAnObject(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`["a"]`)}
	err := h.ValidateSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "object")
}

// The document handed to the container: what was declared, or an empty object
// -- never nothing, so a hook reading its config has no missing-file branch.
func TestSettingsJSONDefaultsToEmptyObject(t *testing.T) {
	assert.JSONEq(t, `{}`, string((&Hook{ID: "h"}).SettingsJSON()))
	assert.JSONEq(t, `{"a":1}`, string((&Hook{ID: "h", Settings: json.RawMessage(`{"a":1}`)}).SettingsJSON()))
}

// The schema file may carry JSONC comments, like every other manifest here.
func TestSettingsSchemaMayCarryComments(t *testing.T) {
	src := writeSettingsFixture(t, "// what this hook accepts\n"+settingsSchema)
	h := &Hook{ID: "h", SourcePath: src, Settings: json.RawMessage(`{"app_id":"42"}`)}
	require.NoError(t, h.ValidateSettings())
}

// Parse is where it bites: a hook whose settings do not match its schema is
// DROPPED, exactly like an undeclared concurrency group.
func TestParseRejectsSettingsThatDoNotValidate(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	_, err := Parse("h", src, []byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","description":"d","settings":{"pacing_ms":5}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app_id")
}

func TestParseAcceptsValidSettings(t *testing.T) {
	src := writeSettingsFixture(t, settingsSchema)
	h, err := Parse("h", src, []byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","description":"d","settings":{"app_id":"42"}}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"app_id":"42"}`, string(h.SettingsJSON()))
}

// `env` is gone: a manifest still carrying one must fail loudly rather than
// have its configuration silently ignored.
func TestParseRejectsTheRetiredEnvBlock(t *testing.T) {
	src := writeSettingsFixture(t, "")
	_, err := Parse("h", src, []byte(`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","description":"d","env":{"A":"b"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env")
}
