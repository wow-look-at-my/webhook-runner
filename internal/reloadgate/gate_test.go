package reloadgate

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// fakeRepo scripts the GitRepo surface: tip/commits describe what a fetch
// would see, head is the working tree, known gates FetchSHA.
type fakeRepo struct {
	head    string
	tip     string
	commits []string // newest first, as of the next fetch
	known   map[string]bool

	headErr, fetchErr, recentErr, resetErr error

	headCalls, fetchBranchCalls, recentCalls, resetCalls, fetchSHACalls int
	resets                                                              []string
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
	if !f.known[sha] {
		return fmt.Errorf("sha %s not on origin", sha)
	}
	return nil
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
		Apply:     func() { applies++ },
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

func TestGreenNotInHistoryIgnoredStale(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("status", statusBody(t, "Z", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "ignored-stale", status)
	assert.Empty(t, repo.resets)
	assert.Zero(t, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.ignored_stale")
}

func TestGreenAtBranchesCapBypassesPrefilter(t *testing.T) {
	// GitHub caps the status payload's branches array at 10; with the sha
	// on more branches than that the tracked one can be squeezed out. A
	// green at exactly the cap must skip the prefilter and reach the
	// ordering rule, which vouches for the sha via the fresh fetch.
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	branches := make([]string, statusBranchesCap)
	for i := range branches {
		branches[i] = fmt.Sprintf("claude/parked-%d", i)
	}
	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", branches...))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)
	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	st := readState(t, f.statePath)
	assert.Equal(t, "B", st.ServingSHA)
	assert.True(t, st.Verified)
}

func TestGreenSwitchesWhenServingFellOutOfWindow(t *testing.T) {
	// Enough commits landed while the runner was held that the serving sha
	// fell out of the fetchDepth window: RecentCommits no longer lists it.
	// A green for the current tip must still switch — an absent serving
	// position can never mean "older than the sha".
	repo := &fakeRepo{tip: "T", commits: []string{"T", "S", "R"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("status", statusBody(t, "T", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)
	assert.Equal(t, []string{"T"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	st := readState(t, f.statePath)
	assert.Equal(t, "T", st.ServingSHA)
	assert.True(t, st.Verified)
}

func TestRedeliveredGreenAppliesOnce(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)

	status, err = f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "already-serving", status)
	assert.Equal(t, 1, *f.applies, "one apply total across the redelivery")
	assert.Equal(t, []string{"B"}, repo.resets)
}

func TestIntermediateGreenSwitchesAndKeepsPending(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	repo.tip, repo.commits = "C", []string{"C", "B", "A"}
	_, err = f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "C"))
	require.NoError(t, err)

	// Green for the INTERMEDIATE commit B: switch to B (the evented sha,
	// not the tip), keep C pending and the held entry alive.
	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)
	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Equal(t, 1, *f.applies)

	st := readState(t, f.statePath)
	assert.Equal(t, "B", st.ServingSHA)
	assert.Equal(t, "C", st.PendingSHA, "the newer pending commit survives the switch")
	keys := attentionKeys(f.agg)
	require.Contains(t, keys, attention.KeyReloadHeld)
	assert.Contains(t, keys[attention.KeyReloadHeld], "still awaiting")

	// Green for C completes the catch-up and resolves the hold.
	status, err = f.gate.HandleEvent("status", statusBody(t, "C", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "reloaded", status)
	assert.Equal(t, []string{"B", "C"}, repo.resets)
	assert.Equal(t, 2, *f.applies)
	assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadHeld)
	assert.Empty(t, readState(t, f.statePath).PendingSHA)
}

func TestIgnoredDeliveriesTouchNothing(t *testing.T) {
	cases := map[string]struct {
		event string
		body  []byte
	}{
		"wrong context":              {"status", nil}, // filled below
		"foreign branches below cap": {"status", nil},
		"pending state":              {"status", nil},
		"ping":                       {"ping", []byte(`{"zen":"ok"}`)},
		"unknown event":              {"issues", []byte(`{}`)},
		"non-tracked ref":            {"push", nil},
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
			f := servingFixture(t, repo, "A")
			before := readState(t, f.statePath)
			gitOps := repo.gitOps()

			var body []byte
			event := cases[name].event
			switch name {
			case "wrong context":
				body = statusBody(t, "B", "success", "some-other-check", "master")
			case "foreign branches below cap":
				// Below statusBranchesCap the list is provably complete, so
				// the prefilter drops a green whose branches miss the
				// tracked one without a single git op.
				body = statusBody(t, "B", "success", "all-builds", "claude/x", "claude/y", "claude/z")
			case "pending state":
				body = statusBody(t, "B", "pending", "all-builds", "master")
			case "non-tracked ref":
				body = pushBody(t, "refs/heads/claude/x", "B")
			default:
				body = cases[name].body
			}

			status, err := f.gate.HandleEvent(event, body)
			require.NoError(t, err)
			assert.Contains(t, status, "ignored")
			assert.Equal(t, gitOps, repo.gitOps(), "ignored deliveries must make zero git calls")
			assert.Zero(t, *f.applies)
			assert.Equal(t, before, readState(t, f.statePath), "state unchanged")
		})
	}
}

func TestPushFetchErrorSurfaces(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	repo.fetchErr = fmt.Errorf("remote unreachable")

	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.Error(t, err, "a fetch failure must 500 so GitHub records a redeliverable delivery")
	assert.Contains(t, eventKinds(f.rec), "git.pull_failed")
}

func TestStartupRestoresLastGood(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true})
	repo := &fakeRepo{head: "Z", known: map[string]bool{"A": true}}
	f := newFixtureAt(t, repo, statePath)

	f.gate.Startup()

	assert.Equal(t, []string{"A"}, repo.resets)
	assert.Equal(t, 1, repo.fetchSHACalls)
	assert.Zero(t, *f.applies, "startup never applies — the watcher's initial scan loads")
	assert.NotContains(t, eventKinds(f.rec), "reload.unverified")
	assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)
}

func TestStartupNormalRestartIsQuiet(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true})
	repo := &fakeRepo{head: "A"}
	f := newFixtureAt(t, repo, statePath)

	f.gate.Startup()

	assert.Empty(t, repo.resets)
	assert.Zero(t, repo.fetchSHACalls+repo.fetchBranchCalls)
	assert.Empty(t, attentionKeys(f.agg))
}

func TestStartupVanishedShaFallsToTipUnverified(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true})
	repo := &fakeRepo{head: "Z", tip: "T", commits: []string{"T"}, known: map[string]bool{}}
	f := newFixtureAt(t, repo, statePath)

	f.gate.Startup()

	assert.Equal(t, []string{"T"}, repo.resets)
	st := readState(t, f.statePath)
	assert.Equal(t, "T", st.ServingSHA)
	assert.False(t, st.Verified)
	assert.Contains(t, eventKinds(f.rec), "reload.unverified")
	assert.Contains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)
	assert.Zero(t, *f.applies)
}

func TestStartupTotalGitFailureKeepsLastGoodRecord(t *testing.T) {
	// Every git op fails (dead remote, broken clone): startup must degrade
	// to serving whatever the tree holds — no apply, loud — and must NOT
	// rewrite the state file, so the last-good record survives untouched
	// for the next boot.
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true})
	repo := &fakeRepo{
		headErr:  fmt.Errorf("head: repo broken"),
		fetchErr: fmt.Errorf("remote unreachable"),
		resetErr: fmt.Errorf("reset: repo broken"),
		// known stays nil, so FetchSHA errors too.
	}
	f := newFixtureAt(t, repo, statePath)

	f.gate.Startup()

	assert.Zero(t, *f.applies, "degraded startup must not apply")
	assert.Empty(t, repo.resets, "the tree never moved")
	assert.Contains(t, eventKinds(f.rec), "reload.failed")
	assert.Contains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)

	// The no-persist-on-degrade guarantee: the on-disk record still names
	// the original last-good sha.
	st := readState(t, f.statePath)
	assert.Equal(t, "A", st.ServingSHA)
	assert.True(t, st.Verified)
}

func TestStartupFreshThenGreenVerifies(t *testing.T) {
	repo := &fakeRepo{head: "A"}
	f := newFixture(t, repo)

	f.gate.Startup()

	st := readState(t, f.statePath)
	assert.Equal(t, "A", st.ServingSHA)
	assert.False(t, st.Verified)
	assert.Contains(t, eventKinds(f.rec), "reload.unverified")
	assert.Contains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)

	// The first green for the serving tree verifies it in place: no
	// reset, no apply, entry resolved.
	status, err := f.gate.HandleEvent("status", statusBody(t, "A", "success", "all-builds", "master"))
	require.NoError(t, err)
	assert.Equal(t, "already-serving", status)
	assert.Empty(t, repo.resets)
	assert.Zero(t, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.verified")
	assert.True(t, readState(t, f.statePath).Verified)
	assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)
}

func TestStartupReArmsHeldFromPersistedPending(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true, PendingSHA: "B", PendingState: "failure"})
	repo := &fakeRepo{head: "A"}
	f := newFixtureAt(t, repo, statePath)

	f.gate.Startup()

	keys := attentionKeys(f.agg)
	require.Contains(t, keys, attention.KeyReloadHeld)
	assert.Contains(t, keys[attention.KeyReloadHeld], "failure")
	assert.NotContains(t, keys, attention.KeyReloadUnverified)
}

func TestForceBypassesGate(t *testing.T) {
	repo := &fakeRepo{tip: "C", commits: []string{"C", "B", "A"}}
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: false, PendingSHA: "C", PendingState: "failure"})
	repo.head = "A"
	f := newFixtureAt(t, repo, statePath)
	f.gate.Startup()
	require.Contains(t, attentionKeys(f.agg), attention.KeyReloadHeld)
	require.Contains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)

	require.NoError(t, f.gate.Force())

	assert.Equal(t, []string{"C"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.forced")
	st := readState(t, f.statePath)
	assert.Equal(t, "C", st.ServingSHA)
	assert.True(t, st.Verified)
	assert.Empty(t, st.PendingSHA)
	assert.Empty(t, attentionKeys(f.agg), "both gate entries resolved")
}

func TestMalformedPayloadsIgnoredLoudly(t *testing.T) {
	for _, event := range []string{"push", "status"} {
		t.Run(event, func(t *testing.T) {
			repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
			f := servingFixture(t, repo, "A")
			gitOps := repo.gitOps()

			status, err := f.gate.HandleEvent(event, []byte(`{"broken`))
			require.NoError(t, err)
			assert.Equal(t, "ignored", status)
			assert.Equal(t, gitOps, repo.gitOps())
			assert.Zero(t, *f.applies)
			assert.Contains(t, eventKinds(f.rec), "reload.ignored")
		})
	}
}

func TestUnparseableStateFileIsHardError(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	require.NoError(t, os.WriteFile(statePath, []byte("{not json"), 0o600))
	_, err := New(Config{
		Repo:      &fakeRepo{},
		Context:   "all-builds",
		StatePath: statePath,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.Error(t, err)
}
