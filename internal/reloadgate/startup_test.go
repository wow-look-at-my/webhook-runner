package reloadgate

// Startup-path tests: what the gate serves when the process boots -- restore
// the persisted last-good commit, or degrade LOUDLY and keep running. The
// fixtures these use (fakeRepo, seedState, newGate, ...) live in gate_test.go.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

func TestStartupRestoresLastGood(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "reload-gate.json")
	seedState(t, statePath, gateState{ServingSHA: "A", Verified: true})
	repo := &fakeRepo{head: "Z", known: set.Of("A")}
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
	repo := &fakeRepo{head: "Z", tip: "T", commits: []string{"T"}, known: set.New[string]()}
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
	// Total git failure: startup degrades loud, with no apply and no state rewrite.
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

	// The on-disk record still names the original last-good sha.
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

	// The green for the serving tree verifies it in place: no reset, no apply.
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
