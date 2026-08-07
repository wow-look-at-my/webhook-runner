// Startup: how the gate settles the working tree when the process boots,
// before the first hooks load. The other tree-moving paths live beside it --
// the status/push events in gate.go, the reconciliation poll in poll.go, the
// operator's controls in manual.go.
package reloadgate

import (
	"fmt"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

// Startup settles the working tree — called BEFORE the watcher's initial
// scan performs the first hooks load, and it never calls Apply itself. It
// restores the persisted last-good commit, or (first boot / vanished
// commit) serves what is checked out, loudly flagged unverified. Git
// failures degrade to serving the current tree rather than crashing:
// the runner staying up on the old tree IS the design.
func (g *Gate) Startup() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.servingSHA == "" {
		g.startupFreshLocked()
	} else {
		g.startupRestoreLocked()
	}

	// A pending commit that ended up serving is settled.
	if g.pendingSHA != "" && g.pendingSHA == g.servingSHA {
		g.pendingSHA, g.pendingState = "", ""
		g.persistLocked()
	}
	// Re-arm the in-memory attention entries from the settled state (the
	// aggregator is empty after a restart).
	if !g.verified {
		g.attention.Report(attention.Entry{
			Source: attention.SourceReload,
			Key:    attention.KeyReloadUnverified,
			Message: fmt.Sprintf("serving hooks tree %s without a recorded %s green; verifies on its next success (or admin /reload)",
				short(g.servingSHA), g.context),
		})
	}
	if g.pendingSHA != "" {
		g.attention.Report(attention.Entry{
			Source: attention.SourceReload,
			Key:    attention.KeyReloadHeld,
			Message: g.withChecks(fmt.Sprintf("hooks repo %s awaiting %s (last known: %s); serving %s",
				short(g.pendingSHA), g.context, g.pendingStateLocked(), short(g.servingSHA)), g.pendingSHA),
		})
	}
}

// startupFreshLocked handles the first boot with no recorded state: serve
// whatever is checked out (fresh clone = branch tip; upgraded deployment =
// the tree it was already serving), flagged unverified until the first
// green.
func (g *Gate) startupFreshLocked() {
	head, err := g.repo.Head()
	if err != nil {
		// Degrade: stay up on whatever the tree holds; the next status
		// event or admin /reload settles it.
		g.log.Error("reload gate: reading hooks repo HEAD failed", "err", err)
		g.events.Record("reload.failed", "reload gate: reading hooks repo head failed: "+err.Error(), nil)
	}
	g.servingSHA, g.verified = head, false
	g.persistLocked()
	msg := fmt.Sprintf("serving unverified tree %s; no recorded green — will verify on the next %s success (or admin /reload)",
		short(head), g.context)
	g.log.Warn("hooks repo serving unverified tree", "sha", head, "context", g.context)
	g.events.Record("reload.unverified", msg, nil)
}

// startupRestoreLocked puts the tree back at the persisted last-good
// commit, falling to the branch tip (unverified, loud) when that commit is
// no longer reachable.
func (g *Gate) startupRestoreLocked() {
	head, err := g.repo.Head()
	if err != nil {
		g.log.Error("reload gate: reading hooks repo HEAD failed", "err", err)
	}
	if err == nil && head == g.servingSHA {
		return // normal restart: already at the last-good commit
	}
	// The tree is not at the record (dir wiped and re-cloned, or a crash
	// between reset and persist): restore the last-good commit, keeping
	// its verified flag.
	if ferr := g.repo.FetchSHA(g.servingSHA, fetchDepth); ferr == nil {
		if rerr := g.repo.ResetTo(g.servingSHA); rerr == nil {
			g.log.Info("hooks repo restored to last-good commit", "sha", g.servingSHA)
			g.events.Record("reload.restored", "hooks repo restored to last-good "+short(g.servingSHA), nil)
			return
		}
	}
	// The recorded commit is gone (force-push removed it?): fall to the
	// branch tip, loudly unverified.
	lost := g.servingSHA
	tip, terr := g.fetchBranchBounded()
	if terr == nil {
		terr = g.repo.ResetTo(tip)
	}
	if terr != nil {
		// Full git failure: serve whatever the tree holds, and keep the
		// last-good record on disk for the next boot — this boot runs
		// degraded but runs.
		g.log.Error("reload gate: falling back to hooks repo tip failed", "err", terr)
		g.events.Record("reload.failed", "reload gate: falling back to hooks repo tip failed: "+terr.Error(), nil)
		g.servingSHA, g.verified = head, false
		return
	}
	g.servingSHA, g.verified = tip, false
	g.persistLocked()
	msg := fmt.Sprintf("could not restore last-good %s; serving unverified tip %s", short(lost), short(tip))
	g.log.Warn("hooks repo last-good commit not restorable", "lost", lost, "serving", tip)
	g.events.Record("reload.unverified", msg, nil)
}
