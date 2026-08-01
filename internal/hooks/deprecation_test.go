package hooks

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseHook parses one manifest in a throwaway hook dir carrying the
// mandatory Dockerfile.
func parseHook(t *testing.T, doc string) (*Hook, error) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, DockerfileName), "FROM alpine\n")
	return Parse("h", filepath.Join(dir, "hook.json"), []byte(doc))
}

// The whole point of the deprecation: `env` still LOADS. A runner that
// rejected it could not serve the fleet that still declares it, and there is
// no rollback from that -- the tree never changed, the binary did.
func TestEnvStillLoads(t *testing.T) {
	h, err := parseHook(t, `{`+schemaURL+`, "command": ["true"], "env": {"A": "1", "B": "${SECRET}"}}`)
	require.NoError(t, err, "a manifest with env must still load")
	assert.Equal(t, map[string]string{"A": "1", "B": "${SECRET}"}, h.Env,
		"the values must survive parsing -- a deprecation that stops working is worse than the flag day")
}

// ...and is reported, every time, by identity rather than by message text.
func TestEnvIsReportedAsDeprecated(t *testing.T) {
	h, err := parseHook(t, `{`+schemaURL+`, "command": ["true"], "env": {"A": "1"}}`)
	require.NoError(t, err)

	deps := h.Deprecations()
	require.Len(t, deps, 1)
	assert.Equal(t, "env", deps[0].Field)
	assert.Contains(t, deps[0].Message, "settings", "the advice must name the replacement")
	assert.Contains(t, deps[0].Message, "removed in the next", "and say the reprieve is temporary")
}

// A clean manifest reports nothing -- otherwise the surface is noise and
// nobody reads it.
func TestCleanManifestHasNoDeprecations(t *testing.T) {
	h, err := parseHook(t, `{`+schemaURL+`, "command": ["true"]}`)
	require.NoError(t, err)
	assert.Empty(t, h.Deprecations())

	var nilHook *Hook
	assert.Empty(t, nilHook.Deprecations(), "nil-safe: the reload path ranges over this unconditionally")
}

// env and settings are independent, so a half-migrated entity is a legal
// state: that is what makes migrating one key at a time possible.
func TestEnvAndSettingsCoexist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, DockerfileName), "FROM alpine\n")
	writeFile(t, filepath.Join(dir, SettingsSchemaFile), `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)

	h, err := Parse("h", filepath.Join(dir, "hook.json"), []byte(`{`+schemaURL+`,
	  "command": ["true"], "env": {"A": "1"}, "settings": {"n": 7}}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1"}, h.Env)
	assert.JSONEq(t, `{"n": 7}`, string(h.SettingsJSON()))
	assert.Len(t, h.Deprecations(), 1, "the env half is still flagged")
}

// CollectDeprecations covers managers too (they embed *Hook) and orders by
// entity id, so the log, the feed and the attention surface agree.
func TestCollectDeprecationsCoversManagersAndSorts(t *testing.T) {
	hooksIn := map[string]*Hook{
		"zeta":  {ID: "zeta", Env: map[string]string{"A": "1"}},
		"alpha": {ID: "alpha", Env: map[string]string{"A": "1"}},
		"clean": {ID: "clean"},
	}
	managers := map[string]*Manager{
		"mgr": {Hook: &Hook{ID: "mgr", Env: map[string]string{"B": "2"}}},
		"nil": nil,
	}
	deps := CollectDeprecations(hooksIn, managers)
	require.Len(t, deps, 3)
	assert.Equal(t, []string{"alpha", "mgr", "zeta"},
		[]string{deps[0].EntityID, deps[1].EntityID, deps[2].EntityID})
}
