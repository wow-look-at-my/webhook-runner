package reloadgate

import (
	"context"
	"fmt"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

// StatusFunc reads the gating commit-status context's current state for
// sha from the GitHub API: "success", "pending", "failure", or "error".
// An empty state with a nil error means the context has no status on the
// commit yet (CI still starting, or the aggregator never posted one). A
// non-nil error means the state could not be determined at all (no token
// configured, API failure) — the gate fails closed on it.
type StatusFunc func(ctx context.Context, sha string) (state string, err error)

// Reconcile is one reconciliation-poll pass — the fallback that keeps a
// missed status webhook from freezing deploys indefinitely. It fetches
// the tracked branch's remote tip; a tip equal to the serving commit is a
// quiet no-op (no API call). A newer tip has its gating status read via
// the configured StatusFunc: an affirmative green switches through the
// EXACT same ordering-checked path the HMAC-verified status event uses
// (trySwitch — fresh fetch, recent-history membership, not older than
// serving); anything else HOLDS, loudly — red and pending through the
// same bookkeeping the event path records, an unreadable status through
// the poll's own needs-attention entry (KeyReloadPoll). It can never
// switch to a commit whose gating context is not affirmatively green.
//
// Repeat ticks over an unchanged verdict are quiet: the persistent
// needs-attention entries are the surface, and the feed gets one event
// per verdict change, not one per hour.
func (g *Gate) Reconcile(ctx context.Context) {
	g.mu.Lock()
	serving := g.servingSHA
	g.mu.Unlock()

	tip, err := g.repo.FetchBranch(fetchDepth)
	if err != nil {
		// Learned nothing; the next tick (or the next delivery) retries.
		g.log.Warn("reload poll: hooks repo fetch failed", "err", err)
		g.events.Record("git.pull_failed", "reload poll: hooks repo fetch failed: "+err.Error(), nil)
		return
	}
	if tip == serving {
		// Nothing newer — done, with NO status API call. Mirror the push
		// handler's up-to-date bookkeeping so a stale pending record
		// (origin rewound back to serving) clears here too.
		g.mu.Lock()
		if tip == g.servingSHA { // re-check under the lock (an event may have switched)
			if g.pendingSHA != "" {
				g.pendingSHA, g.pendingState = "", ""
				g.persistLocked()
			}
			g.resolveHoldEntriesLocked()
		}
		g.mu.Unlock()
		return
	}

	// The tip moved past what is serving: the poll must determine the
	// gating status before anything can switch. Fail closed — an
	// unreadable status HOLDS, loudly.
	if g.status == nil {
		g.holdBlind(tip, "no status reader configured")
		return
	}
	state, err := g.status(ctx, tip)
	if err != nil {
		g.holdBlind(tip, err.Error())
		return
	}

	// Determined: whatever blindness there was is over. What follows
	// re-reports whichever hold (if any) applies.
	g.mu.Lock()
	g.lastPollBlind = ""
	g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadPoll)
	g.mu.Unlock()

	switch state {
	case "success":
		// The exact switch path the status event uses — trySwitch's own
		// fresh fetch + ordering rules re-validate everything under the
		// gate's lock, so a racing event delivery can never be trampled.
		if _, err := g.trySwitch(tip); err != nil {
			g.log.Error("reload poll: switch failed", "sha", tip, "err", err)
		}
	case "failure", "error":
		if g.alreadyHeld(tip, state) {
			return
		}
		// The tip's push webhook may have been missed too, leaving an
		// older (or no) commit recorded pending: quietly point the hold at
		// the tip first — what that push delivery would have recorded —
		// then record its red state through the event path's own holdRed.
		g.mu.Lock()
		g.pendingSHA = tip
		g.mu.Unlock()
		if _, err := g.holdRed(tip, state); err != nil {
			g.log.Error("reload poll: hold failed", "sha", tip, "err", err)
		}
	default:
		// "pending", "" (no status reported for the context yet), or an
		// unrecognized state: not affirmatively green — hold, exactly as a
		// push delivery records a not-yet-green tip.
		if g.alreadyHeld(tip, "pending") {
			return
		}
		g.mu.Lock()
		g.recordPendingLocked(tip)
		g.mu.Unlock()
	}
}

// alreadyHeld reports whether the hold for sha with this state is already
// recorded — the poll's repeat-tick dedupe (an unchanged verdict every
// tick must not spam the feed or rewrite persisted state).
func (g *Gate) alreadyHeld(sha, state string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pendingSHA == sha && g.pendingStateLocked() == state
}

// holdBlind records that the poll found a newer tip whose gating status
// could not be read — the tree stays put (fail closed), loudly: the
// persistent needs-attention entry (KeyReloadPoll) plus one
// reload.poll_blind event per distinct problem, not per hourly tick.
func (g *Gate) holdBlind(tip, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	msg := fmt.Sprintf("reload poll: hooks repo %s awaits %s but its status could not be read (%s); serving %s — fix the credential, redeliver the status event, or force via admin /reload",
		short(tip), g.context, reason, short(g.servingSHA))
	if msg != g.lastPollBlind {
		g.lastPollBlind = msg
		g.log.Warn("reload poll cannot determine gating status", "pending", tip, "reason", reason)
		g.events.Record("reload.poll_blind", msg, nil)
	}
	g.attention.Report(attention.Entry{
		Source:  attention.SourceReload,
		Key:     attention.KeyReloadPoll,
		Message: msg,
	})
}
