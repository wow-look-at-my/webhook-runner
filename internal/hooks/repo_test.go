package hooks

import (
	"log/slog"
	"os"
	"testing"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestCloneRepo_TokenNotLeakedInError(t *testing.T) {
	token := "ghp_S3CR3TT0K3N_do_not_leak_me"
	dir := t.TempDir()

	// Use a URL that will fail to clone (nonexistent host).
	_, err := CloneRepo(
		"https://git.invalid.example/org/repo.git",
		"",
		dir+"/hooks",
		token,
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)
	require.NotNil(t, err)

	assert.NotContains(t, err.Error(), token)

}

func TestSanitize(t *testing.T) {
	cases := []struct {
		input	string
		token	string
		want	string
	}{
		{"no token here", "secret", "no token here"},
		{"the secret is secret!", "secret", "the [REDACTED] is [REDACTED]!"},
		{"empty token", "", "empty token"},
	}
	for _, tc := range cases {
		got := sanitize(tc.input, tc.token)
		assert.Equal(t, tc.want, got)

	}
}

func TestWriteAskpass(t *testing.T) {
	path, err := writeAskpass("test-token-123")
	require.Nil(t, err)

	defer os.Remove(path)

	info, err := os.Stat(path)
	require.Nil(t, err)

	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	content, err := os.ReadFile(path)
	require.Nil(t, err)

	assert.Contains(t, string(content), "x-access-token")

	assert.Contains(t, string(content), "test-token-123")

}
