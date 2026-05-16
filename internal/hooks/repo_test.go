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
		input string
		token string
		want  string
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

func TestGitCmd_SetsAuthEnv(t *testing.T) {
	r := &Repo{token: "test-token"}
	cmd := r.gitCmd("status")

	found := map[string]bool{}
	for _, e := range cmd.Env {
		switch {
		case e == "GIT_CONFIG_COUNT=1":
			found["count"] = true
		case e == "GIT_CONFIG_KEY_0=http.extraHeader":
			found["key"] = true
		case e == "GIT_CONFIG_VALUE_0=Authorization: Bearer test-token":
			found["value"] = true
		case e == "GIT_TERMINAL_PROMPT=0":
			found["prompt"] = true
		}
	}
	assert.True(t, found["count"], "missing GIT_CONFIG_COUNT")
	assert.True(t, found["key"], "missing GIT_CONFIG_KEY_0")
	assert.True(t, found["value"], "missing GIT_CONFIG_VALUE_0")
	assert.True(t, found["prompt"], "missing GIT_TERMINAL_PROMPT")
}

func TestGitCmd_NoTokenNoExtraEnv(t *testing.T) {
	r := &Repo{}
	cmd := r.gitCmd("status")
	assert.Nil(t, cmd.Env)
}
