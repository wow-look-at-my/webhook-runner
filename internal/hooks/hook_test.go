package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseInDir parses a hook.json document inside a fresh hook directory
// that ships the mandatory Dockerfile.
func parseInDir(t *testing.T, doc string) (*Hook, error) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, DockerfileName), []byte("FROM alpine\n"), 0o644))
	return Parse("h", filepath.Join(dir, "hook.json"), []byte(doc))
}

func TestParseValid(t *testing.T) {
	doc := `{
		// description supports JSONC comments
		"$schema": "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json",
		"description": "deploy",
		"command": ["sh", "-c", "echo hi"],
		"tests": [["sh", "-c", "true"], ["node", "--test", "x.test.ts"]],
		"timeout": "30s",
		"env": {"FOO": "bar"},
		"github_status": { "enabled": true, "context": "ci/deploy" }
	}`
	h, err := parseInDir(t, doc)
	require.Nil(t, err)

	assert.Equal(t, "h", h.ID)
	assert.Equal(t, [][]string{{"sh", "-c", "true"}, {"node", "--test", "x.test.ts"}}, h.Tests)

	got := h.Timeout()
	assert.Equal(t, 30*time.Second, got)

	assert.Equal(t, DefaultSignatureHeader, h.SigHeader())
}

func TestParseMinimal(t *testing.T) {
	// Command is optional: the image's CMD (from the Dockerfile) runs.
	// $schema is the only required field.
	h, err := parseInDir(t, `{"$schema":"s"}`)
	require.Nil(t, err)
	assert.Empty(t, h.Command)
}

// Omitting timeout falls back to DefaultTimeout — every hook keeps hang
// protection (5 minutes of silence) by default.
func TestTimeoutDefaultsWhenOmitted(t *testing.T) {
	h, err := parseInDir(t, `{"$schema":"s"}`)
	require.Nil(t, err)
	assert.Equal(t, DefaultTimeout, h.Timeout())
}

func TestParseScheduleValid(t *testing.T) {
	h, err := parseInDir(t, `{"$schema":"s","schedule":"5m"}`)
	require.Nil(t, err)
	assert.Equal(t, "5m", h.Schedule)
	assert.Equal(t, 5*time.Minute, h.ScheduleInterval())
}

func TestParseScheduleEmptyMeansUnscheduled(t *testing.T) {
	h, err := parseInDir(t, `{"$schema":"s"}`)
	require.Nil(t, err)
	assert.Equal(t, time.Duration(0), h.ScheduleInterval())
}

func TestParseScheduleInvalidDuration(t *testing.T) {
	_, err := parseInDir(t, `{"$schema":"s","schedule":"5 minutes"}`)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "invalid schedule")
}

func TestParseScheduleMustBePositive(t *testing.T) {
	_, err := parseInDir(t, `{"$schema":"s","schedule":"0s"}`)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "schedule must be positive")
}

func TestParseRequiresDockerfile(t *testing.T) {
	dir := t.TempDir() // no Dockerfile
	_, err := Parse("h", filepath.Join(dir, "hook.json"), []byte(`{}`))
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "Dockerfile")
}

func TestAPIKeyHdrDefault(t *testing.T) {
	h := &Hook{APIKey: "k"}
	assert.Equal(t, DefaultAPIKeyHeader, h.APIKeyHdr())
}

func TestSigHeaderLegacyDefault(t *testing.T) {
	h := &Hook{Secret: "s"}
	assert.Equal(t, LegacySignatureHeader, h.SigHeader())

}

func TestParseRejectsBadDocs(t *testing.T) {
	cases := map[string]string{
		// Go validation only checks that $schema is present (non-empty);
		// json-validator enforces it points at the published schema.
		"missing schema":       `{"command":["x"]}`,
		"image is not a field": `{"$schema":"s","image":"alpine"}`,
		"reserved env":         `{"$schema":"s","env":{"HOOK_PAYLOAD_FILE":"x"}}`,
		"empty test command":   `{"$schema":"s","tests":[["ok"],[]]}`,
		"bad timeout":          `{"$schema":"s","timeout":"banana"}`,
		"negative timeout":     `{"$schema":"s","timeout":"-1s"}`,
		// idle_timeout was removed when timeout itself became activity-based;
		// DisallowUnknownFields makes a hook.json that still sets it fail to
		// load (acceptable: nothing merged ever set it).
		"idle_timeout removed":    `{"$schema":"s","idle_timeout":"5m"}`,
		"github_status nocontext": `{"$schema":"s","github_status":{"enabled":true}}`,
		"unknown field":           `{"$schema":"s","frobnicate":true}`,
		"api_key+secret":          `{"$schema":"s","api_key":"k","secret":"s"}`,
		"public_key+secret":       `{"$schema":"s","public_key":"k","secret":"s"}`,
		"api_key+public_key":      `{"$schema":"s","api_key":"k","public_key":"k"}`,
		"bad public_key":          `{"$schema":"s","public_key":"not-a-key"}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseInDir(t, doc)
			require.NotNil(t, err)

		})
	}
}

func TestContentHash(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two"), 0o644))
	h := &Hook{ID: "h", SourcePath: filepath.Join(dir, "hook.json")}

	first, err := h.ContentHash()
	require.NoError(t, err)
	again, err := h.ContentHash()
	require.NoError(t, err)
	assert.Equal(t, first, again, "hash must be deterministic")
	assert.Len(t, first, 16)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644))
	changed, err := h.ContentHash()
	require.NoError(t, err)
	assert.NotEqual(t, first, changed, "content change must change the hash")

	_, err = (&Hook{ID: "nodisk"}).ContentHash()
	require.Error(t, err)
}

func TestStripComments(t *testing.T) {
	in := []byte(`{
        // line comment with "fake string"
        "key": "value /* not a comment */",
        /* block
           comment */
        "n": 1
    }`)
	got, err := readAll(stripComments(in))
	require.NoError(t, err)

	// The result must parse as JSON and preserve the in-string sequences.
	var parsed struct {
		Key string `json:"key"`
		N   int    `json:"n"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &parsed))
	assert.Equal(t, "value /* not a comment */", parsed.Key)
	assert.Equal(t, 1, parsed.N)

	// And the stripped output must not contain the literal comment text.
	assert.NotContains(t, got, "line comment")
	assert.NotContains(t, got, "block")
}

func makeScriptHookDir(t *testing.T, scriptName, scriptContent string) (hookDir, hookJSON string) {
	t.Helper()
	root := t.TempDir()
	hookDir = filepath.Join(root, "my-hook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, scriptName), []byte(scriptContent), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644))
	hookJSON = filepath.Join(hookDir, "hook.json")
	return hookDir, hookJSON
}

const testSchema = `"$schema":"https://wow-look-at-my.github.io/webhook-runner/hook.schema.json"`

func TestScriptResolveBash(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash\necho hi")
	doc := []byte(`{` + testSchema + `,"script":{"file":"run.sh","interpreter":"bash"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "run.sh"}, h.Command)
}

func TestScriptResolvePwsh(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.ps1", "Write-Host hi")
	doc := []byte(`{` + testSchema + `,"script":{"file":"run.ps1","interpreter":"pwsh"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"pwsh", "-File", "run.ps1"}, h.Command)
}

func TestScriptResolveNode(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "index.js", "console.log('hi')")
	doc := []byte(`{` + testSchema + `,"script":{"file":"index.js","interpreter":"node"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"node", "index.js"}, h.Command)
}

func TestScriptResolveTsx(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "handler.ts", "console.log('hi')")
	doc := []byte(`{` + testSchema + `,"script":{"file":"handler.ts","interpreter":"tsx"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"tsx", "handler.ts"}, h.Command)
}

func TestScriptWithArgs(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	doc := []byte(`{` + testSchema + `,"script":{"file":"run.sh","interpreter":"bash","args":["--verbose","--dry-run"]}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "run.sh", "--verbose", "--dry-run"}, h.Command)
}

func TestScriptCommandOverride(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	doc := []byte(`{` + testSchema + `,"script":{"file":"run.sh","interpreter":"bash"},"command":["sh","run.sh"]}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "run.sh"}, h.Command)
}

func TestScriptRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "my-hook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	outside := filepath.Join(root, "evil.sh")
	require.NoError(t, os.WriteFile(outside, []byte("rm -rf /"), 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(hookDir, "run.sh")))

	hookJSON := filepath.Join(hookDir, "hook.json")
	doc := []byte(`{` + testSchema + `,"script":{"file":"run.sh","interpreter":"bash"}}`)
	_, err := Parse("my-hook", hookJSON, doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves outside hook directory")
}

func TestScriptRejectsMissingFile(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "my-hook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	hookJSON := filepath.Join(hookDir, "hook.json")

	doc := []byte(`{` + testSchema + `,"script":{"file":"nope.sh","interpreter":"bash"}}`)
	_, err := Parse("my-hook", hookJSON, doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope.sh")
}

func TestScriptRejectsBadInterpreter(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.rb", "puts 'hi'")
	doc := []byte(`{` + testSchema + `,"script":{"file":"run.rb","interpreter":"ruby"}}`)
	_, err := Parse("my-hook", hookJSON, doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestScriptRejectsMissingFields(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	cases := map[string]string{
		"missing file":        `{` + testSchema + `,"script":{"interpreter":"bash"}}`,
		"missing interpreter": `{` + testSchema + `,"script":{"file":"run.sh"}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("h", hookJSON, []byte(doc))
			require.Error(t, err)
		})
	}
}

func readAll(r interface{ Read(p []byte) (int, error) }) (string, error) {
	var b strings.Builder
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			if err.Error() == "EOF" {
				return b.String(), nil
			}
			return b.String(), err
		}
	}
}
