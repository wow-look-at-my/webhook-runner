package hooks

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitRun executes one git command for the fixtures below, failing the test
// on any error. Identity env vars make commits work in a bare CI container.
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

// newTestOrigin creates a bare origin repository with n commits on master
// and returns its file:// URL, the commit shas oldest-first, and a function
// that adds one more commit (returning its sha). The origin allows
// reachable-sha fetches, mirroring GitHub (what FetchSHA relies on).
func newTestOrigin(t *testing.T, n int) (url string, shas []string, addCommit func(msg string) string) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=master", bare)
	gitRun(t, bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	gitRun(t, "", "init", "--initial-branch=master", work)

	addCommit = func(msg string) string {
		require.NoError(t, os.WriteFile(filepath.Join(work, "file.txt"), []byte(msg+"\n"), 0o644))
		gitRun(t, work, "add", ".")
		gitRun(t, work, "commit", "-m", msg)
		gitRun(t, work, "push", bare, "master:master")
		return gitRun(t, work, "rev-parse", "HEAD")
	}
	for i := 0; i < n; i++ {
		shas = append(shas, addCommit(fmt.Sprintf("c%d", i+1)))
	}
	return "file://" + bare, shas, addCommit
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRepoGitOpsRoundTrip(t *testing.T) {
	url, shas, addCommit := newTestOrigin(t, 3)
	c1, c2, c3 := shas[0], shas[1], shas[2]
	dir := filepath.Join(t.TempDir(), "clone")

	r, err := CloneRepo(url, "master", dir, "", discardLogger())
	require.NoError(t, err)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, c3, head)

	tip, err := r.FetchBranch(100)
	require.NoError(t, err)
	assert.Equal(t, c3, tip)

	commits, err := r.RecentCommits(100)
	require.NoError(t, err)
	assert.Equal(t, []string{c3, c2, c1}, commits, "newest first")

	// Reset to an older, fetched commit — the working tree follows.
	require.NoError(t, r.ResetTo(c2))
	head, err = r.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head)
	content, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "c2\n", string(content))

	// Fetch a specific reachable sha (the startup restore path) and reset.
	require.NoError(t, r.FetchSHA(c1, 100))
	require.NoError(t, r.ResetTo(c1))
	head, err = r.Head()
	require.NoError(t, err)
	assert.Equal(t, c1, head)

	// New commits on the origin show up on the next fetch, tree untouched.
	c4 := addCommit("c4")
	tip, err = r.FetchBranch(100)
	require.NoError(t, err)
	assert.Equal(t, c4, tip)
	commits, err = r.RecentCommits(2)
	require.NoError(t, err)
	assert.Equal(t, []string{c4, c3}, commits)
	head, err = r.Head()
	require.NoError(t, err)
	assert.Equal(t, c1, head, "fetch must not move the working tree")
}

func TestRepoRecentCommitsBounded(t *testing.T) {
	url, shas, _ := newTestOrigin(t, 3)
	dir := filepath.Join(t.TempDir(), "clone")
	r, err := CloneRepo(url, "master", dir, "", discardLogger())
	require.NoError(t, err)
	_, err = r.FetchBranch(100)
	require.NoError(t, err)

	commits, err := r.RecentCommits(1)
	require.NoError(t, err)
	assert.Equal(t, []string{shas[2]}, commits)
}

func TestOpenRepoLeavesExistingTreeUntouched(t *testing.T) {
	url, shas, addCommit := newTestOrigin(t, 2)
	dir := filepath.Join(t.TempDir(), "clone")

	r, err := CloneRepo(url, "master", dir, "", discardLogger())
	require.NoError(t, err)
	_, err = r.FetchBranch(100)
	require.NoError(t, err)
	require.NoError(t, r.ResetTo(shas[0]))
	addCommit("c3") // the origin moves on

	// Gate-mode open: the checked-out commit stays exactly where it was.
	r2, err := OpenRepo(url, "master", dir, "", discardLogger())
	require.NoError(t, err)
	head, err := r2.Head()
	require.NoError(t, err)
	assert.Equal(t, shas[0], head)

	// Legacy CloneRepo on the same dir pulls to the remote tip.
	r3, err := CloneRepo(url, "master", dir, "", discardLogger())
	require.NoError(t, err)
	head, err = r3.Head()
	require.NoError(t, err)
	tip, err := r3.FetchBranch(100)
	require.NoError(t, err)
	assert.Equal(t, tip, head)
}

func TestOpenRepoClonesFreshDir(t *testing.T) {
	url, shas, _ := newTestOrigin(t, 2)
	dir := filepath.Join(t.TempDir(), "clone")

	r, err := OpenRepo(url, "master", dir, "", discardLogger())
	require.NoError(t, err)
	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, shas[1], head, "fresh dir clones the branch tip")
}
