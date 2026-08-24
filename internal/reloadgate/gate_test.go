package reloadgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// No pending (the push delivery was missed); a red for a foreign sha still records the hold so the operator sees it.
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
	// GitHub caps the status payload's branches array at 10; with the sha on more branches than that the tracked one can be squeezed out.
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
	// Enough commits landed while the runner was held that the serving sha fell out of the fetchDepth window: RecentCommits no longer lists it.
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

	// Green for the INTERMEDIATE commit B: switch to B (the evented sha, not the tip), keep C pending and the held entry alive.
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
				// Below statusBranchesCap the list is provably complete, so the prefilter drops a green whose branches miss the tracked one without a single.
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

// The outage shape: GitHub is the reason the tree is held, so the fetch
// Force opens with is exactly what cannot be relied on. It must still land
// on the held commit — the push webhook already fetched that one — rather
// than erroring out or (unbounded) hanging the admin port behind the gate
// mutex.
func TestForceFallsBackToTheHeldCommitWhenGitHubIsDown(t *testing.T) {
	repo := &fakeRepo{fetchErr: errors.New("origin unreachable"), commits: []string{"C", "A"}}
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: false, PendingSHA: "C", PendingState: "pending"})
	repo.head = "A"
	f := newFixtureAt(t, repo, statePath)
	f.gate.Startup()

	require.NoError(t, f.gate.Force(), "a dead origin must not block the operator's bypass")

	assert.Equal(t, []string{"C"}, repo.resets, "forced to the commit already fetched and held")
	assert.Equal(t, 1, *f.applies)
	kinds := eventKinds(f.rec)
	assert.Contains(t, kinds, "reload.forced")
	assert.Contains(t, kinds, "git.pull_failed", "the degraded path is LOUDER, never silent")
	st := readState(t, f.statePath)
	assert.Equal(t, "C", st.ServingSHA)
	assert.True(t, st.Verified)
}

// With nothing local to fall back to, a dead origin is a real failure and
// says so — never a success that moved nothing.
func TestForceStillFailsWhenThereIsNothingLocalToForceTo(t *testing.T) {
	// No pending hold and no resolvable origin/master: nothing local is newer than what is already serving.
	repo := &fakeRepo{fetchErr: errors.New("origin unreachable")}
	f := servingFixture(t, repo, "A")

	err := f.gate.Force()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch hooks repo")
	assert.Empty(t, repo.resets, "nothing moved")
}

// EVERY fetch this package runs must go through the bounded helper. This is
// a source-level assertion on purpose: the failure it prevents is not a
// wrong value but a call that never returns, which no behavioural test can
// observe without hanging the suite itself. A production runner froze this
// way -- one unbounded fetch under g.mu took /version, /reload/status, both
// force paths, the status webhook and the poll with it, and the tree could
// not move by any route until the process was restarted.
func TestEveryGateFetchIsBounded(t *testing.T) {
	for _, name := range []string{"gate.go", "poll.go", "manual.go"} {
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "repo.FetchBranch(") {
				continue
			}
			t.Errorf("%s:%d calls the UNBOUNDED repo.FetchBranch; use g.fetchBranchBounded() — an unbounded fetch here can hang forever holding the gate mutex:\n\t%s",
				name, i+1, strings.TrimSpace(line))
		}
	}
}

// The status webhook's switch path fetches under the gate mutex. A dead
// origin must make it fail CLOSED and release the lock, never hold it.
func TestStatusSwitchFailsClosedOnDeadOrigin(t *testing.T) {
	repo := &fakeRepo{fetchErr: errors.New("origin unreachable"), commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))

	require.Error(t, err, "a fetch it could not complete must not read as a switch")
	assert.NotEqual(t, "reloaded", status)
	assert.Empty(t, repo.resets, "nothing moved")
	assert.Zero(t, *f.applies)

	// The lock is FREE: the next call answers instead of hanging behind it.
	done := make(chan struct{})
	go func() { defer close(done); _ = f.gate.Status() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gate mutex still held after a failed fetch — this is the production freeze")
	}
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

// TreeState mirrors the gate's recorded state through the full
// held→red→switched lifecycle — the pure read /version renders — and
// never performs git work.
func TestTreeStateSnapshot(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	ts := f.gate.TreeState()
	assert.Equal(t, TreeState{ServingSHA: "A", Verified: true, Context: "all-builds"}, ts)

	// A pushed newer tip is recorded pending.
	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	ts = f.gate.TreeState()
	assert.Equal(t, TreeState{ServingSHA: "A", Verified: true, PendingSHA: "B", PendingState: "pending", Context: "all-builds"}, ts)

	// A red gating status updates the pending state.
	_, err = f.gate.HandleEvent("status", statusBody(t, "B", "failure", "all-builds", "master"))
	require.NoError(t, err)
	ts = f.gate.TreeState()
	assert.Equal(t, "B", ts.PendingSHA)
	assert.Equal(t, "failure", ts.PendingState)

	// The green switch settles: serving B, verified, nothing pending.
	gitOps := repo.gitOps()
	_, err = f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)
	ts = f.gate.TreeState()
	assert.Equal(t, TreeState{ServingSHA: "B", Verified: true, Context: "all-builds"}, ts)
	assert.Greater(t, repo.gitOps(), gitOps, "sanity: the switch did git work")

	gitOps = repo.gitOps()
	_ = f.gate.TreeState()
	assert.Equal(t, gitOps, repo.gitOps(), "TreeState must never touch git")
}
