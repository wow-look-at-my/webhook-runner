package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -- Tree builders -----------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

const minimalHookJSON = `{
  "$schema": "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json",
  "description": "test hook",
  "command": ["sh", "-c", "echo hi"],
  "api_key": "k"
}`

// writeLegacyHook creates <root>/<id>/{hook.json,Dockerfile}.
func writeLegacyHook(t *testing.T, root, id string) {
	t.Helper()
	writeFile(t, filepath.Join(root, id, "hook.json"), minimalHookJSON)
	writeFile(t, filepath.Join(root, id, "Dockerfile"), "FROM alpine:3.20\n")
}

// writeSrcHook creates <root>/src/hooks/<id>/{hook.json,Dockerfile,main.ts}.
func writeSrcHook(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, "src", "hooks", id)
	writeFile(t, filepath.Join(dir, "hook.json"), minimalHookJSON)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM alpine:3.20\nCOPY sdk/ /app/sdk/\nCOPY hooks/"+id+"/ /app/hooks/"+id+"/\nWORKDIR /app/hooks/"+id+"\n")
	writeFile(t, filepath.Join(dir, "main.ts"), "import { greet } from '../../sdk/util.ts';\nconsole.log(greet('x'));\n")
}

// -- Detection ---------------------------------------------------------------

func TestDetectLayout(t *testing.T) {
	legacy := t.TempDir()
	writeLegacyHook(t, legacy, "a")
	l := DetectLayout(legacy)
	assert.False(t, l.SDK)
	assert.Equal(t, "legacy", l.String())
	assert.Equal(t, legacy, l.HooksDir())
	assert.Equal(t, filepath.Join(legacy, "concurrency.json"), l.ConcurrencyPath())
	assert.Empty(t, l.SrcDir())
	shared, err := l.SharedDirs()
	require.NoError(t, err)
	assert.Empty(t, shared)

	src := t.TempDir()
	writeSrcHook(t, src, "a")
	l = DetectLayout(src)
	assert.True(t, l.SDK)
	assert.Equal(t, "src", l.String())
	assert.Equal(t, filepath.Join(src, "src", "hooks"), l.HooksDir())
	assert.Equal(t, filepath.Join(src, "cfg", "concurrency.json"), l.ConcurrencyPath())
	assert.Equal(t, filepath.Join(src, "src"), l.SrcDir())
	// Every shared dir, lexical, entity trees excluded — not just src/sdk.
	writeFile(t, filepath.Join(src, "src", "sdk", "util.ts"), "export const x = 1;\n")
	writeFile(t, filepath.Join(src, "src", "actions-runner", "jit.ts"), "export const y = 2;\n")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "src", "managers"), 0o755))
	shared, err = l.SharedDirs()
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(src, "src", "actions-runner"), filepath.Join(src, "src", "sdk")}, shared)

	// src/hooks must be a DIRECTORY — a stray file doesn't flip the layout.
	odd := t.TempDir()
	writeFile(t, filepath.Join(odd, "src", "hooks"), "not a dir")
	assert.False(t, DetectLayout(odd).SDK)
}

// -- Loading -----------------------------------------------------------------

func TestLoadLayoutSrcTree(t *testing.T) {
	root := t.TempDir()
	writeSrcHook(t, root, "alpha")
	writeSrcHook(t, root, "beta")
	writeFile(t, filepath.Join(root, "src", "sdk", "util.ts"), "export const x = 1;\n")
	// A stray legacy-shaped dir at the root: must be IGNORED, loudly.
	writeLegacyHook(t, root, "stray")
	// A root-level non-hook dir: silently fine (README folders etc.).
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))

	loaded, errs := LoadDir(root)
	require.Len(t, loaded, 2)
	require.Contains(t, loaded, "alpha")
	require.Contains(t, loaded, "beta")
	assert.True(t, loaded["alpha"].SDKLayout())
	assert.Equal(t, filepath.Join(root, "src"), loaded["alpha"].SrcRoot)
	assert.Equal(t, filepath.Join(root, "src"), loaded["alpha"].BuildContext())

	// Exactly one error: the mixed-layout stray dir, named. It must read as a HARD ERROR, not a silent skip (the operator's failsafe).
	require.Len(t, errs, 1)
	var ignored IgnoredLegacyDirError
	require.True(t, errors.As(errs[0], &ignored), "want IgnoredLegacyDirError, got %v", errs[0])
	assert.Contains(t, ignored.Dir, "stray")
	assert.Contains(t, ignored.Error(), "src layout")
	assert.Contains(t, ignored.Error(), "mixed hook layout")
	assert.Contains(t, ignored.Error(), "hard error")
	assert.Contains(t, ignored.Error(), "stray")
}

// -- The mixed-layout guard ----------------------------------------------------

// A stray top-level hook alongside src/hooks/ is a HARD ERROR, never a
// silent skip — the failsafe an incomplete move to the src layout must
// trip. Scoped to MIXED layouts ONLY: pure-src and pure-legacy both load
// clean, so the runner's own legacy fixtures (examples/hooks, e2e/hooks)
// are unaffected.
func TestMixedLayoutRejectsTopLevelHooks(t *testing.T) {
	// (a) MIXED: src/hooks/<id> AND a top-level <other>/hook.json ⇒ the typed error, naming the offending top-level dir, and it does NOT leak.
	mixed := t.TempDir()
	writeSrcHook(t, mixed, "alpha")
	writeLegacyHook(t, mixed, "leftover")
	loaded, errs := LoadDir(mixed)
	require.Len(t, loaded, 1, "only the src hook loads; the top-level dir must be rejected")
	require.Contains(t, loaded, "alpha")
	require.NotContains(t, loaded, "leftover", "a mixed-layout top-level dir must never enter the registry")
	require.Len(t, errs, 1)
	var mix IgnoredLegacyDirError
	require.True(t, errors.As(errs[0], &mix), "want IgnoredLegacyDirError, got %v", errs[0])
	assert.Contains(t, mix.Dir, "leftover", "the error must name the offending top-level dir")
	assert.Contains(t, mix.Error(), "not a silent skip")

	// (b) PURE-SRC: only src/hooks/, no top-level hook dirs ⇒ loads clean.
	pureSrc := t.TempDir()
	writeSrcHook(t, pureSrc, "alpha")
	writeSrcHook(t, pureSrc, "beta")
	loaded, errs = LoadDir(pureSrc)
	assert.Empty(t, errs, "a pure-src tree must load without errors")
	assert.Len(t, loaded, 2)

	// (c) PURE-LEGACY: only top-level hook dirs, no src/hooks/ ⇒ loads clean and is never scanned for the mixed-layout error (this is exactly the.
	pureLegacy := t.TempDir()
	writeLegacyHook(t, pureLegacy, "one")
	writeLegacyHook(t, pureLegacy, "two")
	loaded, errs = LoadDir(pureLegacy)
	assert.Empty(t, errs, "a pure-legacy tree must load without errors")
	assert.Len(t, loaded, 2)
	assert.False(t, DetectLayout(pureLegacy).SDK, "a top-level-only tree is legacy, never scanned for mixed-layout")
}

func TestLoadLayoutLegacyUnchanged(t *testing.T) {
	root := t.TempDir()
	writeLegacyHook(t, root, "a")
	loaded, errs := LoadDir(root)
	assert.Empty(t, errs)
	require.Len(t, loaded, 1)
	assert.False(t, loaded["a"].SDKLayout())
	assert.Equal(t, loaded["a"].Dir(), loaded["a"].BuildContext())
}

func TestLoadFixtureTree(t *testing.T) {
	loaded, errs := LoadDir(filepath.Join("testdata", "srclayout"))
	assert.Empty(t, errs)
	require.Contains(t, loaded, "demo-ts")
	h := loaded["demo-ts"]
	assert.True(t, h.SDKLayout())
	hash, err := h.ContentHash()
	require.NoError(t, err)
	assert.Len(t, hash, 16)
}

// -- The zero-hooks guard ------------------------------------------------------

func TestZeroHooksIsLoud(t *testing.T) {
	// Empty dir: nothing to serve — must be a loud, typed error.
	_, errs := LoadDir(t.TempDir())
	require.Len(t, errs, 1)
	var zero ZeroHooksError
	require.True(t, errors.As(errs[0], &zero))
	assert.Contains(t, zero.Error(), "no hooks loaded")

	// The dry-run disaster shape: a src-restructured tree WITHOUT src/hooks (or scanned by anything that falls back to legacy rules) yields.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "misc", "note.txt"), "not hooks")
	loaded, errs := LoadDir(root)
	assert.Empty(t, loaded)
	require.Len(t, errs, 1)
	require.True(t, errors.As(errs[0], &zero))
	assert.Contains(t, zero.Error(), "legacy layout", "the message must name the layout that was scanned")
}

// -- Content hashing -----------------------------------------------------------

// The legacy algorithm must stay byte-identical across this change:
// existing deployments must not re-tag (and so re-build) every hook on
// upgrade. Golden value computed from the historical algorithm (relative
// path + \x00 + content + \x00 per file, lexical walk, sha256 hex[:16]).
func TestContentHashLegacyByteIdentical(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "golden")
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM alpine:3.20\nCMD [\"sh\", \"-c\", \"echo golden\"]\n")
	writeFile(t, filepath.Join(dir, "hook.json"), "{\n  \"$schema\": \"https://wow-look-at-my.github.io/webhook-runner/hook.schema.json\",\n  \"description\": \"golden legacy fixture\",\n  \"command\": [\"sh\", \"-c\", \"echo golden\"],\n  \"api_key\": \"golden-key\"\n}\n")
	writeFile(t, filepath.Join(dir, "payload.txt"), "fixed bytes\n")

	loaded, errs := LoadDir(root)
	require.Empty(t, errs)
	h := loaded["golden"]
	hash, err := h.ContentHash()
	require.NoError(t, err)
	assert.Equal(t, "c1b0e5de5444c992", hash,
		"legacy content hashing changed — every deployed hook would re-tag on upgrade")
}

func TestContentHashSDKSensitivity(t *testing.T) {
	root := t.TempDir()
	writeSrcHook(t, root, "a")
	writeSrcHook(t, root, "b")
	writeFile(t, filepath.Join(root, "src", "sdk", "util.ts"), "export function greet(n: string) { return n; }\n")

	load := func() (*Hook, *Hook) {
		loaded, errs := LoadDir(root)
		require.Empty(t, errs)
		return loaded["a"], loaded["b"]
	}
	hash := func(h *Hook) string {
		v, err := h.ContentHash()
		require.NoError(t, err)
		return v
	}

	a, b := load()
	a1, b1 := hash(a), hash(b)
	assert.NotEqual(t, a1, b1, "different hooks must not share a hash")

	// An sdk edit re-tags EVERY src-layout hook.
	writeFile(t, filepath.Join(root, "src", "sdk", "util.ts"), "export function greet(n: string) { return 'hi ' + n; }\n")
	a2, b2 := hash(a), hash(b)
	assert.NotEqual(t, a1, a2, "sdk edit must re-tag hook a")
	assert.NotEqual(t, b1, b2, "sdk edit must re-tag hook b")

	// An edit to hook B never re-tags hook A.
	writeFile(t, filepath.Join(root, "src", "hooks", "b", "main.ts"), "console.log('changed');\n")
	a3, b3 := hash(a), hash(b)
	assert.Equal(t, a2, a3, "sibling edit must not re-tag hook a")
	assert.NotEqual(t, b2, b3, "own edit must re-tag hook b")

	// Mode changes count under the SDK layout (relative path + bytes + mode).
	require.NoError(t, os.Chmod(filepath.Join(root, "src", "hooks", "a", "main.ts"), 0o755))
	a4, _ := hash(a), hash(b)
	assert.NotEqual(t, a3, a4, "mode change must re-tag under the src layout")

	// A src tree WITHOUT an sdk dir still hashes (shared code is optional).
	bare := t.TempDir()
	writeSrcHook(t, bare, "solo")
	loaded, errs := LoadDir(bare)
	require.Empty(t, errs)
	_, err := loaded["solo"].ContentHash()
	assert.NoError(t, err)
}

// EVERY shared dir is hashed, not just src/sdk — the property that lets
// shared code be organized by what it is. A shared dir the tag ignores is
// the worst outcome available: editing it re-tags nothing, so every
// consumer silently keeps running the old copy.
func TestContentHashCoversEverySharedDir(t *testing.T) {
	root := t.TempDir()
	writeSrcHook(t, root, "a")
	writeFile(t, filepath.Join(root, "src", "sdk", "util.ts"), "export const x = 1;\n")
	writeFile(t, filepath.Join(root, "src", "actions-runner", "jit.ts"), "export const mint = 1;\n")

	hash := func() string {
		loaded, errs := LoadDir(root)
		require.Empty(t, errs)
		v, err := loaded["a"].ContentHash()
		require.NoError(t, err)
		return v
	}

	before := hash()
	writeFile(t, filepath.Join(root, "src", "actions-runner", "jit.ts"), "export const mint = 2;\n")
	assert.NotEqual(t, before, hash(), "an edit in a non-sdk shared dir must re-tag its consumers")

	// A NEW shared dir joins the hash the moment it exists.
	mid := hash()
	writeFile(t, filepath.Join(root, "src", "wire", "proto.ts"), "export const v = 1;\n")
	assert.NotEqual(t, mid, hash(), "adding a shared dir must re-tag")
}

// An sdk-only tree must hash EXACTLY as it did before shared dirs were
// generalized: the deployed fleet is sdk-only, and a changed algorithm
// re-tags and rebuilds every image on upgrade for no reason. Golden value
// computed from the pre-generalization code (hook dir, then src/sdk).
func TestContentHashSDKOnlyUnchanged(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "src", "hooks", "golden")
	writeFile(t, filepath.Join(dir, "hook.json"), minimalHookJSON)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM alpine:3.20\n")
	writeFile(t, filepath.Join(root, "src", "sdk", "util.ts"), "export const x = 1;\n")

	loaded, errs := LoadDir(root)
	require.Empty(t, errs)
	hash, err := loaded["golden"].ContentHash()
	require.NoError(t, err)
	assert.Equal(t, "da957cfcb483a28b", hash,
		"src-layout content hashing changed for an sdk-only tree — every deployed entity would re-tag on upgrade")
}
