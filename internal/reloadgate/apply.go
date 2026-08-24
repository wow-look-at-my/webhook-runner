// The APPLY half of the gate: turning a recorded switch into a reloaded
// fleet, and undoing it when the fleet refuses.
//
// The gate is the only component that knows which commit was serving a
// moment ago, which is what makes it the right owner of the rollback. A tree
// carrying a manifest field the running binary does not know fails to load,
// and internal/cli.buildLoadAndApply applies NOTHING rather than a fraction
// of it -- so the gate resets the working tree to the previously-serving
// commit, re-applies THAT, and keeps its old serving record. The deploy is
// held, loudly, and the fleet keeps running.
package reloadgate

import (
	"fmt"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

// forceApplyLocked is the ONE Force-style apply path — reset the tree to
// sha, record it serving + verified (the operator, or a green the operator
// picked, vouched), persist, record the event, and reload. Force and both
// ManualSwitch outcomes go through it; unlike trySwitch it applies NO
// staleness ordering, which is exactly what makes operator rollback to an
// older commit possible. clearPending drops the pending record and its
// hold entries (switching to the pending commit itself always clears it —
// nothing is awaited anymore). Caller holds g.mu.
func (g *Gate) forceApplyLocked(sha string, clearPending bool, eventKind, eventMsg string) error {
	prevSHA, prevVerified := g.servingSHA, g.verified
	if err := g.repo.ResetTo(sha); err != nil {
		g.events.Record("reload.failed", "hooks repo reset to "+short(sha)+" failed: "+err.Error(), nil)
		return fmt.Errorf("reset hooks repo to %s: %w", sha, err)
	}
	g.servingSHA, g.verified = sha, true
	if clearPending || g.pendingSHA == sha {
		g.pendingSHA, g.pendingState = "", ""
	}
	g.persistLocked()
	if g.pendingSHA == "" {
		g.resolveHoldEntriesLocked()
	}
	g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadUnverified)
	g.events.Record(eventKind, eventMsg, nil)
	return g.applyOrRollbackLocked(sha, prevSHA, prevVerified)
}

// applyOrRollbackLocked runs the reload and, if the new tree is REFUSED,
// puts the working tree back where it was and re-applies THAT — so a tree
// the running binary cannot load leaves the fleet exactly as it was rather
// than half-swapped.
//
// This is the rollback the gate always had the parts for and never used:
// `apply` could not fail, so a refused tree was recorded as serving and the
// fleet silently lost every entity the binary could not parse. The gate is
// the right owner because it is the only thing that knows which commit was
// serving a moment ago.
//
// A rollback that ITSELF fails to apply is the one genuinely unrecoverable
// case (the previous tree no longer loads either, so there is nothing good
// to serve); it is reported at full volume and the fleet is left with
// whatever the previous apply had installed, which is the last known-good
// registry — nothing is torn down on this path.
//
// Caller holds g.mu. prevSHA == "" means nothing was recorded as serving
// (a first switch); there is nowhere to roll back to, so the refusal is
// simply reported.
func (g *Gate) applyOrRollbackLocked(sha, prevSHA string, prevVerified bool) error {
	if g.apply == nil {
		return nil
	}
	applyErr := g.apply()
	if applyErr == nil {
		return nil
	}
	msg := fmt.Sprintf("hooks tree %s was REFUSED by this binary (%s)", short(sha), applyErr.Error())
	g.log.Error("hooks tree refused", "sha", sha, "err", applyErr)

	if prevSHA == "" || prevSHA == sha {
		g.events.Record("reload.refused", msg+" — no previously-serving commit to roll back to", nil)
		return fmt.Errorf("hooks tree %s refused: %w", short(sha), applyErr)
	}

	if err := g.repo.ResetTo(prevSHA); err != nil {
		g.events.Record("reload.refused",
			msg+fmt.Sprintf("; rolling back to %s ALSO failed: %s", short(prevSHA), err.Error()), nil)
		return fmt.Errorf("hooks tree %s refused (%w) and rollback to %s failed: %w", short(sha), applyErr, short(prevSHA), err)
	}
	g.servingSHA, g.verified = prevSHA, prevVerified
	g.persistLocked()
	if reErr := g.apply(); reErr != nil {
		g.events.Record("reload.refused",
			msg+fmt.Sprintf("; rolled the tree back to %s, which does not load either: %s", short(prevSHA), reErr.Error()), nil)
		return fmt.Errorf("hooks tree %s refused (%w); rolled back to %s which also failed: %w", short(sha), applyErr, short(prevSHA), reErr)
	}
	g.events.Record("reload.refused",
		msg+fmt.Sprintf(" — rolled back to %s, which is serving. The deploy is HELD, not half-applied.", short(prevSHA)), nil)
	return fmt.Errorf("hooks tree %s refused, rolled back to %s: %w", short(sha), short(prevSHA), applyErr)
}
