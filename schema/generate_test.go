package schema

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The whole point of the base: a constraint added there reaches both
// documents, which is what hand-copying failed to do.
func TestSharedConstraintsAreIdenticalInBothSchemas(t *testing.T) {
	hook := propertiesOf(t, Hook)
	manager := propertiesOf(t, Manager)

	shared := 0
	for name, hp := range hook {
		mp, ok := manager[name]
		if !ok {
			continue
		}
		shared++
		assert.Equal(t, constraintsOf(t, hp), constraintsOf(t, mp),
			"property %q differs between the two schemas; shared constraints belong in src/common.json", name)
	}
	assert.NotZero(t, shared, "the schemas share no properties -- the base is not wired up")
}

// The five that had drifted, pinned so they cannot drift back.
func TestPreviouslyDriftedConstraintsSurvive(t *testing.T) {
	for _, doc := range [][]byte{Hook, Manager} {
		props := propertiesOf(t, doc)
		assert.Equal(t, "^[0-9]+(ns|us|ms|s|m|h)+$", props["timeout"]["pattern"],
			"a timeout must be a Go duration in BOTH schemas")
		assert.Equal(t, "X-API-Key", props["api_key_header"]["default"])
		assert.Equal(t, true, props["enable"]["default"])
		assert.Contains(t, props["run_title"], "examples")
		assert.NotContains(t, props["skip_if"], "minItems")
	}
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

func propertiesOf(t *testing.T, doc []byte) map[string]map[string]any {
	t.Helper()
	var parsed struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(doc, &parsed))
	return parsed.Properties
}

func constraintsOf(t *testing.T, prop map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, v := range prop {
		if k != "description" {
			out[k] = v
		}
	}
	return out
}
