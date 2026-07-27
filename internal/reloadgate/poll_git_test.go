package reloadgate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// TestReconcileAgainstRealRepoAndFakeAPI drives the poll fallback end to
// end: a real git clone whose origin moved while "the webhooks were
// missed", an httptest GitHub API serving the combined commit status, and
// the real githubstatus.ContextState as the reader — unreported and red
// hold, green switches the working tree.
func TestReconcileAgainstRealRepoAndFakeAPI(t *testing.T) {
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

	// Fake GitHub combined-status API, scripted per sha.
	var mu sync.Mutex
	states := map[string]string{} // sha -> all-builds state
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasPrefix(r.URL.Path, "/repos/wow-look-at-my/webhooks/commits/"), r.URL.Path)
		assert.True(t, strings.HasSuffix(r.URL.Path, "/status"), r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		sha := parts[len(parts)-2]
		mu.Lock()
		state, ok := states[sha]
		mu.Unlock()
		statuses := []map[string]string{}
		if ok {
			statuses = append(statuses, map[string]string{"context": "all-builds", "state": state})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "statuses": statuses})
	}))
	defer api.Close()
	setState := func(sha, state string) {
		mu.Lock()
		states[sha] = state
		mu.Unlock()
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gh := githubstatus.New("test-token", logger)
	gh.SetAPIURL(api.URL)

	dir := filepath.Join(base, "clone")
	repo, err := hooks.OpenRepo("file://"+bare, "master", dir, "", logger)
	require.NoError(t, err)

	applies := 0
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: filepath.Join(base, "reload-gate.json"),
		Apply:     func() { applies++ },
		Status: func(ctx context.Context, sha string) (string, error) {
			// The cli adapter's exact shape: missing context reads as "no
			// status yet", everything else passes through.
			state, err := gh.ContextState(ctx, "wow-look-at-my/webhooks", sha, "all-builds")
			if errors.Is(err, githubstatus.ErrNoContextStatus) {
				return "", nil
			}
			return state, err
		},
		Events:    events.NewRecorder(100),
		Attention: attention.New(),
		Logger:    logger,
	})
	require.NoError(t, err)
	g.Startup()

	// The origin moves on while every webhook delivery is "missed".
	c3 := commit("c3")

	// No all-builds status posted yet: the poll holds.
	g.Reconcile(context.Background())
	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head, "no status for the context yet — hold")
	assert.Zero(t, applies)

	// Red: still holds.
	setState(c3, "failure")
	g.Reconcile(context.Background())
	head, err = repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c2, head, "red tip — hold")
	assert.Zero(t, applies)

	// Green: the poll switches through the normal ordering-checked path.
	setState(c3, "success")
	g.Reconcile(context.Background())
	head, err = repo.Head()
	require.NoError(t, err)
	assert.Equal(t, c3, head)
	content, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "c3\n", string(content))
	assert.Equal(t, 1, applies)
}
