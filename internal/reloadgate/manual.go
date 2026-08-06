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

// ciStateNotProbed is the CI state of an OVERRIDDEN switch: we deliberately
// did not ask GitHub, because the answer changes nothing once the operator
// has overridden and the probe is a network call that hangs when GitHub is
// the thing that is broken. It is NOT "unknown" (a probe that failed) and
// certainly not "success" — the surfaces must never show a green nobody saw.
const ciStateNotProbed = "not-probed (override)"

// manualFetchTimeout bounds the fetch the manual switch may need when a ref
// is not local yet. A `git fetch` against a degraded GitHub HANGS rather
// than fails, and this call holds the gate mutex — so an unbounded one takes
// the whole reload panel down with it (a Cloudflare 524 on /reload/switch
// and /reload/status timing out behind the same lock). The operator gets an
// answer either way; a switch to an already-local commit never waits at all.
const manualFetchTimeout = 20 * time.Second

// fetchBranchBounded fetches with manualFetchTimeout, killing the git process
// when the remote will not answer.
func (g *Gate) fetchBranchBounded() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), manualFetchTimeout)
	defer cancel()
	return g.repo.FetchBranchContext(ctx, fetchDepth)
}

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
	// Reasons names everything about this pick the gate would have refused
	// on (empty = a clean green pick). An override always contributes at
	// least the un-probed CI state, so a Switched outcome with reasons is
	// exactly an overridden one.
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
//   - Without override the commit's gating CI state and src-layout tree
//     marker are evaluated. Green AND src present: switch, recorded as an
//     ordinary reload.switched (the operator picked a vouched commit).
//     Anything else — a red/pending/absent/UNREADABLE CI state, or a tree
//     missing src/hooks — is refused (Switched=false, every failed check
//     in Reasons; nothing moves).
//   - With override=true the CI probe is SKIPPED (see ciStateNotProbed)
//     and the switch happens regardless, loudly: a reload.forced event
//     names the un-probed state and any other reason.
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

	// LOCAL FIRST. This endpoint is the escape hatch for a wedged gate, and a
	// wedged gate is usually GitHub being down — so it must not begin with a
	// network round trip. It used to fetch unconditionally, holding g.mu
	// across a `git fetch` that HANGS (not fails) when GitHub is degraded:
	// the request never answered, Cloudflare 524'd at 100s, /reload/status
	// timed out behind the same mutex, and the operator was left with a
	// dashboard that could not show the problem or fix it. The commit an
	// operator forces is nearly always already local — the push webhook
	// fetched it when it recorded the hold — so resolve first and reach for
	// the network only when that fails.
	sha, resolveErr := g.repo.ResolveRef(ref)

	// THEN freshen, on a leash. The fetch still runs — the tip is what tells
	// us whether this switch settles the pending hold — but it can no longer
	// decide whether the switch happens at all, and it cannot run forever.
	tip, tipErr := g.fetchBranchBounded()
	if tipErr != nil {
		g.log.Warn("manual switch: hooks repo fetch failed; continuing with local objects", "err", tipErr)
		g.events.Record("git.pull_failed", "manual switch: hooks repo fetch failed (continuing with local objects): "+tipErr.Error(), nil)
	}
	if resolveErr != nil {
		// Only a ref we could NOT resolve locally depends on that fetch.
		if sha, resolveErr = g.repo.ResolveRef(ref); resolveErr != nil {
			return SwitchOutcome{}, fmt.Errorf("%w: %q: %v", ErrUnknownRef, ref, resolveErr)
		}
	}

	out := SwitchOutcome{
		SHA:    sha,
		HasSrc: g.repo.TreeHasDir(sha, SrcMarkerDir),
	}
	// The CI probe is a GitHub call, and under override its answer changes
	// NOTHING — the operator has already decided. Skipping it keeps the force
	// path working when GitHub is the thing that is broken, which is exactly
	// when it gets used. Without override the probe still runs: that is the
	// gate doing its job.
	if override {
		out.CIState = ciStateNotProbed
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("%s state for %s was not probed (operator override)", g.context, short(sha)))
	} else {
		out.CIState = g.CIState(ctx, sha)
		if out.CIState != "success" {
			out.Reasons = append(out.Reasons,
				fmt.Sprintf("%s state for %s is %q, not success", g.context, short(sha), out.CIState))
		}
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
	// An override is ALWAYS recorded as forced: we never asked GitHub for the
	// CI state, so "green" is something we do not know, and recording it as an
	// ordinary switch would put a green nobody saw into the audit trail.
	if override {
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
