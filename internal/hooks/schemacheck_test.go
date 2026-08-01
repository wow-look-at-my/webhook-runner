package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The published schema is now enforced at LOAD, by the same implementation the
// hooks repo's CI runs. These cases are the ones the Go model CANNOT express --
// exactly the drift that used to be caught in CI and nowhere else.
func parseHookDoc(t *testing.T, doc string) error {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, DockerfileName), []byte("FROM alpine\n"), 0o644))
	_, err := Parse("h", filepath.Join(dir, "hook.json"), []byte(doc))
	return err
}

const schemaURL = `"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json"`

func TestSchemaGateRejectsWhatTheGoModelCannotExpress(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		// format: uri -- a plain string satisfies the Go type and is a broken
		// link on every status the hook posts.
		"github_status.target_url is not a URI": {
			doc:  `{` + schemaURL + `, "command":["x"], "github_status":{"enabled":true,"context":"ci","target_url":"not a url"}}`,
			want: "target_url",
		},
		// minLength: 1 -- an empty template renders no title at all.
		"run_title is empty": {
			doc:  `{` + schemaURL + `, "command":["x"], "run_title":""}`,
			want: "run_title",
		},
		// The $schema field must be a URI: "s" used to load fine.
		"$schema is not a URI": {
			doc:  `{"$schema":"s", "command":["x"]}`,
			want: "$schema",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseHookDoc(t, tc.doc)
			require.Error(t, err, "the published schema must reject this")
			assert.Contains(t, err.Error(), "does not match the published schema")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// The gate runs LAST on purpose: where the Go model checks something, its
// message is the more actionable one and must survive.
func TestGoValidationMessagesWinOverTheSchema(t *testing.T) {
	err := parseHookDoc(t, `{`+schemaURL+`, "command":["x"], "schedule":"5 minutes"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid schedule")

	// Same for a timeout the Go parser rejects: the schema's pattern would
	// also catch it, but "invalid timeout" names what to fix.
	err = parseHookDoc(t, `{`+schemaURL+`, "command":["x"], "timeout":"5 minutes"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid timeout")
}

// A valid manifest passes both gates -- the schema must not reject what the
// repo actually ships.
func TestSchemaGateAcceptsARealisticManifest(t *testing.T) {
	require.NoError(t, parseHookDoc(t, `{`+schemaURL+`,
		"description": "does a thing",
		"command": ["node", "main.ts"],
		"timeout": "5m",
		"schedule": "1h",
		"state": true,
		"run_title": "{{repository.full_name}}",
		"api_key": "0123456789abcdef0123456789abcdef",
		"skip_if": [{"header:x-github-event": {"in": ["ping"]}}],
		"tests": [["npx", "tsc", "--noEmit"]]
	}`))
}

// The gate applies to managers too, against THEIR schema.
func TestManagerSchemaGate(t *testing.T) {
	root := writeManagerTree(t, "m1", `{
	  "$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json",
	  "command": ["run"],
	  "run_title": ""
	}`)
	_, errs := LoadManagers(DetectLayout(root))
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "does not match the published schema")
	assert.Contains(t, errs[0].Error(), "run_title")
}

// The embedded schemas must be compilable -- a broken one would otherwise turn
// every load into a schema-compile error (loud, but the wrong loud).
func TestEmbeddedSchemasCompile(t *testing.T) {
	v, err := hookSchemaValidator()
	require.NoError(t, err)
	require.NotNil(t, v.Schema())
	v, err = managerSchemaValidator()
	require.NoError(t, err)
	require.NotNil(t, v.Schema())
}
