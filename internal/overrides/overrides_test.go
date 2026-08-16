package overrides

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenMissingFileIsZeroState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)
	assert.False(t, s.HookDisabled("anything", true))
	assert.Empty(t, s.DisabledHooks())
	assert.Empty(t, s.ConcurrencyLimits())
}

func TestOpenCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "overrides.json")
	_, err := Open(path)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Dir(path))
	require.NoError(t, err)
}

// Restart survival: everything written by one Store is read back by a
// fresh Open at the same path.
func TestRoundTripSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	s, err := Open(path)
	require.NoError(t, err)

	changed, err := s.SetHookDisabled("runaway", true)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = s.SetConcurrencyLimit("model-gateway", 1)
	require.NoError(t, err)
	require.True(t, changed)

	// Simulated restart.
	s2, err := Open(path)
	require.NoError(t, err)
	assert.True(t, s2.HookDisabled("runaway", true))
	assert.Equal(t, []string{"runaway"}, s2.DisabledHooks())
	n, ok := s2.ConcurrencyLimit("model-gateway")
	require.True(t, ok)
	assert.Equal(t, 1, n)
}

func TestSetHookDisabledIdempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)

	changed, err := s.SetHookDisabled("h", true)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s.SetHookDisabled("h", true)
	require.NoError(t, err)
	assert.False(t, changed, "disabling an already-disabled hook is not a flip")

	changed, err = s.SetHookDisabled("h", false)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s.SetHookDisabled("h", false)
	require.NoError(t, err)
	assert.False(t, changed, "enabling an already-enabled hook is not a flip")
}

// Upgrade compatibility: a pre-tri-state overrides.json carries only the
// disabled_hooks set — each entry must read back as an EXPLICIT disable
// override, so an operator's persisted kill switch survives the upgrade.
func TestOpenLegacyDisabledSetReadsAsExplicitDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"disabled_hooks":["old"],"concurrency_limits":{"g":2}}`), 0o644))

	s, err := Open(path)
	require.NoError(t, err)
	enabled, ok := s.HookOverride("old")
	require.True(t, ok, "a legacy disabled entry must be an explicit override")
	assert.False(t, enabled)
	assert.True(t, s.HookDisabled("old", true), "the override must win over a default-enabled hook")
	assert.Equal(t, []string{"old"}, s.DisabledHooks())
	n, ok := s.ConcurrencyLimit("g")
	require.True(t, ok)
	assert.Equal(t, 2, n)
}

// The tri-state contract: no override means the hook.json `enable` default
// decides; an explicit override wins over the default in BOTH directions
// and survives a reopen. Downgrade compatibility: the persisted file still
// carries disabled_hooks (the explicit-disable subset) for old readers.
func TestHookOverrideTriState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	s, err := Open(path)
	require.NoError(t, err)

	// No override: the caller-supplied default decides.
	_, ok := s.HookOverride("h")
	assert.False(t, ok)
	assert.False(t, s.HookDisabled("h", true), "default-enabled + no override = enabled")
	assert.True(t, s.HookDisabled("h", false), "default-disabled + no override = disabled")

	// Explicit ENABLE beats a default-disabled hook (the enable:false case)
	// — and creating it counts as a change even though nothing was stored
	// before, because the stored state machine moved.
	changed, err := s.SetHookDisabled("h", false)
	require.NoError(t, err)
	assert.True(t, changed)
	enabled, ok := s.HookOverride("h")
	require.True(t, ok)
	assert.True(t, enabled)
	assert.False(t, s.HookDisabled("h", false), "explicit enable must beat the enable:false default")
	assert.Empty(t, s.DisabledHooks(), "an explicit enable is not a disable")

	// Explicit DISABLE beats a default-enabled hook.
	changed, err = s.SetHookDisabled("h", true)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, s.HookDisabled("h", true))

	// Both directions survive a reopen, and the legacy field is written for
	// binary downgrades.
	s2, err := Open(path)
	require.NoError(t, err)
	assert.True(t, s2.HookDisabled("h", true))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"hook_enable"`)
	assert.Contains(t, string(data), `"disabled_hooks"`)

	// An explicit-enable-only store reopens as explicit enable too.
	_, err = s.SetHookDisabled("h", false)
	require.NoError(t, err)
	s3, err := Open(path)
	require.NoError(t, err)
	enabled, ok = s3.HookOverride("h")
	require.True(t, ok)
	assert.True(t, enabled)
	overrides := s3.HookOverrides()
	assert.Len(t, overrides, 1, "the enable is the only override left")
	assert.True(t, overrides["h"])
}

func TestSetConcurrencyLimitValidatesAndIsIdempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "overrides.json"))
	require.NoError(t, err)

	_, err = s.SetConcurrencyLimit("g", 0)
	require.Error(t, err, "a 0 limit would deadlock queued runs")
	_, err = s.SetConcurrencyLimit("g", -3)
	require.Error(t, err)

	changed, err := s.SetConcurrencyLimit("g", 2)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s.SetConcurrencyLimit("g", 2)
	require.NoError(t, err)
	assert.False(t, changed, "re-setting the same limit is not a change")
	changed, err = s.SetConcurrencyLimit("g", 5)
	require.NoError(t, err)
	assert.True(t, changed)

	changed, err = s.ClearConcurrencyLimit("g")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s.ClearConcurrencyLimit("g")
	require.NoError(t, err)
	assert.False(t, changed, "clearing an absent override is a no-op")
}

// A persist failure must roll the in-memory mutation back — memory never
// diverges from disk (same rule as internal/kv).
func TestPersistFailureRollsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "overrides.json")
	s, err := Open(path)
	require.NoError(t, err)
	_, err = s.SetHookDisabled("kept", true)
	require.NoError(t, err)

	// Sabotage: replace the parent dir with a regular file so the next
	// temp-file creation fails.
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, []byte("not a dir"), 0o644))

	changed, err := s.SetHookDisabled("h", true)
	require.Error(t, err)
	assert.False(t, changed)
	assert.False(t, s.HookDisabled("h", true), "failed disable must roll back")

	changed, err = s.SetHookDisabled("kept", false)
	require.Error(t, err)
	assert.False(t, changed)
	assert.True(t, s.HookDisabled("kept", true), "failed enable must roll back")

	changed, err = s.SetConcurrencyLimit("g", 2)
	require.Error(t, err)
	assert.False(t, changed)
	_, ok := s.ConcurrencyLimit("g")
	assert.False(t, ok, "failed limit override must roll back")
}

// A corrupt overrides file fails Open: booting with the operator's kill
// switches silently dropped is the one thing this store must never do.
func TestOpenCorruptFileFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err := Open(path)
	require.Error(t, err)
}

// Nil-store reads are safe no-ops (like events.Recorder); writes error.
// With no store there are no overrides, so the effective state is exactly
// the caller's default.
func TestNilStore(t *testing.T) {
	var s *Store
	assert.False(t, s.HookDisabled("h", true))
	assert.True(t, s.HookDisabled("h", false), "a nil store leaves the enable:false default in force")
	_, ok := s.HookOverride("h")
	assert.False(t, ok)
	assert.Nil(t, s.HookOverrides())
	assert.Nil(t, s.DisabledHooks())
	assert.Nil(t, s.ConcurrencyLimits())
	_, ok = s.ConcurrencyLimit("g")
	assert.False(t, ok)

	_, err := s.SetHookDisabled("h", true)
	require.Error(t, err)
	_, err = s.SetConcurrencyLimit("g", 1)
	require.Error(t, err)
	_, err = s.ClearConcurrencyLimit("g")
	require.Error(t, err)
}

// The global run cap override: validated, idempotent, restart-surviving,
// rollback-on-persist-failure — the same contract as group limits.
func TestGlobalRunLimitOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	s, err := Open(path)
	require.NoError(t, err)

	_, ok := s.GlobalRunLimit()
	assert.False(t, ok, "no override in the zero state")

	_, err = s.SetGlobalRunLimit(0)
	require.Error(t, err, "a 0 cap is rejected — it would block every run")

	changed, err := s.SetGlobalRunLimit(96)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s.SetGlobalRunLimit(96)
	require.NoError(t, err)
	assert.False(t, changed, "same value again is idempotent")

	// Simulated restart: the override is read back from disk.
	s2, err := Open(path)
	require.NoError(t, err)
	n, ok := s2.GlobalRunLimit()
	require.True(t, ok, "the global cap override must survive a restart")
	assert.Equal(t, 96, n)

	changed, err = s2.ClearGlobalRunLimit()
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = s2.ClearGlobalRunLimit()
	require.NoError(t, err)
	assert.False(t, changed, "clearing an absent override is idempotent")

	s3, err := Open(path)
	require.NoError(t, err)
	_, ok = s3.GlobalRunLimit()
	assert.False(t, ok, "the cleared override stays cleared across a restart")
}

func TestGlobalRunLimitPersistFailureRollsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "overrides.json")
	s, err := Open(path)
	require.NoError(t, err)
	_, err = s.SetGlobalRunLimit(10)
	require.NoError(t, err)

	// Sabotage: replace the parent dir with a regular file so the next
	// temp-file creation fails.
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, []byte("not a dir"), 0o644))

	changed, err := s.SetGlobalRunLimit(20)
	require.Error(t, err)
	assert.False(t, changed)
	n, ok := s.GlobalRunLimit()
	require.True(t, ok)
	assert.Equal(t, 10, n, "a failed set must roll back to the previous value")

	changed, err = s.ClearGlobalRunLimit()
	require.Error(t, err)
	assert.False(t, changed)
	n, ok = s.GlobalRunLimit()
	require.True(t, ok)
	assert.Equal(t, 10, n, "a failed clear must roll back")
}

func TestNilStoreGlobalRunLimit(t *testing.T) {
	var s *Store
	_, ok := s.GlobalRunLimit()
	assert.False(t, ok)
	_, err := s.SetGlobalRunLimit(1)
	require.Error(t, err)
	_, err = s.ClearGlobalRunLimit()
	require.Error(t, err)
}
