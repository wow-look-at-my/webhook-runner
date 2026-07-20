package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeManager materializes a manager dir (manager.json + Dockerfile) and
// returns the tree root, laid out as an SDK tree (src/managers/<id>).
func writeManagerTree(t *testing.T, id, managerJSON string) string {
	t.Helper()
	root := t.TempDir()
	// The SDK layout detection rule: src/hooks must exist.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "hooks"), 0o755))
	dir := filepath.Join(root, "src", "managers", id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manager.json"), []byte(managerJSON), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644))
	return root
}

const minimalManager = `{
  "$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json",
  "description": "test manager",
  "secret": "s3cret",
  "reconcile_interval": "3m",
  "command": ["run"]
}`

func TestParseManagerFullFieldSet(t *testing.T) {
	root := writeManagerTree(t, "m1", `{
	  "$schema": "x",
	  "description": "full",
	  "enable": true,
	  "reconcile_interval": "90s",
	  "timeout": "10m",
	  "command": ["serve"],
	  "tests": [["true"]],
	  "networks": ["net"],
	  "volumes": ["/a:/b"],
	  "env": {"K": "v"},
	  "user": "1000",
	  "workdir": "/w",
	  "extra_docker_args": ["--label", "x"],
	  "dind": true,
	  "concurrency_group": "g",
	  "run_title": "manager {{x}}",
	  "github_status": {"enabled": true, "context": "mgr"},
	  "synchronous": true,
	  "secret": "s",
	  "skip_if": [ {"header:x-github-event": {"ne": "workflow_job"}} ]
	}`)
	ms, errs := LoadManagers(DetectLayout(root))
	require.Empty(t, errs)
	m := ms["m1"]
	require.NotNil(t, m)
	assert.Equal(t, 90*time.Second, m.ReconcileInterval())
	assert.True(t, m.EnabledByDefault())
	assert.True(t, m.Dind)
	assert.True(t, m.Synchronous)
	assert.Equal(t, "g", m.ConcurrencyGroup)
	assert.Equal(t, "mgr", m.GitHubStatus.Context)
	assert.True(t, m.State, "state is implied true — the inbox/KV ride the socket")
	assert.NotEmpty(t, m.SrcRoot, "SDK build-context semantics apply")
}

// Managers share the hook enable default: absent `enable` means ENABLED —
// a declared manager works the moment it deploys (operator ruling:
// features ship enabled, never dormant-gated). Only an explicit
// enable:false (or the operator's dashboard switch) makes one dormant.
func TestManagerDefaultsEnabled(t *testing.T) {
	root := writeManagerTree(t, "m1", minimalManager)
	ms, errs := LoadManagers(DetectLayout(root))
	require.Empty(t, errs)
	assert.True(t, ms["m1"].EnabledByDefault(), "absent enable must mean enabled — managers ship working")

	root2 := writeManagerTree(t, "m2", `{"$schema":"x","enable":false,"command":["run"]}`)
	ms2, errs := LoadManagers(DetectLayout(root2))
	require.Empty(t, errs)
	assert.False(t, ms2["m2"].EnabledByDefault(), "explicit enable:false is the only config-side off switch")
}

// The two deliberate non-fields fail loudly: `state` is implied and
// `schedule` is superseded by reconcile_interval.
func TestParseManagerRejectsNonFields(t *testing.T) {
	for _, bad := range []string{
		`{"$schema":"x","state":true,"command":["run"]}`,
		`{"$schema":"x","schedule":"5m","command":["run"]}`,
		`{"$schema":"x","bogus":1,"command":["run"]}`,
	} {
		root := writeManagerTree(t, "m1", bad)
		_, errs := LoadManagers(DetectLayout(root))
		require.Len(t, errs, 1, bad)
		var mle ManagerLoadError
		require.ErrorAs(t, errs[0], &mle)
		assert.Equal(t, "m1", mle.ManagerID)
	}
}

func TestParseManagerValidation(t *testing.T) {
	cases := map[string]string{
		"bad interval":  `{"$schema":"x","reconcile_interval":"nope","command":["run"]}`,
		"zero interval": `{"$schema":"x","reconcile_interval":"0s","command":["run"]}`,
		"no schema":     `{"description":"d","command":["run"]}`,
		"two auth":      `{"$schema":"x","secret":"a","api_key":"b","command":["run"]}`,
	}
	for name, doc := range cases {
		root := writeManagerTree(t, "m1", doc)
		_, errs := LoadManagers(DetectLayout(root))
		assert.Len(t, errs, 1, name)
	}
}

func TestParseManagerRequiresDockerfile(t *testing.T) {
	root := writeManagerTree(t, "m1", minimalManager)
	require.NoError(t, os.Remove(filepath.Join(root, "src", "managers", "m1", "Dockerfile")))
	_, errs := LoadManagers(DetectLayout(root))
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "Dockerfile")
}

// Legacy trees have no managers — LoadManagers never scans them, so a
// pre-manager tree (or a stray src/managers under a legacy root) can never
// load one. Zero managers is NOT an error (unlike ZeroHooksError).
func TestLoadManagersLegacyAndAbsent(t *testing.T) {
	legacy := t.TempDir() // no src/hooks — legacy layout
	ms, errs := LoadManagers(DetectLayout(legacy))
	assert.Empty(t, ms)
	assert.Empty(t, errs)

	sdk := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(sdk, "src", "hooks"), 0o755))
	ms, errs = LoadManagers(DetectLayout(sdk)) // src layout, no managers dir
	assert.Empty(t, ms)
	assert.Empty(t, errs)
}

// The managers dir may hold non-manager folders (READMEs, assets) — they
// are silently skipped, the hooks-loader convention.
func TestLoadManagersSkipsNonManagerDirs(t *testing.T) {
	root := writeManagerTree(t, "m1", minimalManager)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "managers", "notes"), 0o755))
	ms, errs := LoadManagers(DetectLayout(root))
	require.Empty(t, errs)
	assert.Len(t, ms, 1)
}
