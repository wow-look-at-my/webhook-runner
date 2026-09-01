package reloadgate

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// The production wiring hands the gate a *hooks.Repo.
var _ GitRepo = (*hooks.Repo)(nil)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// TestGateAgainstRealRepo drives the full held-then-switch flow over an
// actual git clone, proving the GitRepo interface matches what *hooks.Repo
// really does.
func TestGateAgainstRealRepo(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=master", bare)
	gitRun(t, bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	gitRun(t, "", "init", "--initial-branch=master", work)
	commit := func(msg string) string {
		require.NoError(t, os.WriteFile(filepath.Join(work, "file.txt"), []byte(msg+"\n"), 0o644))
		gitRun(t, work, "add", ".")
		gitRun(t, work, "commit", "-m", msg)
		gitRun(t, work, "push", bare, "master:master")
		return gitRun(t, work, "rev-parse", "HEAD")
	}
	commit("c1")
	c2 := commit("c2")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(base, "clone")
	repo, err := hooks.OpenRepo("file://"+bare, "master", dir, "", logger)
	require.NoError(t, err)

	rec := events.NewRecorder(100)
	agg := attention.New()
	applies := 0
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: filepath.Join(base, "reload-gate.json"),
		Apply:     func() error { applies++; return nil },
		Events:    rec,
		Attention: agg,
		Logger:    logger,
	})
	require.NoError(t, err)
	g.Startup()

	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, c2, head, "fresh clone serves the tip, unverified")
	require.Zero(t, applies)

	// The origin moves on; the push only records it.
	c3 := commit("c3")
	status, err := g.HandleEvent("push", pushBody(t, "refs/heads/master", c3))
	require.NoError(t, err)
	assert.Equal(t, "held", status)
	head, err = repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head, "push must not move the tree")
	assert.Zero(t, applies)

	// The green gating status is what switches.
	status, err = g.HandleEvent("status", statusBody(t, c3, "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)
	head, err = repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c3, head)
	content, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "c3\n", string(content))
	assert.Equal(t, 1, applies)
	assert.Empty(t, attentionKeys(agg))
}

// TestGateStartupRestoreAgainstRealRepo proves the persisted last-good sha
// is restored over a real clone that was left at the wrong commit.
func TestGateStartupRestoreAgainstRealRepo(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=master", bare)
	gitRun(t, bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	gitRun(t, "", "init", "--initial-branch=master", work)
	commit := func(msg string) string {
		require.NoError(t, os.WriteFile(filepath.Join(work, "file.txt"), []byte(msg+"\n"), 0o644))
		gitRun(t, work, "add", ".")
		gitRun(t, work, "commit", "-m", msg)
		gitRun(t, work, "push", bare, "master:master")
		return gitRun(t, work, "rev-parse", "HEAD")
	}
	commit("c1")
	c2 := commit("c2")
	commit("c3")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(base, "clone")
	// The clone sits at the tip (c), but the last recorded green was c.
	repo, err := hooks.OpenRepo("file://"+bare, "master", dir, "", logger)
	require.NoError(t, err)

	statePath := filepath.Join(base, "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: c2, Verified: true})

	agg := attention.New()
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: statePath,
		Events:    events.NewRecorder(100),
		Attention: agg,
		Logger:    logger,
	})
	require.NoError(t, err)
	g.Startup()

	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head, "boot restores the persisted last-good commit")
	assert.NotContains(t, attentionKeys(agg), attention.KeyReloadUnverified)
}
