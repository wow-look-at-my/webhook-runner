// Package schema holds the published JSON Schemas. The tests here keep the
// schema honest against the Go model's contract: repo fixtures must
// validate, and the documented skip_if shapes must be accepted/rejected the
// same way hooks.Parse accepts/rejects them.
package schema

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/jsonc"
)

const hookSchemaURL = "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json"

func compileHookSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("hook.schema.json")
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource(hookSchemaURL, doc))
	sch, err := c.Compile(hookSchemaURL)
	require.NoError(t, err)
	return sch
}

// validateJSONC validates a (possibly commented) hook.json document, the
// same JSONC dialect the loader accepts.
func validateJSONC(t *testing.T, sch *jsonschema.Schema, doc []byte) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(jsonc.NewReader(doc))
	require.NoError(t, err, "document must at least be JSON(C): %s", doc)
	return sch.Validate(inst)
}

// Every shipped fixture — examples and e2e hooks — must validate against
// the published schema (CLAUDE.md: keep the Go model, the JSON schema, and
// the example/e2e fixtures in sync).
func TestRepoHookFixturesMatchSchema(t *testing.T) {
	sch := compileHookSchema(t)
	var files []string
	for _, glob := range []string{"../examples/hooks/*/hook.json", "../e2e/hooks/*/hook.json"} {
		found, err := filepath.Glob(glob)
		require.NoError(t, err)
		files = append(files, found...)
	}
	require.NotEmpty(t, files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.NoError(t, validateJSONC(t, sch, raw), "fixture %s must validate", f)
	}
}

func TestSchemaAcceptsGoodSkipIf(t *testing.T) {
	sch := compileHookSchema(t)
	good := []string{
		// The motivating one-liner: header shorthand equality.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"header:x-github-event":"workflow_run"}]}`,
		// Every operator, plus AND within a condition and OR across entries.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[
			{"action":{"in":["labeled","unlabeled"]},"sender.type":"Bot"},
			{"ref":{"prefix":"refs/tags/","ne":"refs/tags/latest"}},
			{"workflow_run.conclusion":{"exists":false}},
			{"repository.full_name":{"regex":"^wow-look-at-my/"}},
			{"action":{"eq":"closed"}}
		]}`,
		// Empty list: declared but no conditions.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[]}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
}

func TestSchemaAcceptsGoodRunTitle(t *testing.T) {
	sch := compileHookSchema(t)
	good := []string{
		// The motivating PR shape.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":"{{repository.full_name}}#{{pull_request.number}}"}`,
		// Header placeholders and static titles are titles too.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":"{{header:x-github-event}} delivery"}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":"nightly sweep"}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
}

func TestSchemaRejectsBadRunTitle(t *testing.T) {
	sch := compileHookSchema(t)
	bad := []string{
		// Wrong types — the template is a string, full stop.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":5}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":["a"]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":{"tmpl":"a"}}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":null}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","run_title":""}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}

func TestSchemaDind(t *testing.T) {
	sch := compileHookSchema(t)
	good := []string{
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","dind":true}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","dind":false}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
	bad := []string{
		// dind is a boolean, full stop.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","dind":"true"}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","dind":1}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","dind":null}`,
		// additionalProperties:false still rejects unknown keys.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","dindd":true}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}

const managerSchemaURL = "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json"

func compileManagerSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("manager.schema.json")
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource(managerSchemaURL, doc))
	sch, err := c.Compile(managerSchemaURL)
	require.NoError(t, err)
	return sch
}

// spawn_targets is an array of unique, non-empty strings. (Whether each
// entry names a DECLARED hook is the Go loader's check — the schema cannot
// see the tree.)
func TestSchemaManagerSpawnTargets(t *testing.T) {
	sch := compileManagerSchema(t)
	good := []string{
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":["gha-runner","gha-runner-dind"]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":[]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json"}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
	bad := []string{
		// An id list, full stop: no bare string, no non-strings, no empties, no duplicates.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":"gha-runner"}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":[1]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":[""]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":["a","a"]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","spawn_targets":null}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}

func TestSchemaRejectsBadSkipIf(t *testing.T) {
	sch := compileHookSchema(t)
	bad := []string{
		// Unknown operator.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"contains":"y"}}]}`,
		// Matcher must be a string or an operator object.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":5}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":null}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":["a"]}]}`,
		// Empty condition / empty matcher object.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{}}]}`,
		// Operator value types.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"eq":5}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"exists":"yes"}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"in":[]}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"in":"not-a-list"}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"in":[1]}}]}`,
		// skip_if itself must be a list of objects.
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":{"x":"y"}}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":["x"]}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}
