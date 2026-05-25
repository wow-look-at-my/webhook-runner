package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestParseValid(t *testing.T) {
	doc := []byte(`{
		// description supports JSONC comments
		"description": "deploy",
		"image": "alpine:3.20",
		"command": ["sh", "-c", "echo hi"],
		"timeout": "30s",
		"env": {"FOO": "bar"},
		"github_status": { "enabled": true, "context": "ci/deploy" }
	}`)
	h, err := Parse("deploy-frontend", "/some/path/hook.json", doc)
	require.Nil(t, err)

	assert.Equal(t, "deploy-frontend", h.ID)

	got := h.Timeout()
	assert.Equal(t, 30*time.Second, got)

	assert.Equal(t, DefaultSignatureHeader, h.SigHeader())
}

func TestAPIKeyHdrDefault(t *testing.T) {
	h := &Hook{APIKey: "k"}
	assert.Equal(t, DefaultAPIKeyHeader, h.APIKeyHdr())
}

func TestSigHeaderLegacyDefault(t *testing.T) {
	h := &Hook{Secret: "s"}
	assert.Equal(t, LegacySignatureHeader, h.SigHeader())

}

func TestParseRejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"missing image":		`{"command":["x"]}`,
		"missing command":		`{"image":"alpine"}`,
		"empty command":		`{"image":"alpine","command":[]}`,
		"reserved env":			`{"image":"alpine","command":["x"],"env":{"HOOK_PAYLOAD_FILE":"x"}}`,
		"bad timeout":			`{"image":"alpine","command":["x"],"timeout":"banana"}`,
		"negative timeout":		`{"image":"alpine","command":["x"],"timeout":"-1s"}`,
		"github_status nocontext":	`{"image":"alpine","command":["x"],"github_status":{"enabled":true}}`,
		"unknown field":		`{"image":"alpine","command":["x"],"frobnicate":true}`,
		"api_key+secret":		`{"image":"alpine","command":["x"],"api_key":"k","secret":"s"}`,
		"public_key+secret":		`{"image":"alpine","command":["x"],"public_key":"k","secret":"s"}`,
		"api_key+public_key":		`{"image":"alpine","command":["x"],"api_key":"k","public_key":"k"}`,
		"bad public_key":		`{"image":"alpine","command":["x"],"public_key":"not-a-key"}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("h", "p", []byte(doc))
			require.NotNil(t, err)

		})
	}
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
	hookJSON = filepath.Join(hookDir, "hook.json")
	return hookDir, hookJSON
}

func TestScriptResolveBash(t *testing.T) {
	hookDir, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash\necho hi")
	doc := []byte(`{"script":{"file":"run.sh","interpreter":"bash"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, "bash:5", h.Image)
	assert.Equal(t, []string{"bash", "/opt/hook/run.sh"}, h.Command)
	assert.Contains(t, h.Volumes, hookDir+":/opt/hook:ro")
}

func TestScriptResolvePwsh(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.ps1", "Write-Host hi")
	doc := []byte(`{"script":{"file":"run.ps1","interpreter":"pwsh"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, "mcr.microsoft.com/powershell:lts-alpine-3.20", h.Image)
	assert.Equal(t, []string{"pwsh", "-File", "/opt/hook/run.ps1"}, h.Command)
}

func TestScriptResolveNode(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "index.js", "console.log('hi')")
	doc := []byte(`{"script":{"file":"index.js","interpreter":"node"}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, "node:22-alpine", h.Image)
	assert.Equal(t, []string{"node", "/opt/hook/index.js"}, h.Command)
}

func TestScriptWithArgs(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	doc := []byte(`{"script":{"file":"run.sh","interpreter":"bash","args":["--verbose","--dry-run"]}}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "/opt/hook/run.sh", "--verbose", "--dry-run"}, h.Command)
}

func TestScriptImageOverride(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	doc := []byte(`{"script":{"file":"run.sh","interpreter":"bash"},"image":"alpine:3.20"}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, "alpine:3.20", h.Image)
	assert.Equal(t, []string{"bash", "/opt/hook/run.sh"}, h.Command)
}

func TestScriptCommandOverride(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	doc := []byte(`{"script":{"file":"run.sh","interpreter":"bash"},"command":["sh","/opt/hook/run.sh"]}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "/opt/hook/run.sh"}, h.Command)
}

func TestScriptVolumesAppended(t *testing.T) {
	hookDir, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	doc := []byte(`{"script":{"file":"run.sh","interpreter":"bash"},"volumes":["/data:/data:ro"]}`)
	h, err := Parse("my-hook", hookJSON, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{"/data:/data:ro", hookDir + ":/opt/hook:ro"}, h.Volumes)
}

func TestScriptRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "my-hook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	outside := filepath.Join(root, "evil.sh")
	require.NoError(t, os.WriteFile(outside, []byte("rm -rf /"), 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(hookDir, "run.sh")))

	hookJSON := filepath.Join(hookDir, "hook.json")
	doc := []byte(`{"script":{"file":"run.sh","interpreter":"bash"}}`)
	_, err := Parse("my-hook", hookJSON, doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves outside hook directory")
}

func TestScriptRejectsMissingFile(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "my-hook")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	hookJSON := filepath.Join(hookDir, "hook.json")

	doc := []byte(`{"script":{"file":"nope.sh","interpreter":"bash"}}`)
	_, err := Parse("my-hook", hookJSON, doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope.sh")
}

func TestScriptRejectsBadInterpreter(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.rb", "puts 'hi'")
	doc := []byte(`{"script":{"file":"run.rb","interpreter":"ruby"}}`)
	_, err := Parse("my-hook", hookJSON, doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestScriptRejectsMissingFields(t *testing.T) {
	_, hookJSON := makeScriptHookDir(t, "run.sh", "#!/bin/bash")
	cases := map[string]string{
		"missing file":        `{"script":{"interpreter":"bash"}}`,
		"missing interpreter": `{"script":{"file":"run.sh"}}`,
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
