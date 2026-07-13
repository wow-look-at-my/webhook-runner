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
	assert.False(t, s.HookDisabled("anything"))
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
	assert.True(t, s2.HookDisabled("runaway"))
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
	assert.False(t, s.HookDisabled("h"), "failed disable must roll back")

	changed, err = s.SetHookDisabled("kept", false)
	require.Error(t, err)
	assert.False(t, changed)
	assert.True(t, s.HookDisabled("kept"), "failed enable must roll back")

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
func TestNilStore(t *testing.T) {
	var s *Store
	assert.False(t, s.HookDisabled("h"))
	assert.Nil(t, s.DisabledHooks())
	assert.Nil(t, s.ConcurrencyLimits())
	_, ok := s.ConcurrencyLimit("g")
	assert.False(t, ok)

	_, err := s.SetHookDisabled("h", true)
	require.Error(t, err)
	_, err = s.SetConcurrencyLimit("g", 1)
	require.Error(t, err)
	_, err = s.ClearConcurrencyLimit("g")
	require.Error(t, err)
}
