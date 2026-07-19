package reloadgate

// The operator-facing manual controls behind the admin dashboard's reload
// panel: a state snapshot (Status), a best-effort CI probe (CIState), and
// the informed manual commit switch (ManualSwitch). ManualSwitch is
// explicit operator authority — the documented escape hatch for a wedged
// gate (CI unreadable, statuses lost, a broken tip to roll back from) — so
// it routes through the SAME Force-style apply path admin /reload uses
// (forceApplyLocked: no staleness ordering, so rollback to an older commit
// works), never through trySwitch, and never silently: a pick that fails
// the gate's checks is refused without an explicit override, and an
// override is recorded loudly with every reason named. The automatic paths
// (status events, the reconciliation poll) are untouched: they still switch
// only on an affirmative green through trySwitch's ordering rules.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SrcMarkerDir is the tree path whose presence marks a commit as holding
// the src hooks layout (internal/hooks.DetectLayout's rule). A hooks-repo
// commit without it would load zero hooks on this fleet — the exact broken
// tree the manual switch must warn about.
const SrcMarkerDir = "src/hooks"

// manualCILookupTimeout bounds each best-effort CI status read: the panel
// (and a manual switch under a wedged gate) must answer promptly, with
// "unknown", rather than hang on GitHub.
const manualCILookupTimeout = 5 * time.Second

// ErrUnknownRef marks a manual switch whose ref could not be resolved to a
// commit — a caller input problem (HTTP 400), not a gate failure.
var ErrUnknownRef = errors.New("cannot resolve ref")

// GateStatus is a point-in-time snapshot of the gate's bookkeeping for the
// admin reload panel.
type GateStatus struct {
	// ServingSHA is the commit the working tree serves ("" before the
	// first Startup on a broken clone).
	ServingSHA string
	// Verified reports whether a green gating status (or operator force)
	// vouched for ServingSHA.
	Verified bool
	// PendingSHA is a newer fetched commit not yet green ("" = none), with
	// its last known CI state in PendingState ("pending"/"failure"/"error").
	PendingSHA   string
	PendingState string
	// Branch is the configured tracked branch ("" = the repo default).
	Branch string
	// Context is the gating commit-status context (e.g. "all-builds").
	Context string
}

// Status returns the gate's current bookkeeping.
func (g *Gate) Status() GateStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := GateStatus{
		ServingSHA: g.servingSHA,
		Verified:   g.verified,
		PendingSHA: g.pendingSHA,
		Branch:     g.branch,
		Context:    g.context,
	}
	if g.pendingSHA != "" {
		st.PendingState = g.pendingStateLocked()
	}
	return st
}

// CIState reads the gating context's current state for sha, best-effort
// and bounded: "success", "pending", "failure", or "error" straight from
// the reader; "none" when the reader determinately reports no status for
// the context yet; "unknown" when the state could not be read at all (no
// reader configured, API failure, timeout) — never a guess.
func (g *Gate) CIState(ctx context.Context, sha string) string {
	if g.status == nil || sha == "" {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, manualCILookupTimeout)
	defer cancel()
	state, err := g.status(ctx, sha)
	if err != nil {
		return "unknown"
	}
	if state == "" {
		return "none"
	}
	return state
}

// SwitchOutcome reports a ManualSwitch decision.
type SwitchOutcome struct {
	// SHA is the resolved target commit.
	SHA string
	// CIState is the gating context's state for SHA at decision time
	// (CIState's vocabulary; "unknown" counts as not green).
	CIState string
	// HasSrc reports whether SHA's tree contains SrcMarkerDir.
	HasSrc bool
	// Switched is true when the tree moved to SHA. False with a non-empty
	// Reasons means the switch was REFUSED pending an explicit override.
	Switched bool
	// Reasons names every gate check the commit fails (empty = a clean
	// green pick). On a Switched outcome a non-empty Reasons means the
	// operator overrode them.
	Reasons []string
}

// Overridden reports whether a completed switch went through over failed
// gate checks.
func (o SwitchOutcome) Overridden() bool {
	return o.Switched && len(o.Reasons) > 0
}

// ManualSwitch moves the hooks tree to an operator-picked ref (sha or
// branch/tag name), through the Force-style apply path — see the package
// comment above. Semantics:
//
//   - The ref is resolved (fetching from origin as needed); an
//     unresolvable ref errors with ErrUnknownRef and mutates nothing.
//   - The commit's gating CI state and src-layout tree marker are
//     evaluated. Green AND src present: switch, recorded as an ordinary
//     reload.switched (the operator picked a vouched commit).
//   - Anything else — a red/pending/absent/UNREADABLE CI state, or a tree
//     missing src/hooks — is refused without override (Switched=false,
//     every failed check in Reasons; nothing moves), and with
//     override=true switches anyway, loudly: a reload.forced event names
//     the override and each reason.
//
// Rollback to a commit OLDER than the serving one is deliberately allowed
// (no trySwitch staleness ordering — that rule exists to order automatic
// status deliveries, not the operator). Pending bookkeeping stays
// consistent: switching to the pending commit (or to the fetched tip)
// clears the pending record and its hold entries; switching elsewhere
// leaves an existing hold for a DIFFERENT commit visible — a newer commit
// is still awaiting its green.
func (g *Gate) ManualSwitch(ctx context.Context, ref string, override bool) (SwitchOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Freshen the tracked branch first (best-effort): a just-pushed ref
	// should resolve, and the tip comparison below wants current knowledge.
	// A fetch failure must NOT block the switch — rolling back to an
	// already-local commit while origin is unreachable is a supported
	// escape hatch.
	tip, tipErr := g.repo.FetchBranch(fetchDepth)
	if tipErr != nil {
		g.log.Warn("manual switch: hooks repo fetch failed; continuing with local objects", "err", tipErr)
		g.events.Record("git.pull_failed", "manual switch: hooks repo fetch failed (continuing with local objects): "+tipErr.Error(), nil)
	}

	sha, err := g.repo.ResolveRef(ref)
	if err != nil {
		return SwitchOutcome{}, fmt.Errorf("%w: %q: %v", ErrUnknownRef, ref, err)
	}

	out := SwitchOutcome{
		SHA:     sha,
		CIState: g.CIState(ctx, sha),
		HasSrc:  g.repo.TreeHasDir(sha, SrcMarkerDir),
	}
	if out.CIState != "success" {
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("%s state for %s is %q, not success", g.context, short(sha), out.CIState))
	}
	if !out.HasSrc {
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("commit %s has no %s directory in its tree — reloading from it would load zero hooks", short(sha), SrcMarkerDir))
	}

	if len(out.Reasons) > 0 && !override {
		msg := fmt.Sprintf("manual switch to %s refused (no override): %s", short(sha), strings.Join(out.Reasons, "; "))
		g.log.Info("manual hooks-repo switch refused", "sha", sha, "reasons", strings.Join(out.Reasons, "; "))
		g.events.Record("reload.switch_refused", msg, nil)
		return out, nil
	}

	// Switching to the pending commit (or the current tip) settles the
	// hold; a pick elsewhere keeps an existing hold for that OTHER commit
	// visible.
	clearPending := g.pendingSHA == sha || (tipErr == nil && sha == tip)
	kind := "reload.switched"
	msg := fmt.Sprintf("hooks repo manually switched to %s (operator pick; %s green)", short(sha), g.context)
	logMsg := "hooks repo manually switched"
	if len(out.Reasons) > 0 {
		kind = "reload.forced"
		msg = fmt.Sprintf("operator forced manual switch to %s, OVERRIDING the reload gate: %s",
			short(sha), strings.Join(out.Reasons, "; "))
		logMsg = "hooks repo manually switched, reload gate OVERRIDDEN"
	}
	if err := g.forceApplyLocked(sha, clearPending, kind, msg); err != nil {
		return out, err
	}
	out.Switched = true
	g.log.Warn(logMsg, "sha", sha, "ci_state", out.CIState, "has_src", out.HasSrc, "overridden", len(out.Reasons) > 0)
	return out, nil
}
