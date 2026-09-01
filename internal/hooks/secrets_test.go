package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMockSops drops a script that emits the file's content verbatim (the
// "encrypted" fixtures in these tests are plaintext dotenv) and counts its
// invocations in <dir>/sops-calls, so caching can be asserted.
func writeMockSops(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "sops")
	script := `#!/bin/sh
echo x >> "$(dirname "$0")/sops-calls"
for a; do f="$a"; done
cat "$f"
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func sopsCalls(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "sops-calls"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	n := 0
	for _, b := range data {
		if b == 'x' {
			n++
		}
	}
	return n
}

func secretsHook(t *testing.T, dir, content string) *Hook {
	t.Helper()
	hookDir := filepath.Join(dir, "h")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	if content != "" {
		require.NoError(t, os.WriteFile(filepath.Join(hookDir, SecretsFileName), []byte(content), 0o600))
	}
	return &Hook{ID: "h", SourcePath: filepath.Join(hookDir, "hook.json"), Command: []string{"x"}}
}

func TestSecretsLoaderNoFile(t *testing.T) {
	dir := t.TempDir()
	l := NewSecretsLoader(writeMockSops(t, dir))
	got, err := l.Load(secretsHook(t, dir, ""))
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Equal(t, 0, sopsCalls(t, dir)) // absent file never invokes sops

	// A hook not loaded from disk has no secrets either.
	got, err = l.Load(&Hook{ID: "mem", Command: []string{"x"}})
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSecretsLoaderDecryptsAndCaches(t *testing.T) {
	dir := t.TempDir()
	l := NewSecretsLoader(writeMockSops(t, dir))
	h := secretsHook(t, dir, "# comment\nKEY_ONE=v1\nKEY_TWO=a=b\n\n")

	got, err := l.Load(h)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"KEY_ONE": "v1", "KEY_TWO": "a=b"}, got)

	// Unchanged file: served from cache, no sops invocation.
	_, err = l.Load(h)
	require.NoError(t, err)
	assert.Equal(t, 1, sopsCalls(t, dir))

	// A changed file (different size — mtime granularity is too coarse to rely on in a fast test) is re-decrypted.
	path := filepath.Join(filepath.Dir(h.SourcePath), SecretsFileName)
	require.NoError(t, os.WriteFile(path, []byte("KEY_ONE=v2\n"), 0o600))
	got, err = l.Load(h)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"KEY_ONE": "v2"}, got)
	assert.Equal(t, 2, sopsCalls(t, dir))
}

func TestSecretsLoaderMalformedOutput(t *testing.T) {
	dir := t.TempDir()
	l := NewSecretsLoader(writeMockSops(t, dir))

	_, err := l.Load(secretsHook(t, dir, "not a dotenv line\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not KEY=VALUE")

	dir2 := t.TempDir()
	l2 := NewSecretsLoader(writeMockSops(t, dir2))
	_, err = l2.Load(secretsHook(t, dir2, "1BAD=x\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid variable name")
}

func TestSecretsLoaderSopsFailure(t *testing.T) {
	dir := t.TempDir()
	failing := filepath.Join(dir, "sops")
	require.NoError(t, os.WriteFile(failing, []byte("#!/bin/sh\necho 'no key found' >&2\nexit 1\n"), 0o755))
	l := NewSecretsLoader(failing)

	_, err := l.Load(secretsHook(t, dir, "KEY=v\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no key found")
}

func TestSecretsFirstLookup(t *testing.T) {
	t.Setenv("WHR_SECRETS_TEST_HOST", "from-host")
	lookup := SecretsFirstLookup(map[string]string{"FROM_SECRETS": "s", "WHR_SECRETS_TEST_HOST": "shadowed"})

	v, ok := lookup("FROM_SECRETS")
	assert.True(t, ok)
	assert.Equal(t, "s", v)

	// Secrets win over the host env on conflict.
	v, ok = lookup("WHR_SECRETS_TEST_HOST")
	assert.True(t, ok)
	assert.Equal(t, "shadowed", v)

	// nil map degrades to plain host-env lookup.
	v, ok = SecretsFirstLookup(nil)("WHR_SECRETS_TEST_HOST")
	assert.True(t, ok)
	assert.Equal(t, "from-host", v)

	_, ok = lookup("WHR_SECRETS_TEST_UNSET")
	assert.False(t, ok)
}
