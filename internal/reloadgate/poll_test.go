package reloadgate

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// countingStatus is a scriptable StatusFunc that records its calls.
// Mutate state/err between Reconcile calls to script transitions.
type countingStatus struct {
	state string
	err   error
	calls int
	shas  []string
}

func (s *countingStatus) fn(ctx context.Context, sha string) (string, error) {
	s.calls++
	s.shas = append(s.shas, sha)
	return s.state, s.err
}

func countKind(rec *events.Recorder, kind string) int {
	n := 0
	for _, ev := range rec.List(0) {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func TestReconcileUpToDateMakesNoStatusCall(t *testing.T) {
	repo := &fakeRepo{tip: "A", commits: []string{"A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{state: "success"}
	f.gate.status = st.fn

	f.gate.Reconcile(context.Background())

	assert.Zero(t, st.calls, "tip == serving must make no status API call")
	assert.Zero(t, *f.applies)
	assert.Empty(t, repo.resets)
	assert.Equal(t, 1, repo.fetchBranchCalls, "one fetch to learn the tip")
	s := readState(t, f.statePath)
	assert.Equal(t, "A", s.ServingSHA)
	assert.True(t, s.Verified)
	assert.Empty(t, attentionKeys(f.agg))
}

func TestReconcileGreenSwitches(t *testing.T) {
	// The startup catch-up shape: a green landed while the runner was down
	// (or its status webhook was missed) — the first poll pass switches
	// through the exact event path.
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{state: "success"}
	f.gate.status = st.fn

	f.gate.Reconcile(context.Background())

	assert.Equal(t, 1, st.calls)
	assert.Equal(t, []string{"B"}, st.shas, "the status read is for the fetched tip")
	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.switched")
	s := readState(t, f.statePath)
	assert.Equal(t, "B", s.ServingSHA)
	assert.True(t, s.Verified)
	assert.Empty(t, s.PendingSHA)
	assert.Empty(t, attentionKeys(f.agg))
}

func TestReconcileRedHolds(t *testing.T) {
	for _, state := range []string{"failure", "error"} {
		t.Run(state, func(t *testing.T) {
			repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
			f := servingFixture(t, repo, "A")
			f.gate.status = (&countingStatus{state: state}).fn

			f.gate.Reconcile(context.Background())

			assert.Empty(t, repo.resets, "a red tip must never switch")
			assert.Zero(t, *f.applies)
			assert.Contains(t, eventKinds(f.rec), "reload.held_red")
			s := readState(t, f.statePath)
			assert.Equal(t, "A", s.ServingSHA)
			assert.Equal(t, "B", s.PendingSHA)
			assert.Equal(t, state, s.PendingState)
			keys := attentionKeys(f.agg)
			require.Contains(t, keys, attention.KeyReloadHeld)
			assert.Contains(t, keys[attention.KeyReloadHeld], state)
			assert.NotContains(t, keys, attention.KeyReloadPoll, "a determined verdict is not blindness")
		})
	}
}

func TestReconcilePendingOrUnreportedHolds(t *testing.T) {
	// "pending" and "" (no status for the context yet) both mean "not
	// affirmatively green": hold, exactly as a push delivery records a
	// not-yet-green tip.
	for name, state := range map[string]string{"pending": "pending", "no status yet": ""} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
			f := servingFixture(t, repo, "A")
			f.gate.status = (&countingStatus{state: state}).fn

			f.gate.Reconcile(context.Background())

			assert.Empty(t, repo.resets)
			assert.Zero(t, *f.applies)
			assert.Contains(t, eventKinds(f.rec), "reload.held")
			s := readState(t, f.statePath)
			assert.Equal(t, "A", s.ServingSHA)
			assert.Equal(t, "B", s.PendingSHA)
			assert.Equal(t, "pending", s.PendingState)
			keys := attentionKeys(f.agg)
			require.Contains(t, keys, attention.KeyReloadHeld)
			assert.Contains(t, keys[attention.KeyReloadHeld], "awaiting all-builds")
		})
	}
}

func TestReconcileNoReaderHoldsLoudly(t *testing.T) {
	// No StatusFunc configured at all: the poll must fail closed — the
	// tree stays put and the blindness is loud (event + attention entry).
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	f.gate.Reconcile(context.Background())

	assert.Empty(t, repo.resets)
	assert.Zero(t, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.poll_blind")
	keys := attentionKeys(f.agg)
	require.Contains(t, keys, attention.KeyReloadPoll)
	assert.Contains(t, keys[attention.KeyReloadPoll], "could not be read")
	// Blindness records no pending sha — it learned nothing definitive.
	assert.Empty(t, readState(t, f.statePath).PendingSHA)
}

func TestReconcileStatusErrorHoldsThenRecovers(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{err: fmt.Errorf("api unreachable")}
	f.gate.status = st.fn

	f.gate.Reconcile(context.Background())
	assert.Empty(t, repo.resets)
	assert.Contains(t, eventKinds(f.rec), "reload.poll_blind")
	require.Contains(t, attentionKeys(f.agg), attention.KeyReloadPoll)

	// The same problem on the next tick stays quiet on the feed (the
	// attention entry is the persistent surface).
	f.gate.Reconcile(context.Background())
	assert.Equal(t, 1, countKind(f.rec, "reload.poll_blind"))

	// The credential/API recovers and the tip is green: switch, and the
	// blindness entry resolves.
	st.err, st.state = nil, "success"
	f.gate.Reconcile(context.Background())
	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	assert.Empty(t, attentionKeys(f.agg))
}

func TestReconcileFetchErrorLeavesEverything(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{state: "success"}
	f.gate.status = st.fn
	repo.fetchErr = fmt.Errorf("remote unreachable")
	before := readState(t, f.statePath)

	f.gate.Reconcile(context.Background())

	assert.Contains(t, eventKinds(f.rec), "git.pull_failed")
	assert.Zero(t, st.calls)
	assert.Zero(t, *f.applies)
	assert.Empty(t, repo.resets)
	assert.Equal(t, before, readState(t, f.statePath), "state unchanged")
}

func TestReconcileGreenNotInHistoryIgnoredStale(t *testing.T) {
	// The ordering rules are the SAME shared path the status event uses: a
	// tip whose sha the freshly-fetched history does not vouch for is
	// ignored stale, never applied — even with the API reporting green.
	repo := &fakeRepo{tip: "B", commits: []string{"C", "A"}}
	f := servingFixture(t, repo, "A")
	f.gate.status = (&countingStatus{state: "success"}).fn

	f.gate.Reconcile(context.Background())

	assert.Empty(t, repo.resets)
	assert.Zero(t, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.ignored_stale")
	assert.Equal(t, "A", readState(t, f.statePath).ServingSHA)
}

func TestReconcileRepeatTicksQuiet(t *testing.T) {
	// An unchanged verdict on later ticks re-records nothing: the
	// persistent attention entries are the surface, not hourly feed spam.
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{state: "failure"}
	f.gate.status = st.fn

	f.gate.Reconcile(context.Background())
	f.gate.Reconcile(context.Background())
	assert.Equal(t, 1, countKind(f.rec, "reload.held_red"))

	// The verdict changing (red -> pending, e.g. a new CI run) records
	// exactly once more.
	st.state = "pending"
	f.gate.Reconcile(context.Background())
	f.gate.Reconcile(context.Background())
	assert.Equal(t, 1, countKind(f.rec, "reload.held"))
}

func TestReconcileRedRepointsPendingToTip(t *testing.T) {
	// A push event recorded B pending; then C landed but its push webhook
	// was missed and C's CI failed. The poll must point the hold at the
	// actual tip (what the missed push would have recorded) with its red
	// state.
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)

	repo.tip, repo.commits = "C", []string{"C", "B", "A"}
	f.gate.status = (&countingStatus{state: "failure"}).fn
	f.gate.Reconcile(context.Background())

	s := readState(t, f.statePath)
	assert.Equal(t, "C", s.PendingSHA)
	assert.Equal(t, "failure", s.PendingState)
	assert.Empty(t, repo.resets)
	assert.Contains(t, attentionKeys(f.agg)[attention.KeyReloadHeld], "failure")
}

func TestReconcileStartupCatchupAfterDowntime(t *testing.T) {
	// The runner was down while master advanced and greened: the poller's
	// immediate first pass (Poller fires once on start) converges without
	// any webhook delivery.
	statePath := t.TempDir() + "/reload-gate.json"
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true})
	repo := &fakeRepo{head: "A", tip: "C", commits: []string{"C", "B", "A"}}
	f := newFixtureAt(t, repo, statePath)
	f.gate.Startup()
	require.Zero(t, *f.applies)
	f.gate.status = (&countingStatus{state: "success"}).fn

	f.gate.Reconcile(context.Background())

	assert.Equal(t, []string{"C"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	s := readState(t, f.statePath)
	assert.Equal(t, "C", s.ServingSHA)
	assert.True(t, s.Verified)
	assert.Empty(t, attentionKeys(f.agg))
}
