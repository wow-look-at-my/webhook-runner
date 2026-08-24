package reloadgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
)

// StatusFunc reads the gating commit-status context's current state for sha from the GitHub API: "success", "pending", "failure", or.
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
//
// The returned outcome names what the pass did — "fetch-failed",
// "already-current", "held-blind", "held-red", "held-pending",
// "ignored-stale", "switched", or "switch-failed" — for on-demand callers
// (the admin re-evaluate endpoint); the timer poller ignores it.
func (g *Gate) Reconcile(ctx context.Context) string {
	g.mu.Lock()
	serving := g.servingSHA
	g.mu.Unlock()

	tip, err := g.fetchBranchBounded()
	if err != nil {
		// Learned nothing; the next tick (or the next delivery) retries.
		g.log.Warn("reload poll: hooks repo fetch failed", "err", err)
		g.events.Record("git.pull_failed", "reload poll: hooks repo fetch failed: "+err.Error(), nil)
		return "fetch-failed"
	}
	if tip == serving {
		// Nothing newer — done, with NO status API call.
		g.mu.Lock()
		if tip == g.servingSHA { // re-check under the lock (an event may have switched)
			if g.pendingSHA != "" {
				g.pendingSHA, g.pendingState = "", ""
				g.persistLocked()
			}
			g.resolveHoldEntriesLocked()
		}
		g.mu.Unlock()
		return "already-current"
	}

	// The tip moved past what is serving: the poll must determine the gating status before anything can switch.
	state, err := g.readGatingState(ctx, tip)
	if err != nil {
		g.holdBlind(tip, err.Error())
		return "held-blind"
	}

	// Determined: whatever blindness there was is over. What follows re-reports whichever hold (if any) applies.
	g.mu.Lock()
	g.lastPollBlind = ""
	g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadPoll)
	g.mu.Unlock()

	switch state {
	case "success":
		// The exact switch path the status event uses — trySwitch's own fresh fetch + ordering rules re-validate everything under the gate's lock, so.
		status, err := g.trySwitch(tip)
		if err != nil {
			g.log.Error("reload poll: switch failed", "sha", tip, "err", err)
			return "switch-failed"
		}
		switch status {
		case "reloaded":
			return "switched"
		case "already-serving":
			return "already-current"
		default: // "ignored-stale"
			return status
		}
	case "failure", "error":
		if g.alreadyHeld(tip, state) {
			return "held-red"
		}
		// The tip's push webhook may have been missed too, leaving an older (or no) commit recorded pending: quietly point the hold at the tip first —.
		g.mu.Lock()
		g.pendingSHA = tip
		g.mu.Unlock()
		if _, err := g.holdRed(tip, state); err != nil {
			g.log.Error("reload poll: hold failed", "sha", tip, "err", err)
		}
		return "held-red"
	default:
		// "pending", "" (no status reported for the context yet), or an unrecognized state: not affirmatively green — hold, exactly as a push.
		if g.alreadyHeld(tip, "pending") {
			return "held-pending"
		}
		g.mu.Lock()
		g.recordPendingLocked(tip)
		g.mu.Unlock()
		return "held-pending"
	}
}

// readGatingState determines the tip's gating state for the poll. The API is
// asked FIRST and its answer always wins: the poll exists to catch what the
// status webhook missed, so a recorded verdict must never mask a fresher read
// (a CI re-run flipping green->red whose status event was lost is exactly that
// case). Only when the API cannot answer at all — no reader configured, or the
// call failed — does a previously delivered verdict stand in, which is what
// makes the credential an optimization rather than a hard dependency for any
// sha the gate has already been told about. Neither path switches the tree on
// its own: trySwitch's ordering rule still gates every apply.
func (g *Gate) readGatingState(ctx context.Context, tip string) (string, error) {
	apiErr := errors.New("no status reader configured")
	if g.status != nil {
		state, err := g.status(ctx, tip)
		if err == nil {
			return state, nil
		}
		apiErr = err
	}
	state, ok := g.verdictFor(tip)
	if !ok {
		return "", apiErr
	}
	g.log.Info("reload poll: using the recorded gating verdict (status API unreadable)",
		"sha", tip, "state", state, "context", g.context, "api_err", apiErr)
	g.events.Record("reload.poll_recorded", fmt.Sprintf(
		"reload poll: %s status for %s could not be read (%s); using the %q verdict recorded from that commit's own status delivery",
		g.context, short(tip), apiErr, state), nil)
	return state, nil
}

// alreadyHeld reports whether the hold for sha with this state is already recorded — the poll's repeat-tick dedupe (an unchanged verdict every tick.
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
