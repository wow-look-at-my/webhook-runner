package reloadgate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The incident: a green for B was delivered and applied, the operator rolled
// the tree back to A, and the poll then had to buy B's verdict back from the
// GitHub API — with no token, it held blind forever on a fact it had already
// been told. A delivered verdict must survive the switch that consumed it.
func TestStatusVerdictPersisted(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.HandleEvent("status", statusBody(t, "B", "success", "all-builds", "master"))
	require.NoError(t, err)

	st := readState(t, f.statePath)
	require.Len(t, st.Verdicts, 1, "the delivered verdict must be written down")
	assert.Equal(t, "B", st.Verdicts[0].SHA)
	assert.Equal(t, "success", st.Verdicts[0].State)
}

// A green the ordering rule REFUSES is still a verified fact about that sha —
// discarding it is the whole defect.
func TestIgnoredStaleGreenStillRecorded(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	status, err := f.gate.HandleEvent("status", statusBody(t, "Z", "success", "all-builds", "master"))
	require.NoError(t, err)
	require.Equal(t, "ignored-stale", status, "Z is not in recent history")
	assert.Empty(t, repo.resets, "the ordering rule still refuses the switch")

	state, ok := f.gate.verdictFor("Z")
	assert.True(t, ok, "a refused green must still be recorded")
	assert.Equal(t, "success", state)
}

// The poll falls back to the recorded verdict when the API cannot answer —
// the no-token case that froze deploys.
func TestPollUsesRecordedVerdictWhenAPIUnreadable(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	f.gate.status = nil // no WEBHOOK_RUNNER_GITHUB_TOKEN
	f.gate.recordVerdict("B", "success")

	assert.Equal(t, "switched", f.gate.Reconcile(context.Background()))
	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Contains(t, eventKinds(f.rec), "reload.poll_recorded")
	assert.NotContains(t, eventKinds(f.rec), "reload.poll_blind")
}

// With no verdict on record either, the blind hold is unchanged.
func TestPollHoldsBlindWithoutVerdict(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	f.gate.status = nil

	assert.Equal(t, "held-blind", f.gate.Reconcile(context.Background()))
	assert.Empty(t, repo.resets)
	assert.Contains(t, eventKinds(f.rec), "reload.poll_blind")
}

// The API is authoritative: the poll exists to catch what the webhook missed,
// so a stale recorded green must never mask a fresher red.
func TestPollPrefersAPIOverRecordedVerdict(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{state: "failure"}
	f.gate.status = st.fn
	f.gate.recordVerdict("B", "success") // a re-run went red; that status was lost

	assert.Equal(t, "held-red", f.gate.Reconcile(context.Background()))
	assert.Empty(t, repo.resets, "the fresher API red must win")
	assert.Equal(t, 1, st.calls)
}

// A recorded verdict answers "is it green?", never "should the tree switch?" —
// the ordering rule still gates the apply.
func TestRecordedVerdictDoesNotBypassOrderingRule(t *testing.T) {
	repo := &fakeRepo{tip: "Z", commits: []string{"B", "A"}} // Z is not in history
	f := servingFixture(t, repo, "A")
	f.gate.status = nil
	f.gate.recordVerdict("Z", "success")

	assert.Equal(t, "ignored-stale", f.gate.Reconcile(context.Background()))
	assert.Empty(t, repo.resets)
}

// A CI re-run flipping the verdict is respected: last write wins per sha.
func TestRecordVerdictLastWriteWins(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	f.gate.recordVerdict("B", "success")
	f.gate.recordVerdict("B", "failure")

	state, ok := f.gate.verdictFor("B")
	require.True(t, ok)
	assert.Equal(t, "failure", state)
	assert.Len(t, readState(t, f.statePath).Verdicts, 1, "an upsert, not an append")
}

func TestVerdictsExpireAndStayBounded(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	f.gate.mu.Lock()
	f.gate.verdicts = []verdictRecord{{SHA: "old", State: "success", SeenAt: time.Now().UTC().Add(-verdictTTL - time.Hour)}}
	f.gate.mu.Unlock()
	_, ok := f.gate.verdictFor("old")
	assert.False(t, ok, "an expired verdict must not answer")

	for i := 0; i < maxVerdicts+25; i++ {
		f.gate.recordVerdict(shaN(i), "success")
	}
	f.gate.mu.Lock()
	n := len(f.gate.verdicts)
	f.gate.mu.Unlock()
	assert.LessOrEqual(t, n, maxVerdicts, "the store stays bounded")
	assert.NotContains(t, verdictSHAs(f.gate), "old", "expired records are pruned")
}

// A state file written before this field loads as an empty store, not an error.
func TestLoadStateWithoutVerdicts(t *testing.T) {
	repo := &fakeRepo{tip: "A", commits: []string{"A"}}
	f := servingFixture(t, repo, "A") // seedState writes no Verdicts field

	_, ok := f.gate.verdictFor("A")
	assert.False(t, ok)
	assert.NotPanics(t, func() { f.gate.recordVerdict("A", "success") })
}

func TestReadGatingStateSurfacesAPIErrorWhenNoVerdict(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	st := &countingStatus{err: errors.New("boom")}
	f.gate.status = st.fn

	_, err := f.gate.readGatingState(context.Background(), "B")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom", "the API's own error must reach the blind hold")
}

func shaN(i int) string { return "sha-" + time.Unix(int64(i), 0).UTC().Format("150405.000000000") }

func verdictSHAs(g *Gate) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.verdicts))
	for _, v := range g.verdicts {
		out = append(out, v.SHA)
	}
	return out
}
