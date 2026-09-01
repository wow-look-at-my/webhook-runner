package hooks

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloneRepo_UsesSSHKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	require.Nil(t, os.WriteFile(keyPath, []byte("fake-key"), 0o600))

	_, err := CloneRepo(
		"git@git.invalid.example:org/repo.git",
		"",
		filepath.Join(dir, "hooks"),
		keyPath,
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "git clone")
}

func TestGitCmd_SetsSSHCommand(t *testing.T) {
	r := &Repo{sshKeyPath: "/tmp/test-key"}
	cmd := r.gitCmd("status")

	found := false
	for _, e := range cmd.Env {
		if e == "GIT_SSH_COMMAND=ssh -i /tmp/test-key -o StrictHostKeyChecking=accept-new" {
			found = true
			break
		}
	}
	assert.True(t, found, "missing GIT_SSH_COMMAND in env")
}

func TestGitCmd_NoKeyNoExtraEnv(t *testing.T) {
	r := &Repo{}
	cmd := r.gitCmd("status")
	assert.Nil(t, cmd.Env)
}

func TestEnsureSSHKey_GeneratesKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")

	path, err := EnsureSSHKey(keyPath, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.Nil(t, err)
	assert.Equal(t, keyPath, path)

	_, err = os.Stat(keyPath)
	require.Nil(t, err)
	_, err = os.Stat(keyPath + ".pub")
	require.Nil(t, err)

	pub, err := os.ReadFile(keyPath + ".pub")
	require.Nil(t, err)
	assert.Contains(t, string(pub), "ssh-ed25519")
}

func TestEnsureSSHKey_ReusesExisting(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")

	// Generate a key .
	_, err := EnsureSSHKey(keyPath, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.Nil(t, err)

	original, err := os.ReadFile(keyPath)
	require.Nil(t, err)

	// Call again — should reuse, not regenerate.
	_, err = EnsureSSHKey(keyPath, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.Nil(t, err)

	after, err := os.ReadFile(keyPath)
	require.Nil(t, err)
	assert.Equal(t, original, after)
}
