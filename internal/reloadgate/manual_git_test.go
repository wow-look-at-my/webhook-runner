package reloadgate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// TestManualSwitchAgainstRealRepo proves the whole manual-pick surface over
// an actual git clone: ResolveRef (full sha, short sha, branch name),
// TreeHasDir's src/hooks probe, CommitInfo enrichment, and a rollback to an
// OLDER commit — with the CI reader erroring, i.e. the wedged-gate shape the
// override exists for.
func TestManualSwitchAgainstRealRepo(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "origin.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=master", bare)
	gitRun(t, bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	gitRun(t, "", "init", "--initial-branch=master", work)
	commit := func(msg string) string {
		require.NoError(t, os.WriteFile(filepath.Join(work, "file.txt"), []byte(msg+"\n"), 0o644))
		gitRun(t, work, "add", "-A")
		gitRun(t, work, "commit", "-m", msg)
		gitRun(t, work, "push", bare, "master:master")
		return gitRun(t, work, "rev-parse", "HEAD")
	}

	// c1 carries the src layout; c2 removes it (the broken-tree shape).
	hookDir := filepath.Join(work, "src", "hooks", "hello")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "hook.json"), []byte(`{}`), 0o644))
	c1 := commit("c1 with src layout")
	require.NoError(t, os.RemoveAll(filepath.Join(work, "src")))
	c2 := commit("c2 drops src")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(base, "clone")
	repo, err := hooks.OpenRepo("file://"+bare, "master", dir, "", logger)
	require.NoError(t, err)

	// The new plumbing, straight against the (shallow, tip-only) clone.
	subject, date, err := repo.CommitInfo(c2)
	require.NoError(t, err)
	assert.Equal(t, "c2 drops src", subject)
	assert.False(t, date.IsZero())
	assert.False(t, repo.TreeHasDir(c2, SrcMarkerDir))

	// A full sha the shallow clone does not hold yet: ResolveRef fetches it
	// by sha from origin, after which its objects are inspectable locally.
	sha, err := repo.ResolveRef(c1)
	require.NoError(t, err)
	assert.Equal(t, c1, sha)
	subject, _, err = repo.CommitInfo(c1)
	require.NoError(t, err)
	assert.Equal(t, "c1 with src layout", subject)
	assert.True(t, repo.TreeHasDir(c1, SrcMarkerDir))
	assert.False(t, repo.TreeHasDir(c1, "no/such/dir"))

	// Abbreviated shas resolve against LOCAL history only; deepen first
	// (exactly what ManualSwitch's leading FetchBranch does).
	_, err = repo.FetchBranch(100)
	require.NoError(t, err)
	sha, err = repo.ResolveRef(c1[:10])
	require.NoError(t, err)
	assert.Equal(t, c1, sha)
	sha, err = repo.ResolveRef("master") // branch names resolve to ORIGIN's tip
	require.NoError(t, err)
	assert.Equal(t, c2, sha)
	_, err = repo.ResolveRef("no-such-branch")
	require.Error(t, err)
	_, err = repo.ResolveRef("--upload-pack=/bin/true")
	require.Error(t, err, "flag-shaped refs must be rejected outright")

	rec := events.NewRecorder(100)
	agg := attention.New()
	applies := 0
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: filepath.Join(base, "reload-gate.json"),
		Apply:     func() error { applies++; return nil },
		// The wedged shape: every status read fails (no token / API down).
		Status:    statusFor(nil),
		Events:    rec,
		Attention: agg,
		Logger:    logger,
	})
	require.NoError(t, err)
	g.Startup() // fresh clone: serving c2 (the tip), unverified

	// Rolling back to the OLDER c1 with CI unreadable: refused without the
	// override (unknown counts as not green)...
	out, err := g.ManualSwitch(context.Background(), c1, false)
	require.NoError(t, err)
	assert.False(t, out.Switched)
	assert.Equal(t, "unknown", out.CIState)
	assert.True(t, out.HasSrc)
	require.Len(t, out.Reasons, 1)
	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head, "a refusal must not move the tree")

	// ...and switched WITH it, through the Force-style path.
	out, err = g.ManualSwitch(context.Background(), c1, true)
	require.NoError(t, err)
	require.True(t, out.Switched)
	assert.True(t, out.Overridden())
	head, err = repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c1, head)
	content, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "c1 with src layout\n", string(content))
	assert.DirExists(t, filepath.Join(dir, "src", "hooks", "hello"))
	assert.Equal(t, 1, applies)
	assert.Contains(t, eventKinds(rec), "reload.forced")

	// Trying the tip commit (missing src/hooks) without override: refused,
	// naming the broken tree.
	out, err = g.ManualSwitch(context.Background(), c2, false)
	require.NoError(t, err)
	assert.False(t, out.Switched)
	assert.False(t, out.HasSrc)
	assert.Len(t, out.Reasons, 2, "unknown CI and the missing src marker")
}
