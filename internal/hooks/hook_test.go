package hooks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseValid(t *testing.T) {
	doc := []byte(`{
		// description supports JSONC comments
		"description": "deploy",
		"image": "alpine:3.20",
		"command": ["sh", "-c", "echo hi"],
		"tests": [["sh", "-c", "true"], ["node", "--test", "x.test.ts"]],
		"timeout": "30s",
		"env": {"FOO": "bar"},
		"github_status": { "enabled": true, "context": "ci/deploy" }
	}`)
	h, err := Parse("deploy-frontend", "/some/path/hook.json", doc)
	require.Nil(t, err)

	assert.Equal(t, "deploy-frontend", h.ID)
	assert.Equal(t, [][]string{{"sh", "-c", "true"}, {"node", "--test", "x.test.ts"}}, h.Tests)

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
		"missing image":           `{"command":["x"]}`,
		"missing command":         `{"image":"alpine"}`,
		"empty command":           `{"image":"alpine","command":[]}`,
		"reserved env":            `{"image":"alpine","command":["x"],"env":{"HOOK_PAYLOAD_FILE":"x"}}`,
		"reserved env hook_dir":   `{"image":"alpine","command":["x"],"env":{"HOOK_DIR":"x"}}`,
		"empty test command":      `{"image":"alpine","command":["x"],"tests":[["ok"],[]]}`,
		"bad timeout":             `{"image":"alpine","command":["x"],"timeout":"banana"}`,
		"negative timeout":        `{"image":"alpine","command":["x"],"timeout":"-1s"}`,
		"github_status nocontext": `{"image":"alpine","command":["x"],"github_status":{"enabled":true}}`,
		"unknown field":           `{"image":"alpine","command":["x"],"frobnicate":true}`,
		"api_key+secret":          `{"image":"alpine","command":["x"],"api_key":"k","secret":"s"}`,
		"public_key+secret":       `{"image":"alpine","command":["x"],"public_key":"k","secret":"s"}`,
		"api_key+public_key":      `{"image":"alpine","command":["x"],"api_key":"k","public_key":"k"}`,
		"bad public_key":          `{"image":"alpine","command":["x"],"public_key":"not-a-key"}`,
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
