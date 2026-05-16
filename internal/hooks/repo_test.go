package hooks

import (
	"log/slog"
	"os"
	"strings"
	"testing"
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
	if err == nil {
		t.Fatal("expected clone to fail against invalid host")
	}

	if strings.Contains(err.Error(), token) {
		t.Errorf("error message contains token:\n%s", err.Error())
	}
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
		if got != tc.want {
			t.Errorf("sanitize(%q, %q) = %q, want %q", tc.input, tc.token, got, tc.want)
		}
	}
}

func TestWriteAskpass(t *testing.T) {
	path, err := writeAskpass("test-token-123")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("askpass script permissions = %o, want 700", info.Mode().Perm())
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "x-access-token") {
		t.Error("askpass script missing username response")
	}
	if !strings.Contains(string(content), "test-token-123") {
		t.Error("askpass script missing token")
	}
}
