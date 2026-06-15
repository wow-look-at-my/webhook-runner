package concurrency

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDefaultsLimitToOne(t *testing.T) {
	cfg, err := Parse([]byte(`{"groups":{"ollama-local":{"description":"local model"}}}`))
	require.NoError(t, err)
	assert.True(t, cfg.Has("ollama-local"))
	assert.Equal(t, 1, cfg.Limit("ollama-local"))
	assert.Equal(t, "local model", cfg.Groups["ollama-local"].Description)
}

func TestParseExplicitLimit(t *testing.T) {
	cfg, err := Parse([]byte(`{"groups":{"g":{"limit":3}}}`))
	require.NoError(t, err)
	assert.Equal(t, 3, cfg.Limit("g"))
}

func TestParseAllowsComments(t *testing.T) {
	cfg, err := Parse([]byte(`{
		// the one group we have so far
		"groups": {
			"ollama-local": { "limit": 1 /* serialize */ }
		}
	}`))
	require.NoError(t, err)
	assert.True(t, cfg.Has("ollama-local"))
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(`{"groups":{"g":{"limit":1,"bogus":true}}}`))
	require.Error(t, err)
}

func TestParseRejectsNegativeLimit(t *testing.T) {
	_, err := Parse([]byte(`{"groups":{"g":{"limit":-1}}}`))
	require.Error(t, err)
}

func TestParseEmptyDocument(t *testing.T) {
	cfg, err := Parse([]byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, cfg.Names())
	assert.False(t, cfg.Has("anything"))
	assert.Equal(t, 0, cfg.Limit("anything"))
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	cfg, err := Load(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, cfg.Names())
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, FileName),
		[]byte(`{"groups":{"ollama-local":{"limit":2}}}`), 0o644))
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"ollama-local"}, cfg.Names())
	assert.Equal(t, 2, cfg.Limit("ollama-local"))
}

func TestLoadInvalidFileErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, FileName), []byte(`{not json`), 0o644))
	_, err := Load(dir)
	require.Error(t, err)
}

func TestCheckRefs(t *testing.T) {
	cfg, err := Parse([]byte(`{"groups":{"ollama-local":{"limit":1}}}`))
	require.NoError(t, err)

	bad := CheckRefs(cfg, map[string]string{
		"a": "ollama-local", // declared — fine
		"b": "",             // unbounded — ignored
		"c": "ghost",        // undeclared — flagged
		"d": "missing",      // undeclared — flagged
	})
	require.Len(t, bad, 2)
	// Sorted by hook ID.
	assert.Equal(t, "c", bad[0].HookID)
	assert.Equal(t, "ghost", bad[0].Group)
	assert.Equal(t, "d", bad[1].HookID)
	assert.Contains(t, bad[0].Error(), "undeclared concurrency group")
}

func TestHasAndLimitNilSafe(t *testing.T) {
	var cfg *Config
	assert.False(t, cfg.Has("x"))
	assert.Equal(t, 0, cfg.Limit("x"))
	assert.Nil(t, cfg.Names())
}
