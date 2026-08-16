package reloadgate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

// statusFor scripts the gate's StatusFunc per sha; absent shas error (the
// "unreadable" verdict).
func statusFor(states map[string]string) StatusFunc {
	return func(_ context.Context, sha string) (string, error) {
		if st, ok := states[sha]; ok {
			return st, nil
		}
		return "", fmt.Errorf("no scripted status for %s", sha)
	}
}

func TestGateStatusSnapshot(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	st := f.gate.Status()
	assert.Equal(t, "A", st.ServingSHA)
	assert.True(t, st.Verified)
	assert.Empty(t, st.PendingSHA)
	assert.Empty(t, st.PendingState)
	assert.Equal(t, "master", st.Branch)
	assert.Equal(t, "all-builds", st.Context)

	// A held push shows up as pending with its last known state.
	_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	st = f.gate.Status()
	assert.Equal(t, "B", st.PendingSHA)
	assert.Equal(t, "pending", st.PendingState)
}

func TestGateCIState(t *testing.T) {
	repo := &fakeRepo{}
	f := servingFixture(t, repo, "A")
	ctx := context.Background()

	assert.Equal(t, "unknown", f.gate.CIState(ctx, "X"), "nil reader = unknown, never a guess")

	f.gate.status = statusFor(map[string]string{"G": "success", "R": "failure", "P": "pending", "N": ""})
	assert.Equal(t, "success", f.gate.CIState(ctx, "G"))
	assert.Equal(t, "failure", f.gate.CIState(ctx, "R"))
	assert.Equal(t, "pending", f.gate.CIState(ctx, "P"))
	assert.Equal(t, "none", f.gate.CIState(ctx, "N"), "determinate no-status is none, not unknown")
	assert.Equal(t, "unknown", f.gate.CIState(ctx, "missing"), "reader error = unknown")
	assert.Equal(t, "unknown", f.gate.CIState(ctx, ""), "empty sha = unknown")
}

func TestManualSwitchGreenSwitches(t *testing.T) {
	repo := &fakeRepo{tip: "C", commits: []string{"C", "B", "A"}, srcAt: set.Of("C")}
	f := servingFixture(t, repo, "A")
	f.gate.status = statusFor(map[string]string{"C": "success"})

	out, err := f.gate.ManualSwitch(context.Background(), "C", false)
	require.NoError(t, err)
	assert.True(t, out.Switched)
	assert.False(t, out.Overridden())
	assert.Empty(t, out.Reasons)
	assert.Equal(t, "C", out.SHA)
	assert.Equal(t, "success", out.CIState)
	assert.True(t, out.HasSrc)

	assert.Equal(t, []string{"C"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.switched")
	assert.NotContains(t, eventKinds(f.rec), "reload.forced", "a green pick is not a gate bypass")

	st := readState(t, f.statePath)
	assert.Equal(t, "C", st.ServingSHA)
	assert.True(t, st.Verified)
	assert.Empty(t, st.PendingSHA, "switching to the tip settles pending")
}

func TestManualSwitchRollbackToOlderGreen(t *testing.T) {
	// The rollback shape trySwitch's ordering rule would refuse: the target
	// is OLDER than the serving commit. Manual authority allows it.
	repo := &fakeRepo{tip: "C", commits: []string{"C", "B", "A"}, srcAt: set.Of("A")}
	f := servingFixture(t, repo, "C")
	f.gate.status = statusFor(map[string]string{"A": "success"})

	out, err := f.gate.ManualSwitch(context.Background(), "A", false)
	require.NoError(t, err)
	assert.True(t, out.Switched)
	assert.Empty(t, out.Reasons)
	assert.Equal(t, []string{"A"}, repo.resets)
	assert.Equal(t, 1, *f.applies)

	st := readState(t, f.statePath)
	assert.Equal(t, "A", st.ServingSHA)
	assert.True(t, st.Verified)
}

func TestManualSwitchRefusedWithoutOverride(t *testing.T) {
	cases := map[string]struct {
		states     map[string]string // scripted CI; absent = unreadable
		srcAt      set.Set[string]
		wantCI     string
		wantSrc    bool
		wantReason []string
	}{
		"unknown ci": {
			states: map[string]string{}, srcAt: set.Of("B"),
			wantCI: "unknown", wantSrc: true,
			wantReason: []string{`"unknown"`},
		},
		"red ci": {
			states: map[string]string{"B": "failure"}, srcAt: set.Of("B"),
			wantCI: "failure", wantSrc: true,
			wantReason: []string{`"failure"`},
		},
		"missing src": {
			states: map[string]string{"B": "success"}, srcAt: set.New[string](),
			wantCI: "success", wantSrc: false,
			wantReason: []string{"no src/hooks"},
		},
		"both": {
			states: map[string]string{"B": "pending"}, srcAt: set.New[string](),
			wantCI: "pending", wantSrc: false,
			wantReason: []string{`"pending"`, "no src/hooks"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}, srcAt: tc.srcAt}
			f := servingFixture(t, repo, "A")
			f.gate.status = statusFor(tc.states)

			out, err := f.gate.ManualSwitch(context.Background(), "B", false)
			require.NoError(t, err)
			assert.False(t, out.Switched, "a failing commit must not switch without override")
			assert.Equal(t, tc.wantCI, out.CIState)
			assert.Equal(t, tc.wantSrc, out.HasSrc)
			require.NotEmpty(t, out.Reasons)
			for _, want := range tc.wantReason {
				assert.Contains(t, fmt.Sprint(out.Reasons), want)
			}

			assert.Empty(t, repo.resets, "refusal must not move the tree")
			assert.Zero(t, *f.applies)
			assert.Contains(t, eventKinds(f.rec), "reload.switch_refused")
			st := readState(t, f.statePath)
			assert.Equal(t, "A", st.ServingSHA, "state unchanged on refusal")
		})
	}
}

func TestManualSwitchOverrideSwitchesLoudly(t *testing.T) {
	// Src missing, and under override the CI state is deliberately not
	// probed — the fully wedged shape. The override must still work (the
	// documented escape hatch) and be recorded loudly with both named.
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")
	f.gate.status = statusFor(map[string]string{})

	out, err := f.gate.ManualSwitch(context.Background(), "B", true)
	require.NoError(t, err)
	assert.True(t, out.Switched)
	assert.True(t, out.Overridden())
	require.Len(t, out.Reasons, 2)

	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Equal(t, 1, *f.applies)
	assert.Contains(t, eventKinds(f.rec), "reload.forced")
	var forcedMsg string
	for _, ev := range f.rec.List(0) {
		if ev.Kind == "reload.forced" {
			forcedMsg = ev.Msg
		}
	}
	assert.Contains(t, forcedMsg, "OVERRIDING")
	assert.Contains(t, forcedMsg, "not probed")
	assert.Contains(t, forcedMsg, "src/hooks")

	st := readState(t, f.statePath)
	assert.Equal(t, "B", st.ServingSHA)
	assert.True(t, st.Verified, "the operator vouched")
}

func TestManualSwitchWorksWhenFetchFails(t *testing.T) {
	// Origin unreachable: the rollback-to-a-local-commit escape hatch must
	// still work (fetch is best-effort freshness, never a gate).
	repo := &fakeRepo{fetchErr: errors.New("origin down"), commits: []string{"C", "B", "A"},
		known: set.Of("A")}
	f := servingFixture(t, repo, "C")
	f.gate.status = nil // and CI unreadable too

	out, err := f.gate.ManualSwitch(context.Background(), "A", true)
	require.NoError(t, err)
	assert.True(t, out.Switched)
	// An OVERRIDE does not probe CI at all — the answer changes nothing once
	// the operator has decided, and the probe is a GitHub call that hangs
	// when GitHub is the thing that is broken. Reported as not-probed, never
	// as a green nobody saw.
	assert.Equal(t, ciStateNotProbed, out.CIState)
	assert.Equal(t, []string{"A"}, repo.resets)
}

// The force path must not depend on GitHub: an operator forces a commit live
// precisely when the gate is stuck, which is usually because GitHub is
// unreachable. It resolves the ref locally, skips the CI probe, and is
// recorded as FORCED (never as a normal green switch, since no green was
// read).
func TestManualSwitchOverrideNeedsNoGitHub(t *testing.T) {
	repo := &fakeRepo{fetchErr: errors.New("origin down"), commits: []string{"B", "A"},
		known: set.Of("B")}
	f := servingFixture(t, repo, "A")
	f.gate.status = nil // any CI probe would fail

	out, err := f.gate.ManualSwitch(context.Background(), "B", true)
	require.NoError(t, err)
	assert.True(t, out.Switched, "a dead remote must not stop the force")
	assert.Equal(t, []string{"B"}, repo.resets)
	assert.Equal(t, ciStateNotProbed, out.CIState)

	assert.Contains(t, eventKinds(f.rec), "reload.forced",
		"an override is FORCED in the audit trail even with no reasons — we never read a green")
	assert.NotContains(t, eventKinds(f.rec), "reload.switched")
}

func TestManualSwitchUnknownRef(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
	f := servingFixture(t, repo, "A")

	_, err := f.gate.ManualSwitch(context.Background(), "nope", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownRef)
	assert.Empty(t, repo.resets)
	assert.Zero(t, *f.applies)
	st := readState(t, f.statePath)
	assert.Equal(t, "A", st.ServingSHA)
}

func TestManualSwitchPendingBookkeeping(t *testing.T) {
	t.Run("switching to the pending commit clears the hold", func(t *testing.T) {
		repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}, srcAt: set.Of("B")}
		f := servingFixture(t, repo, "A")
		_, err := f.gate.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
		require.NoError(t, err)
		require.Contains(t, attentionKeys(f.agg), attention.KeyReloadHeld)

		f.gate.status = statusFor(map[string]string{"B": "success"})
		out, err := f.gate.ManualSwitch(context.Background(), "B", false)
		require.NoError(t, err)
		require.True(t, out.Switched)

		st := readState(t, f.statePath)
		assert.Empty(t, st.PendingSHA)
		assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadHeld)
	})

	t.Run("rollback keeps a hold for a different commit visible", func(t *testing.T) {
		// C (the tip) is held red; the operator rolls back to A. The C hold
		// must survive — that commit is STILL awaiting its green.
		repo := &fakeRepo{tip: "C", commits: []string{"C", "B", "A"}, srcAt: set.Of("A")}
		f := servingFixture(t, repo, "B")
		_, err := f.gate.HandleEvent("status", statusBody(t, "C", "failure", "all-builds", "master"))
		require.NoError(t, err)
		require.Contains(t, attentionKeys(f.agg), attention.KeyReloadHeld)

		f.gate.status = statusFor(map[string]string{"A": "success"})
		out, err := f.gate.ManualSwitch(context.Background(), "A", false)
		require.NoError(t, err)
		require.True(t, out.Switched)

		st := readState(t, f.statePath)
		assert.Equal(t, "A", st.ServingSHA)
		assert.Equal(t, "C", st.PendingSHA, "the red tip stays recorded")
		assert.Equal(t, "failure", st.PendingState)
		assert.Contains(t, attentionKeys(f.agg), attention.KeyReloadHeld, "the hold entry survives the rollback")
	})

	t.Run("switching to the tip clears a hold for an intermediate commit", func(t *testing.T) {
		// B is held red, the operator force-picks the newer tip C: nothing
		// is awaited anymore.
		repo := &fakeRepo{tip: "C", commits: []string{"C", "B", "A"}, srcAt: set.Of("C")}
		f := servingFixture(t, repo, "A")
		_, err := f.gate.HandleEvent("status", statusBody(t, "B", "failure", "all-builds", "master"))
		require.NoError(t, err)
		require.Contains(t, attentionKeys(f.agg), attention.KeyReloadHeld)

		f.gate.status = statusFor(map[string]string{"C": "success"})
		out, err := f.gate.ManualSwitch(context.Background(), "C", false)
		require.NoError(t, err)
		require.True(t, out.Switched)

		st := readState(t, f.statePath)
		assert.Empty(t, st.PendingSHA)
		assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadHeld)
	})
}

func TestManualSwitchResolvesUnverifiedServing(t *testing.T) {
	// An unverified boot (fresh clone, no recorded green): any manual
	// switch — the operator vouching — resolves the unverified entry.
	repo := &fakeRepo{head: "A", tip: "A", commits: []string{"A"}, srcAt: set.Of("A")}
	f := newFixture(t, repo)
	f.gate.Startup()
	require.Contains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)

	f.gate.status = statusFor(map[string]string{"A": "success"})
	out, err := f.gate.ManualSwitch(context.Background(), "A", false)
	require.NoError(t, err)
	require.True(t, out.Switched)
	assert.NotContains(t, attentionKeys(f.agg), attention.KeyReloadUnverified)
	st := readState(t, f.statePath)
	assert.True(t, st.Verified)
}

func TestReconcileOutcomes(t *testing.T) {
	t.Run("already-current", func(t *testing.T) {
		repo := &fakeRepo{tip: "A", commits: []string{"A"}}
		f := servingFixture(t, repo, "A")
		assert.Equal(t, "already-current", f.gate.Reconcile(context.Background()))
	})
	t.Run("fetch-failed", func(t *testing.T) {
		repo := &fakeRepo{fetchErr: errors.New("down")}
		f := servingFixture(t, repo, "A")
		assert.Equal(t, "fetch-failed", f.gate.Reconcile(context.Background()))
	})
	t.Run("held-blind", func(t *testing.T) {
		repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
		f := servingFixture(t, repo, "A")
		f.gate.status = nil
		assert.Equal(t, "held-blind", f.gate.Reconcile(context.Background()))
	})
	t.Run("held-red", func(t *testing.T) {
		repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
		f := servingFixture(t, repo, "A")
		f.gate.status = statusFor(map[string]string{"B": "failure"})
		assert.Equal(t, "held-red", f.gate.Reconcile(context.Background()))
		assert.Equal(t, "held-red", f.gate.Reconcile(context.Background()), "repeat verdicts report the same outcome")
	})
	t.Run("held-pending", func(t *testing.T) {
		repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
		f := servingFixture(t, repo, "A")
		f.gate.status = statusFor(map[string]string{"B": "pending"})
		assert.Equal(t, "held-pending", f.gate.Reconcile(context.Background()))
	})
	t.Run("switched", func(t *testing.T) {
		repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}
		f := servingFixture(t, repo, "A")
		f.gate.status = statusFor(map[string]string{"B": "success"})
		assert.Equal(t, "switched", f.gate.Reconcile(context.Background()))
		assert.Equal(t, "already-current", f.gate.Reconcile(context.Background()))
	})
}
