package hooks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An operator override is config typed into a dashboard, so every one of these cases is about the same question: can a value the operator set.

const overrideSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["ai"],
  "properties": {
    "ai": {
      "type": "object",
      "additionalProperties": false,
      "required": ["model"],
      "properties": {
        "model": {"type": "string", "minLength": 1},
        "reasoning": {"enum": ["auto", "off"]},
        "retries": {"type": "integer", "minimum": 0, "maximum": 10}
      }
    },
    "features": {"type": "array", "items": {"type": "string"}}
  }
}`

const overrideSettings = `{"ai":{"model":"m","reasoning":"auto","retries":3},"features":["a","b"]}`

func overrideHook(t *testing.T) *Hook {
	t.Helper()
	return &Hook{ID: "h", SourcePath: writeSettingsFixture(t, overrideSchema), Settings: json.RawMessage(overrideSettings)}
}

func TestOverridePinsOneFieldAndLeavesTheRest(t *testing.T) {
	h := overrideHook(t)
	require.NoError(t, h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/ai/reasoning": json.RawMessage(`"off"`),
	}))
	var got map[string]any
	require.NoError(t, json.Unmarshal(h.SettingsJSON(), &got))
	ai := got["ai"].(map[string]any)
	assert.Equal(t, "off", ai["reasoning"], "the pinned field changed")
	assert.Equal(t, "m", ai["model"], "a sibling field still comes from the manifest")
	assert.Equal(t, float64(3), ai["retries"])
	assert.Equal(t, []any{"a", "b"}, got["features"], "an untouched subtree is unchanged")
}

func TestOverrideKeepsTheReferencedValuesType(t *testing.T) {
	h := overrideHook(t)
	require.NoError(t, h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/ai/retries": json.RawMessage(`7`),
	}))
	var got map[string]any
	require.NoError(t, json.Unmarshal(h.SettingsJSON(), &got))
	assert.Equal(t, float64(7), got["ai"].(map[string]any)["retries"], "a number stays a number, never the string \"7\"")
}

func TestOverrideIntoAnArrayElement(t *testing.T) {
	h := overrideHook(t)
	require.NoError(t, h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/features/1": json.RawMessage(`"z"`),
	}))
	var got map[string]any
	require.NoError(t, json.Unmarshal(h.SettingsJSON(), &got))
	assert.Equal(t, []any{"a", "z"}, got["features"])
}

// The schema is the contract, and an override is not exempt from it.
func TestOverrideViolatingTheSchemaIsRefusedAndChangesNothing(t *testing.T) {
	h := overrideHook(t)
	err := h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/ai/reasoning": json.RawMessage(`"sometimes"`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), SettingsSchemaFile)
	assert.JSONEq(t, overrideSettings, string(h.SettingsJSON()),
		"a refused override leaves the manifest document EXACTLY as it was — never half-merged")
}

func TestOverrideOutOfRangeIsRefused(t *testing.T) {
	h := overrideHook(t)
	require.Error(t, h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/ai/retries": json.RawMessage(`99`),
	}))
	assert.JSONEq(t, overrideSettings, string(h.SettingsJSON()))
}

// An override pins a DECLARED field. Inventing one would create config with
// no manifest counterpart, so no revert target and no reviewed default.
func TestOverrideCannotInventAFieldTheManifestOmits(t *testing.T) {
	h := overrideHook(t)
	err := h.ApplySettingsOverrides(map[string]json.RawMessage{
		// Permitted by the schema, absent from the manifest.
		"/features/9": json.RawMessage(`"x"`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid index")
}

func TestOverrideOnAMissingObjectFieldNamesTheField(t *testing.T) {
	h := &Hook{ID: "h", SourcePath: writeSettingsFixture(t, overrideSchema), Settings: json.RawMessage(`{"ai":{"model":"m"}}`)}
	err := h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/ai/reasoning": json.RawMessage(`"off"`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to override")
	assert.Contains(t, err.Error(), "/ai/reasoning")
}

func TestOverrideThroughAMissingParentIsRefused(t *testing.T) {
	h := overrideHook(t)
	err := h.ApplySettingsOverrides(map[string]json.RawMessage{"/nope/deep": json.RawMessage(`1`)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to override")
}

// ${settings:...} resolves at load and ${env:...} at container start, so an
// override carrying either would not be the value the operator typed.
func TestOverrideRefusesReferenceSyntax(t *testing.T) {
	for _, ref := range []string{`"${env:AI_KEY}"`, `"${settings:ai.model}"`} {
		h := overrideHook(t)
		err := h.ApplySettingsOverrides(map[string]json.RawMessage{"/ai/model": json.RawMessage(ref)})
		require.Error(t, err, ref)
		assert.Contains(t, err.Error(), "literal value")
		assert.JSONEq(t, overrideSettings, string(h.SettingsJSON()))
	}
}

func TestOverrideEmptyMapIsANoOp(t *testing.T) {
	h := overrideHook(t)
	require.NoError(t, h.ApplySettingsOverrides(nil))
	assert.JSONEq(t, overrideSettings, string(h.SettingsJSON()))
}

// A multi-field override reports the SAME field first every time. Map
// iteration order is randomized in Go, so without the sort one broken
// override produces a different log line run to run — miserable to debug,
// and it makes the message useless as a thing to grep for.
func TestOverrideErrorNamesTheFirstFieldInPointerOrder(t *testing.T) {
	h := overrideHook(t)
	// Both pointers miss the manifest, so which one is REPORTED is decided
	// by the apply loop's ordering rather than by the schema library.
	bad := map[string]json.RawMessage{
		"/ai/zulu":  json.RawMessage(`"z"`),
		"/ai/alpha": json.RawMessage(`"a"`),
	}
	for range 20 {
		err := h.ApplySettingsOverrides(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/ai/alpha", "sorted order puts alpha first, every time")
		assert.NotContains(t, err.Error(), "/ai/zulu")
	}
}

// Applying several fields at once is all-or-nothing: the good ones must not
// land when a later one is refused.
func TestOverrideBatchIsAtomic(t *testing.T) {
	h := overrideHook(t)
	err := h.ApplySettingsOverrides(map[string]json.RawMessage{
		"/ai/model":     json.RawMessage(`"good"`),
		"/ai/reasoning": json.RawMessage(`"bogus"`),
	})
	require.Error(t, err)
	assert.JSONEq(t, overrideSettings, string(h.SettingsJSON()), "the valid field must not have landed either")
}

func TestManifestPointerValueReadsAndReportsMisses(t *testing.T) {
	h := overrideHook(t)
	v, ok := h.ManifestPointerValue("/ai/model")
	require.True(t, ok)
	assert.JSONEq(t, `"m"`, string(v))

	v, ok = h.ManifestPointerValue("/features/0")
	require.True(t, ok)
	assert.JSONEq(t, `"a"`, string(v))

	_, ok = h.ManifestPointerValue("/ai/gone")
	assert.False(t, ok, "a pointer with no manifest counterpart reports a miss, never a null")

	_, ok = h.ManifestPointerValue("bad")
	assert.False(t, ok)
}

// RFC 6901's escapes, in the order the spec requires: ~1 -> "/" before
// ~0 -> "~", so "~01" is "~1" and not "~" + "1".
func TestPointerEscapesFollowRFC6901(t *testing.T) {
	tokens, err := parsePointer("/a~1b/c~0d/e~01f")
	require.NoError(t, err)
	assert.Equal(t, []string{"a/b", "c~d", "e~1f"}, tokens)
}

func TestPointerMustStartWithSlash(t *testing.T) {
	_, err := parsePointer("ai/model")
	require.Error(t, err)
	_, err = parsePointer("")
	require.Error(t, err)
}

// An entity that ships no schema has nothing an override could legally pin,
// and must not panic on the way to saying so.
func TestOverrideOnASchemalessEntity(t *testing.T) {
	h := &Hook{ID: "h", SourcePath: writeSettingsFixture(t, "")}
	require.Error(t, h.ApplySettingsOverrides(map[string]json.RawMessage{"/x": json.RawMessage(`1`)}))
}

func TestSettingsSchemaJSONIsServedStripped(t *testing.T) {
	h := &Hook{ID: "h", SourcePath: writeSettingsFixture(t, "// leading comment\n"+overrideSchema)}
	raw, err := h.SettingsSchemaJSON()
	require.NoError(t, err)
	require.NotNil(t, raw)
	assert.True(t, json.Valid(raw), "comments are stripped so the dashboard can parse it as JSON")

	none := &Hook{ID: "h", SourcePath: writeSettingsFixture(t, "")}
	raw, err = none.SettingsSchemaJSON()
	require.NoError(t, err)
	assert.Nil(t, raw, "no schema is nil, not an error")
}
