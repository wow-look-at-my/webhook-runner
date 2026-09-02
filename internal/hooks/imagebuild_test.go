package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookJSONWithBase marshals a minimal manifest declaring the given base. The
// name is under test and can hold anything, so the encoder escapes it.
func hookJSONWithBase(t *testing.T, base string) string {
	t.Helper()
	doc, err := json.Marshal(map[string]any{
		"$schema":     "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json",
		"description": "test hook",
		"base":        base,
		"command":     []string{"sh", "-c", "echo hi"},
		"api_key":     "k",
	})
	require.NoError(t, err)
	return string(doc)
}

// writeBase creates <root>/src/base/<name>/Dockerfile.
func writeBase(t *testing.T, root, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "src", BaseDirName, name, DockerfileName), body)
}

// srcHookWithBase is a src-layout tree whose hook "a" declares base "runner".
// An empty build argument leaves that base absent from the tree.
func srcHookWithBase(t *testing.T, base string) string {
	t.Helper()
	root := t.TempDir()
	writeSrcHook(t, root, "a")
	writeFile(t, filepath.Join(root, "src", "hooks", "a", "hook.json"), hookJSONWithBase(t, "runner"))
	if base != "" {
		writeBase(t, root, base, "FROM alpine:3.20\nRUN true\n")
	}
	return root
}

func TestBaseLoads(t *testing.T) {
	root := srcHookWithBase(t, "runner")
	l := DetectLayout(root)
	hooks, errs := LoadLayout(l)
	require.Empty(t, errs)
	h := hooks["a"]
	require.NotNil(t, h)
	assert.Equal(t, "runner", h.Base)
	assert.Equal(t, filepath.Join(root, "src", BaseDirName, "runner"), h.BaseDir())
}

// A base directory that is not there fails at LOAD, never at build time: the
// entity drops before it can serve a delivery it cannot build for.
func TestMissingBaseDropsTheEntity(t *testing.T) {
	root := srcHookWithBase(t, "")
	hooks, errs := LoadLayout(DetectLayout(root))
	assert.NotContains(t, hooks, "a")
	assert.Contains(t, joinErrs(errs), "declares no Dockerfile")
}

// joinErrs renders a load's errors for a contains assertion: dropping the
// last entity also raises ZeroHooksError, so the error under test has company.
func joinErrs(errs []error) string {
	var b strings.Builder
	for _, err := range errs {
		b.WriteString(err.Error())
		b.WriteString("\n")
	}
	return b.String()
}

// A base directory holding no Dockerfile reads as present and cannot build.
func TestBaseWithoutADockerfileDropsTheEntity(t *testing.T) {
	root := srcHookWithBase(t, "")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", BaseDirName, "runner"), 0o755))
	hooks, errs := LoadLayout(DetectLayout(root))
	assert.NotContains(t, hooks, "a")
	assert.Contains(t, joinErrs(errs), "declares no Dockerfile")
}

// The name is a single path segment, so a manifest cannot name a sibling
// entity's directory or anything above the tree as its base. Both the
// published schema's pattern and checkBase refuse it, so the assertion is
// that the entity drops, never which layer said so.
func TestBaseNameCannotEscapeTheBaseDir(t *testing.T) {
	for _, name := range []string{"../hooks/a", "sub/dir", "..", "."} {
		root := t.TempDir()
		writeSrcHook(t, root, "a")
		writeFile(t, filepath.Join(root, "src", "hooks", "a", "hook.json"), hookJSONWithBase(t, name))
		hooks, errs := LoadLayout(DetectLayout(root))
		assert.NotContains(t, hooks, "a", name)
		assert.Contains(t, joinErrs(errs), "base", name)
	}
}

// A hook declaring no base keeps working and resolves no base directory --
// the field is additive, and every existing manifest omits it.
func TestNoBaseIsUnchanged(t *testing.T) {
	root := t.TempDir()
	writeSrcHook(t, root, "a")
	hooks, errs := LoadLayout(DetectLayout(root))
	require.Empty(t, errs)
	require.NotNil(t, hooks["a"])
	assert.Empty(t, hooks["a"].Base)
	assert.Empty(t, hooks["a"].BaseDir())
}

// The base tree is a shared dir, so editing it moves the CONSUMER's tag too:
// that is what stops a base change from leaving stale consumer images behind.
func TestEditingTheBaseRetagsTheConsumer(t *testing.T) {
	root := srcHookWithBase(t, "runner")
	hooks, errs := LoadLayout(DetectLayout(root))
	require.Empty(t, errs)
	before, err := hooks["a"].ContentHash()
	require.NoError(t, err)

	writeBase(t, root, "runner", "FROM alpine:3.20\nRUN true\nRUN echo more\n")
	hooks, errs = LoadLayout(DetectLayout(root))
	require.Empty(t, errs)
	after, err := hooks["a"].ContentHash()
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}

// DirHash answers for the base directory alone, so consumers sharing a base
// resolve the same tag and an unrelated edit elsewhere does not move it.
func TestDirHashCoversOnlyThatDirectory(t *testing.T) {
	root := srcHookWithBase(t, "runner")
	dir := filepath.Join(root, "src", BaseDirName, "runner")
	before, err := DirHash(dir)
	require.NoError(t, err)

	writeFile(t, filepath.Join(root, "src", "hooks", "a", "extra.ts"), "export const x = 1;\n")
	unchanged, err := DirHash(dir)
	require.NoError(t, err)
	assert.Equal(t, before, unchanged)

	writeBase(t, root, "runner", "FROM alpine:3.21\n")
	moved, err := DirHash(dir)
	require.NoError(t, err)
	assert.NotEqual(t, before, moved)
}
