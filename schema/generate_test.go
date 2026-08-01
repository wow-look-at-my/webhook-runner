package schema

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/json-validator/validator"
)

var update = flag.Bool("update", false, "rewrite the generated schemas from src/")

var generated = []struct{ overlay, out string }{
	{"src/hook.json", "hook.schema.json"},
	{"src/manager.json", "manager.schema.json"},
}

// The committed schemas are what go:embed compiles in and what CI publishes,
// so a drifted checkout would validate manifests against something nobody
// reviewed. Regenerate with `go test ./schema -update`.
func TestGeneratedSchemasMatchSources(t *testing.T) {
	base, err := os.ReadFile("src/common.json")
	require.NoError(t, err)

	for _, g := range generated {
		t.Run(g.out, func(t *testing.T) {
			doc, err := os.ReadFile(g.overlay)
			require.NoError(t, err)
			want, err := Generate(base, doc)
			require.NoError(t, err)

			if *update {
				require.NoError(t, os.WriteFile(filepath.Clean(g.out), want, 0o644))
				return
			}
			got, err := os.ReadFile(g.out)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got),
				"%s is stale -- regenerate with: go test ./schema -update", g.out)
		})
	}
}

// The whole point of the base: ONE shared block, byte-identical in both
// published documents, so a constraint cannot reach one and miss the other.
func TestSharedBlockIsIdenticalInBothSchemas(t *testing.T) {
	hook, manager := sharedBlock(t, Hook), sharedBlock(t, Manager)
	assert.Equal(t, string(hook), string(manager),
		"$defs.common differs between the schemas; it is generated from src/common.json and must not")
	assert.NotEmpty(t, hook, "$defs.common is missing -- the base is not wired up")
}

// The five that had drifted, pinned so they cannot drift back.
func TestPreviouslyDriftedConstraintsSurvive(t *testing.T) {
	for _, doc := range [][]byte{Hook, Manager} {
		props := sharedProperties(t, doc)
		assert.Equal(t, "^[0-9]+(ns|us|ms|s|m|h)+$", props["timeout"]["pattern"],
			"a timeout must be a Go duration in BOTH schemas")
		assert.Equal(t, "X-API-Key", props["api_key_header"]["default"])
		assert.Equal(t, true, props["enable"]["default"])
		assert.Contains(t, props["run_title"], "examples")
		assert.NotContains(t, props["skip_if"], "minItems")
	}
}

// The composition trap, pinned: additionalProperties would evaluate the shared
// block alone and reject the entity's own properties. Only unevaluatedProperties
// accounts for what a sibling subschema matched.
func TestCompositionAcceptsEntityPropertiesAndStillRejectsUnknowns(t *testing.T) {
	cases := []struct {
		name, doc string
		valid     bool
	}{
		{"hook-only property", `{"$schema":"https://example.test/hook.schema.json","api_key":"k","schedule":"1h"}`, true},
		{"unknown property", `{"$schema":"https://example.test/hook.schema.json","api_key":"k","nope":1}`, false},
		{"shared constraint still enforced", `{"$schema":"https://example.test/hook.schema.json","api_key":"k","timeout":"banana"}`, false},
	}
	v, err := validator.NewFromBytes("embedded:hook.schema.json", Hook, validator.Options{Draft: "2020"})
	require.NoError(t, err)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := v.ValidateBytes([]byte(c.doc), "hook.json")
			require.NoError(t, res.Err)
			assert.Equal(t, c.valid, res.Valid, res.Detail())
		})
	}
}

// Every published document must carry unevaluatedProperties and NOT
// additionalProperties -- the latter is the shape that silently breaks.
func TestPublishedSchemasUseUnevaluatedProperties(t *testing.T) {
	for _, doc := range [][]byte{Hook, Manager} {
		var top map[string]any
		require.NoError(t, json.Unmarshal(doc, &top))
		assert.Equal(t, false, top["unevaluatedProperties"])
		assert.NotContains(t, top, "additionalProperties",
			"additionalProperties does not compose through allOf")
	}
}

func sharedBlock(t *testing.T, doc []byte) []byte {
	t.Helper()
	var parsed struct {
		Defs struct {
			Common json.RawMessage `json:"common"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(doc, &parsed))
	return parsed.Defs.Common
}

func sharedProperties(t *testing.T, doc []byte) map[string]map[string]any {
	t.Helper()
	var parsed struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(sharedBlock(t, doc), &parsed))
	return parsed.Properties
}

// A shared property an overlay does not document is an error, not an
// undocumented field inherited by accident.
func TestOverlayMustDescribeEverySharedProperty(t *testing.T) {
	_, err := Generate([]byte(`{"timeout":{"type":"string"}}`),
		[]byte(`{"type":"object","descriptions":{},"properties":{}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no description")
}

// An overlay redefining a shared property is the twin returning.
func TestOverlayMayNotRedefineASharedProperty(t *testing.T) {
	_, err := Generate([]byte(`{"timeout":{"type":"string"}}`),
		[]byte(`{"descriptions":{"timeout":"t"},"properties":{"timeout":{"type":"integer"}}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared in both")
}
