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

const hookSchemaURL = "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json"

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
		`{"$schema":"s","skip_if":[{"header:x-github-event":"workflow_run"}]}`,
		// Every operator, plus AND within a condition and OR across entries.
		`{"$schema":"s","skip_if":[
			{"action":{"in":["labeled","unlabeled"]},"sender.type":"Bot"},
			{"ref":{"prefix":"refs/tags/","ne":"refs/tags/latest"}},
			{"workflow_run.conclusion":{"exists":false}},
			{"repository.full_name":{"regex":"^wow-look-at-my/"}},
			{"action":{"eq":"closed"}}
		]}`,
		// Empty list: declared but no conditions.
		`{"$schema":"s","skip_if":[]}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
}

func TestSchemaAcceptsGoodRunTitle(t *testing.T) {
	sch := compileHookSchema(t)
	good := []string{
		// The motivating PR shape.
		`{"$schema":"s","run_title":"{{repository.full_name}}#{{pull_request.number}}"}`,
		// Header placeholders and static titles are titles too.
		`{"$schema":"s","run_title":"{{header:x-github-event}} delivery"}`,
		`{"$schema":"s","run_title":"nightly sweep"}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
}

func TestSchemaRejectsBadRunTitle(t *testing.T) {
	sch := compileHookSchema(t)
	bad := []string{
		// Wrong types — the template is a string, full stop. (Malformed
		// placeholder SYNTAX inside the string is the Go loader's check:
		// JSON Schema can't parse templates.)
		`{"$schema":"s","run_title":5}`,
		`{"$schema":"s","run_title":["a"]}`,
		`{"$schema":"s","run_title":{"tmpl":"a"}}`,
		`{"$schema":"s","run_title":null}`,
		`{"$schema":"s","run_title":""}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}

func TestSchemaDind(t *testing.T) {
	sch := compileHookSchema(t)
	good := []string{
		`{"$schema":"s","dind":true}`,
		`{"$schema":"s","dind":false}`,
	}
	for _, doc := range good {
		assert.NoError(t, validateJSONC(t, sch, []byte(doc)), "should validate: %s", doc)
	}
	bad := []string{
		// dind is a boolean, full stop.
		`{"$schema":"s","dind":"true"}`,
		`{"$schema":"s","dind":1}`,
		`{"$schema":"s","dind":null}`,
		// additionalProperties:false still rejects unknown keys.
		`{"$schema":"s","dindd":true}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}

func TestSchemaRejectsBadSkipIf(t *testing.T) {
	sch := compileHookSchema(t)
	bad := []string{
		// Unknown operator.
		`{"$schema":"s","skip_if":[{"x":{"contains":"y"}}]}`,
		// Matcher must be a string or an operator object.
		`{"$schema":"s","skip_if":[{"x":5}]}`,
		`{"$schema":"s","skip_if":[{"x":null}]}`,
		`{"$schema":"s","skip_if":[{"x":["a"]}]}`,
		// Empty condition / empty matcher object.
		`{"$schema":"s","skip_if":[{}]}`,
		`{"$schema":"s","skip_if":[{"x":{}}]}`,
		// Operator value types.
		`{"$schema":"s","skip_if":[{"x":{"eq":5}}]}`,
		`{"$schema":"s","skip_if":[{"x":{"exists":"yes"}}]}`,
		`{"$schema":"s","skip_if":[{"x":{"in":[]}}]}`,
		`{"$schema":"s","skip_if":[{"x":{"in":"not-a-list"}}]}`,
		`{"$schema":"s","skip_if":[{"x":{"in":[1]}}]}`,
		// skip_if itself must be a list of objects.
		`{"$schema":"s","skip_if":{"x":"y"}}`,
		`{"$schema":"s","skip_if":["x"]}`,
	}
	for _, doc := range bad {
		assert.Error(t, validateJSONC(t, sch, []byte(doc)), "should be rejected: %s", doc)
	}
}
