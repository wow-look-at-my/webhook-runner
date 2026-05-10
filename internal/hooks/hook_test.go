package hooks

import (
	"strings"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if h.ID != "deploy-frontend" {
		t.Errorf("ID = %q", h.ID)
	}
	if got := h.Timeout(); got != 30*time.Second {
		t.Errorf("Timeout = %v", got)
	}
	if h.SigHeader() != DefaultSignatureHeader {
		t.Errorf("SigHeader = %q", h.SigHeader())
	}
}

func TestParseRejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"missing image":       `{"command":["x"]}`,
		"missing command":     `{"image":"alpine"}`,
		"empty command":       `{"image":"alpine","command":[]}`,
		"reserved env":        `{"image":"alpine","command":["x"],"env":{"HOOK_PAYLOAD_FILE":"x"}}`,
		"bad timeout":         `{"image":"alpine","command":["x"],"timeout":"banana"}`,
		"negative timeout":    `{"image":"alpine","command":["x"],"timeout":"-1s"}`,
		"github_status nocontext": `{"image":"alpine","command":["x"],"github_status":{"enabled":true}}`,
		"unknown field":       `{"image":"alpine","command":["x"],"frobnicate":true}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("h", "p", []byte(doc)); err == nil {
				t.Fatalf("expected error for %q", doc)
			}
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
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "//") || strings.Contains(got, "/*") {
		t.Errorf("comments survived: %s", got)
	}
	if !strings.Contains(got, `"value /* not a comment */"`) {
		t.Errorf("string contents mangled: %s", got)
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
