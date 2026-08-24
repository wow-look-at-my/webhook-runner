package reloadgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// fakeRepo scripts the GitRepo surface: tip/commits describe what a fetch
// would see, head is the working tree, known gates FetchSHA. srcAt scripts
// TreeHasDir (absent sha = no src marker); resolve scripts ResolveRef
// (absent ref falls back to "any sha the fake knows about resolves to
// itself").
type fakeRepo struct {
	head    string
	tip     string
	commits []string // newest first, as of the next fetch
	known   set.Set[string]
	srcAt   set.Set[string]
	resolve map[string]string

	headErr, fetchErr, recentErr, resetErr error

	headCalls, fetchBranchCalls, recentCalls, resetCalls, fetchSHACalls, resolveCalls int
	resets                                                                            []string
}

func (f *fakeRepo) Head() (string, error) {
	f.headCalls++
	return f.head, f.headErr
}

func (f *fakeRepo) FetchBranch(depth int) (string, error) {
	f.fetchBranchCalls++
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	return f.tip, nil
}

func (f *fakeRepo) FetchBranchContext(_ context.Context, depth int) (string, error) {
	return f.FetchBranch(depth)
}

func (f *fakeRepo) RecentCommits(max int) ([]string, error) {
	f.recentCalls++
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	return f.commits, nil
}

func (f *fakeRepo) ResetTo(sha string) error {
	f.resetCalls++
	if f.resetErr != nil {
		return f.resetErr
	}
	f.resets = append(f.resets, sha)
	f.head = sha
	return nil
}

func (f *fakeRepo) FetchSHA(sha string, depth int) error {
	f.fetchSHACalls++
	if !f.known.Contains(sha) {
		return fmt.Errorf("sha %s not on origin", sha)
	}
	return nil
}

func (f *fakeRepo) TreeHasDir(sha, path string) bool {
	return f.srcAt.Contains(sha)
}

func (f *fakeRepo) ResolveRef(ref string) (string, error) {
	f.resolveCalls++
	if sha, ok := f.resolve[ref]; ok {
		return sha, nil
	}
	// Any sha the fake knows about resolves to itself.
	if f.known.Contains(ref) || ref == f.tip || ref == f.head {
		return ref, nil
	}
	for _, c := range f.commits {
		if c == ref {
			return ref, nil
		}
	}
	return "", fmt.Errorf("unknown ref %q", ref)
}

// gitOps is the total git-call count, for the "zero git ops" assertions.
func (f *fakeRepo) gitOps() int {
	return f.headCalls + f.fetchBranchCalls + f.recentCalls + f.resetCalls + f.fetchSHACalls
}

type gateFixture struct {
	gate      *Gate
	repo      *fakeRepo
	rec       *events.Recorder
	agg       *attention.Aggregator
	applies   *int
	statePath string
}

func newFixture(t *testing.T, repo *fakeRepo) *gateFixture {
	t.Helper()
	return newFixtureAt(t, repo, filepath.Join(t.TempDir(), "reload-gate.json"))
}

func newFixtureAt(t *testing.T, repo *fakeRepo, statePath string) *gateFixture {
	t.Helper()
	rec := events.NewRecorder(100)
	agg := attention.New()
	applies := 0
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: statePath,
		Apply:     func() error { applies++; return nil },
		Events:    rec,
		Attention: agg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return &gateFixture{gate: g, repo: repo, rec: rec, agg: agg, applies: &applies, statePath: statePath}
}

func seedState(t *testing.T, path string, st gateState) {
	t.Helper()
	data, err := json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func readState(t *testing.T, path string) gateState {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var st gateState
	require.NoError(t, json.Unmarshal(data, &st))
	return st
}

func pushBody(t *testing.T, ref, after string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"ref":        ref,
		"after":      after,
		"repository": map[string]any{"default_branch": "master"},
	})
	require.NoError(t, err)
	return b
}

func statusBody(t *testing.T, sha, state, context string, branches ...string) []byte {
	t.Helper()
	bs := make([]map[string]string, 0, len(branches))
	for _, b := range branches {
		bs = append(bs, map[string]string{"name": b})
	}
	b, err := json.Marshal(map[string]any{
		"sha":        sha,
		"state":      state,
		"context":    context,
		"branches":   bs,
		"repository": map[string]any{"default_branch": "master"},
	})
	require.NoError(t, err)
	return b
}

func eventKinds(rec *events.Recorder) []string {
	evs := rec.List(0)
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func attentionKeys(agg *attention.Aggregator) map[string]string {
	out := map[string]string{}
	for _, e := range agg.Snapshot() {
		if e.Source == attention.SourceReload {
			out[e.Key] = e.Message
		}
	}
	return out
}

// verified serving A at head A: the common starting point for event tests.
func servingFixture(t *testing.T, repo *fakeRepo, serving string) *gateFixture {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: serving, Verified: true})
	repo.head = serving
	f := newFixtureAt(t, repo, statePath)
	f.gate.Startup()
	require.Zero(t, *f.applies, "startup must not apply")
	return f
}

func TestPushHeld(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	assert.Equal(t, "held", status)

	assert.Zero(t, *f.applies, "push must never apply")
	assert.Empty(t, repo.resets, "push must never move the tree")
	assert.Contains(t, eventKinds(f.rec), "reload.held")

	st := readState(t, f.statePath)
	assert.Equal(t, "A", st.ServingSHA)
	assert.True(t, st.Verified)
	assert.Equal(t, "B", st.PendingSHA)
	assert.Equal(t, "pending", st.PendingState)

	keys := attentionKeys(f.agg)
	require.Contains(t, keys, attention.KeyReloadHeld)
	assert.Contains(t, keys[attention.KeyReloadHeld], "awaiting all-builds")
}

func TestPushUpToDateClearsPending(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	// Green switches to B; a later push webhook for B finds us up to date.
	_, err = f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	status, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	assert.Equal(t, "up-to-date", status)
	assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadHeld)
}

func TestGreenSwitches(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)

	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)

	assert.Equal(t, []string{"B"}, repo.resets, "reset to the evented sha")
	assert.Equal(t, 1, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.switched")

	st := readState(t, f.statePath)
	assert.Equal(t, "B", st.ServingSHA)
	assert.True(t, st.Verified)
	assert.Empty(t, st.PendingSHA)

	assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadHeld, "held entry resolved on switch")
}

func TestRedHeld(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	gitOps := repo.gitOps()

	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "failure", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "held", status)

	assert.Zero(t, *f.applies)
	assert.Empty(t, repo.resets)
	assert.Equal(t, gitOps, repo.gitOps(), "a red status needs no git ops")
	assert.Contains(t, eventKinds(f.rec), "reload.held_red")

	st := readState(t, f.statePath)
	assert.Equal(t, "A", st.ServingSHA)
	assert.Equal(t, "B", st.PendingSHA)
	assert.Equal(t, "failure", st.PendingState)

	keys := attentionKeys(f.agg)
	require.Contains(t, keys, attention.KeyReloadHeld)
	assert.Contains(t, keys[attention.KeyReloadHeld], "failure", "held entry message tracks the red state")
}

func TestRedForUnseenNewerCommitStillRecords(t *testing.T) {
	// No pending (the push delivery was missed); a red for a foreign sha
	// still records the hold so the operator sees it.
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "error", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "held", status)
	st := readState(t, f.statePath)
	assert.Equal(t, "B", st.PendingSHA)
	assert.Equal(t, "error", st.PendingState)
}

func TestOutOfOrderGreenIgnoredStale(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	repo.tip, repo.commits = "C", []string{"C", "B", "A"}
	_, err = f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "C"))
	require.NoError(t, err)

	status, err := f.gate.HandleEvent("status", statusBody(t, "C", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)
	assert.Equal(t, 1, *f.applies)

	// The straggler green for B arrives after we already run C.
	status, err = f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "ignored-stale", status)
	assert.Equal(t, 1, *f.applies, "no second apply")
	assert.Equal(t, []string{"C"}, repo.resets, "serving stays C")
	assert.Contains(t, eventKinds(f.rec), "reload.ignored_stale")
	assert.Equal(t, "C", readState(t, f.statePath).ServingSHA)
}
