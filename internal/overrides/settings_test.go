package overrides

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func settingsStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "overrides.json")
	s, err := Open(path)
	require.NoError(t, err)
	return s, path
}

func TestSettingOverrideRoundTripsThroughDisk(t *testing.T) {
	s, path := settingsStore(t)
	changed, err := s.SetSettingOverride("pr-describe", "/ai/reasoning", json.RawMessage(`"off"`))
	require.NoError(t, err)
	assert.True(t, changed)

	// A restart must serve exactly what the operator set — the whole reason
	// this is operational state on disk rather than in memory.
	reopened, err := Open(path)
	require.NoError(t, err)
	got := reopened.SettingsOverrides("pr-describe")
	require.Len(t, got, 1)
	assert.JSONEq(t, `"off"`, string(got["/ai/reasoning"]))
}

func TestSettingOverrideIsIdempotent(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)
	changed, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)
	assert.False(t, changed, "storing the same value again writes nothing")

	changed, err = s.SetSettingOverride("h", "/a", json.RawMessage(`2`))
	require.NoError(t, err)
	assert.True(t, changed)
}

func TestSettingOverridesAreSparsePerField(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)
	_, err = s.SetSettingOverride("h", "/b/c", json.RawMessage(`"x"`))
	require.NoError(t, err)

	got := s.SettingsOverrides("h")
	assert.Len(t, got, 2, "each pinned field is its own entry — never a whole-document snapshot")
}

func TestClearSettingOverrideRevertsOneField(t *testing.T) {
	s, path := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)
	_, err = s.SetSettingOverride("h", "/b", json.RawMessage(`2`))
	require.NoError(t, err)

	changed, err := s.ClearSettingOverride("h", "/a")
	require.NoError(t, err)
	assert.True(t, changed)
	got := s.SettingsOverrides("h")
	require.Len(t, got, 1)
	assert.Contains(t, got, "/b")

	changed, err = s.ClearSettingOverride("h", "/a")
	require.NoError(t, err)
	assert.False(t, changed, "clearing what is not there is a no-op, not an error")

	reopened, err := Open(path)
	require.NoError(t, err)
	assert.Len(t, reopened.SettingsOverrides("h"), 1)
}

func TestClearSettingOverridesDropsTheWholeEntity(t *testing.T) {
	s, path := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)
	_, err = s.SetSettingOverride("other", "/a", json.RawMessage(`1`))
	require.NoError(t, err)

	changed, err := s.ClearSettingOverrides("h")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Nil(t, s.SettingsOverrides("h"))
	assert.Len(t, s.SettingsOverrides("other"), 1, "another entity's overrides are untouched")

	reopened, err := Open(path)
	require.NoError(t, err)
	assert.Nil(t, reopened.SettingsOverrides("h"))

	changed, err = s.ClearSettingOverrides("h")
	require.NoError(t, err)
	assert.False(t, changed)
}

// The empty pointer would pin the WHOLE document, which defeats sparseness:
// every later manifest edit would be silently ignored.
func TestSettingOverrideRefusesTheWholeDocumentPointer(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("h", "", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whole document")
}

func TestSettingOverrideRefusesAMalformedPointer(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("h", "ai/model", json.RawMessage(`"m"`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON Pointer")
}

func TestSettingOverrideRefusesInvalidJSON(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`{not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
	assert.Nil(t, s.SettingsOverrides("h"), "a refused write leaves nothing behind")
}

func TestSettingOverrideRefusesAnEmptyEntityID(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("", "/a", json.RawMessage(`1`))
	require.Error(t, err)
}

// A persist failure must roll memory back: memory that disagrees with disk
// is how an operator sees a value the next restart silently drops.
func TestSettingOverridePersistFailureRollsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(filepath.Join(dir, "overrides.json"))
	require.NoError(t, err)
	_, err = s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)

	// Sabotage the same way the sibling rollback tests do: replace the
	// parent dir with a regular file so the temp-file creation fails.
	// (Permissions would not do it — these tests run as root.)
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, []byte("not a dir"), 0o644))

	_, err = s.SetSettingOverride("h", "/b", json.RawMessage(`2`))
	require.Error(t, err)
	got := s.SettingsOverrides("h")
	assert.Len(t, got, 1, "the failed write is not in memory either")
	assert.Contains(t, got, "/a")

	_, err = s.ClearSettingOverride("h", "/a")
	require.Error(t, err)
	assert.Len(t, s.SettingsOverrides("h"), 1, "a failed clear leaves the override in place")
}

// Returned maps are copies, bytes included: a caller mutating what it got
// back must not reach the store's state behind the mutex.
func TestSettingOverridesReturnCopies(t *testing.T) {
	s, _ := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`"v"`))
	require.NoError(t, err)

	got := s.SettingsOverrides("h")
	got["/a"][1] = 'X'
	got["/injected"] = json.RawMessage(`1`)

	fresh := s.SettingsOverrides("h")
	assert.JSONEq(t, `"v"`, string(fresh["/a"]))
	assert.NotContains(t, fresh, "/injected")

	all := s.AllSettingsOverrides()
	all["h"]["/a"] = json.RawMessage(`"mutated"`)
	assert.JSONEq(t, `"v"`, string(s.SettingsOverrides("h")["/a"]))
}

// A nil store is valid for reads (servers built without one), and its
// writers return errors rather than pretending to have stored something.
func TestNilStoreSettingsBehaviour(t *testing.T) {
	var s *Store
	assert.Nil(t, s.SettingsOverrides("h"))
	assert.Nil(t, s.AllSettingsOverrides())
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.Error(t, err)
	_, err = s.ClearSettingOverride("h", "/a")
	require.Error(t, err)
	_, err = s.ClearSettingOverrides("h")
	require.Error(t, err)
}

// Settings overrides share the file with the kill switches, so writing one
// must never disturb the other.
func TestSettingsOverridesCoexistWithTheOtherOverrides(t *testing.T) {
	s, path := settingsStore(t)
	_, err := s.SetHookDisabled("h", true)
	require.NoError(t, err)
	_, err = s.SetConcurrencyLimit("g", 4)
	require.NoError(t, err)
	_, err = s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)

	reopened, err := Open(path)
	require.NoError(t, err)
	assert.True(t, reopened.HookDisabled("h", true))
	n, ok := reopened.ConcurrencyLimit("g")
	assert.True(t, ok)
	assert.Equal(t, 4, n)
	assert.Len(t, reopened.SettingsOverrides("h"), 1)
}

// An older binary ignores the unknown field and serves the manifest values.
// That is the correct degrade, and it is worth pinning: the manifest is
// always a valid document, a half-understood override might not be.
func TestUnknownFieldsSurviveAReadByAnOlderShape(t *testing.T) {
	s, path := settingsStore(t)
	_, err := s.SetSettingOverride("h", "/a", json.RawMessage(`1`))
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var legacy struct {
		DisabledHooks []string `json:"disabled_hooks"`
	}
	require.NoError(t, json.Unmarshal(raw, &legacy), "the file still parses under the old shape")
}
