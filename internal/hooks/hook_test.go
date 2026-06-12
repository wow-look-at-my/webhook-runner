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
	h, err := parseInDir(t, `{}`)
	require.Nil(t, err)
	assert.Empty(t, h.Command)
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
		"image is not a field":    `{"image":"alpine"}`,
		"reserved env":            `{"env":{"HOOK_PAYLOAD_FILE":"x"}}`,
		"empty test command":      `{"tests":[["ok"],[]]}`,
		"bad timeout":             `{"timeout":"banana"}`,
		"negative timeout":        `{"timeout":"-1s"}`,
		"github_status nocontext": `{"github_status":{"enabled":true}}`,
		"unknown field":           `{"frobnicate":true}`,
		"api_key+secret":          `{"api_key":"k","secret":"s"}`,
		"public_key+secret":       `{"public_key":"k","secret":"s"}`,
		"api_key+public_key":      `{"api_key":"k","public_key":"k"}`,
		"bad public_key":          `{"public_key":"not-a-key"}`,
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
