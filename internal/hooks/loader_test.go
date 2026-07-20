package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeHook(t *testing.T, root, id, body string) {
	t.Helper()
	dir := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, DockerfileName), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook.json"), []byte(body), 0o644))
}

func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "good", `{"$schema":"s","command":["x"]}`)
	writeHook(t, root, "broken", `{"$schema":"s","image":"alpine"}`) // image is no longer a field
	require.NoError(t, os.MkdirAll(filepath.Join(root, "no-hook"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte("ignore me"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".hidden"), 0o755))
	writeHook(t, root, ".hidden", `{"command":["x"]}`)

	hooks, errs := LoadDir(root)
	assert.Len(t, hooks, 1)
	assert.Contains(t, hooks, "good")
	assert.NotContains(t, hooks, "broken")
	assert.NotContains(t, hooks, "no-hook")
	assert.NotContains(t, hooks, ".hidden")
	assert.Len(t, errs, 1)
}

func TestLoadDirMissing(t *testing.T) {
	hooks, errs := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.Empty(t, hooks)
	assert.NotEmpty(t, errs)
}

func TestLoadOne(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "deploy", `{
		// pretty
		"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
		"description": "Deploy",
		"command": ["echo"]
	}`)
	h, err := LoadOne(root, "deploy")
	require.NoError(t, err)
	assert.Equal(t, "deploy", h.ID)
	assert.Equal(t, "Deploy", h.Description)
}

func TestRegistryReplaceAndList(t *testing.T) {
	r := NewRegistry()
	r.Replace(map[string]*Hook{
		"a": {ID: "a", Description: "alpha"},
		"b": {ID: "b", Description: "beta", Synchronous: true},
	})
	out := r.List()
	require.Len(t, out, 2)
	assert.Equal(t, "a", out[0].ID)
	assert.False(t, out[0].Synchronous)
	assert.True(t, out[1].Synchronous)

	r.Set(&Hook{ID: "c", Description: "gamma"})
	got, ok := r.Get("c")
	assert.True(t, ok)
	assert.Equal(t, "gamma", got.Description)

	r.Delete("a")
	_, ok = r.Get("a")
	assert.False(t, ok)
}
