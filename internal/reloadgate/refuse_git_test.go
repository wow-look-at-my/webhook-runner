// THE TREE-MOVED HALF OF THE FAIL-CLOSED RULE, over a real git clone.
//
// A tree can carry a manifest field the RUNNING binary does not know — the
// hooks repo merged , or the runner's own deploy has not landed yet.
// The load then fails and applies nothing (internal/cli.buildLoadAndApply),
// and the question this file answers is what the gate does about a working
// tree it has already moved.
//
// It rolls it back. The gate is the only component that knows which commit
// was serving a moment ago, so it resets to that commit, re-applies it, and
// keeps the old serving record — the deploy is HELD, loudly, and the fleet
// keeps running the tree it was running. Before this, `apply` could not
// fail: the gate recorded the new sha as serving, called apply, and a
// refused tree became a fleet quietly missing every entity the binary could
// not parse, with a persisted state file insisting all was well.
package reloadgate

import (
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

// refuseRepo builds a real bare origin + clone with commits and
// returns the clone, the commits, and a commit() to add more.
func refuseRepo(t *testing.T) (*hooks.Repo, string, func(string) string, string) {
	t.Helper()
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	commit("c1")
	c2 := commit("c2")
	dir := filepath.Join(base, "clone")
	repo, err := hooks.OpenRepo("file://"+bare, "master", dir, "", logger)
	require.NoError(t, err)
	return repo, c2, commit, base
}

func treeContent(t *testing.T, repo *hooks.Repo, base string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, "clone", "file.txt"))
	require.NoError(t, err)
	return string(b)
}

// A green commit whose tree this binary cannot load: the switch is undone,
// the previously-serving commit is restored AND re-applied, and the gate's
// own record still names the old commit.
func TestRefusedTreeRollsBackToTheServingCommit(t *testing.T) {
	repo, c2, commit, base := refuseRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := events.NewRecorder(100)
	agg := attention.New()

	// The binary accepts the tree it is already serving and REFUSES the next — the shape of a manifest field this build does not know.
	refuse := false
	applied := []string{}
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: filepath.Join(base, "reload-gate.json"),
		Apply: func() error {
			head, herr := repo.Head()
			require.NoError(t, herr)
			applied = append(applied, head)
			if refuse {
				return assertRefused{}
			}
			return nil
		},
		Events:    rec,
		Attention: agg,
		Logger:    logger,
	})
	require.NoError(t, err)
	g.Startup()

	c3 := commit("c3")
	_, err = g.HandleEvent("push", pushBody(t, "refs/heads/master", c3))
	require.NoError(t, err)

	refuse = true
	status, err := g.HandleEvent("status", statusBody(t, c3, "success", "all-builds", "master"))

	require.Error(t, err, "a refused tree is a FAILED switch, not a successful one")
	assert.Equal(t, "refused", status)
	assert.Contains(t, err.Error(), "rolled back")

	// The WORKING TREE is back on the commit that was serving.
	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head, "the tree must be reset to the commit that was serving")
	assert.Equal(t, "c2\n", treeContent(t, repo, base), "and the files on disk with it")

	// It was RE-APPLIED, not merely reset: the fleet has to actually be running the rolled-back tree, not whatever the refused load left.
	require.Len(t, applied, 2, "the refused switch, then the rollback")
	assert.Equal(t, c3, applied[0], "the refused apply saw the new tree")
	assert.Equal(t, c2, applied[1], "the rollback re-applies the restored tree")

	// And the gate's own record never claims the refused commit.
	assert.Equal(t, c2, g.TreeState().ServingSHA)
	assert.Equal(t, 1, countKind(rec, "reload.refused"))
}

// The rollback must survive a restart: a refused switch that quietly
// persisted the new sha would come back after a restart claiming to serve a
// commit it could not load.
func TestARefusedSwitchIsNotPersisted(t *testing.T) {
	repo, c2, commit, base := refuseRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	statePath := filepath.Join(base, "reload-gate.json")

	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: statePath,
		Apply:     func() error { return nil },
		Events:    events.NewRecorder(100),
		Attention: attention.New(),
		Logger:    logger,
	})
	require.NoError(t, err)
	g.Startup()
	c3 := commit("c3")
	_, err = g.HandleEvent("push", pushBody(t, "refs/heads/master", c3))
	require.NoError(t, err)
	_, err = g.HandleEvent("status", statusBody(t, c3, "success", "all-builds", "master"))
	require.NoError(t, err, "the accepting binary switches normally")
	require.Equal(t, c3, g.TreeState().ServingSHA)

	// Now the same repo under a binary that refuses the CURRENT tree's successor — a commit arrives and is refused.
	c4 := commit("c4")
	refuse := false
	g2, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: statePath,
		Apply: func() error {
			if refuse {
				return assertRefused{}
			}
			return nil
		},
		Events:    events.NewRecorder(100),
		Attention: attention.New(),
		Logger:    logger,
	})
	require.NoError(t, err)
	g2.Startup()
	_, err = g2.HandleEvent("push", pushBody(t, "refs/heads/master", c4))
	require.NoError(t, err)
	refuse = true
	_, err = g2.HandleEvent("status", statusBody(t, c4, "success", "all-builds", "master"))
	require.Error(t, err)

	// Reopening from the SAME state file must find c serving, never c.
	g3, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: statePath,
		Apply:     func() error { return nil },
		Events:    events.NewRecorder(100),
		Attention: attention.New(),
		Logger:    logger,
	})
	require.NoError(t, err)
	assert.Equal(t, c3, g3.TreeState().ServingSHA,
		"a refused commit must never be persisted as serving")
	assert.NotEqual(t, c2, g3.TreeState().ServingSHA)
}

// assertRefused stands in for internal/cli's refusedError: the gate only needs "the apply failed", never the concrete type.
type assertRefused struct{}

func (assertRefused) Error() string {
	return "2 entit(y/ies) failed to load, so the tree was REFUSED as a whole"
}
