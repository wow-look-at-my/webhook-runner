// Package reloadgate holds the hooks-repo working tree behind a CI
// green-gate: the tree only ever moves to a commit GitHub has reported a
// successful gating commit status for (context "all-builds" by default),
// to the operator's explicit admin /reload (Force — the deliberate
// bypass), or to the persisted last-good commit at startup.
//
// A push webhook NEVER moves the tree — it only fetches, records the new
// tip as pending, and says so loudly (a reload.held event plus a
// needs-attention entry); the HMAC-verified `status` event is the switch
// authority. That split is what makes "reload onto a commit with failing
// CI" impossible: the last green commit keeps serving until GitHub itself
// vouches for the next one.
//
// The gate is strictly EVENT-DRIVEN (operator law): no polling, no
// timers, no backoff. A missed green status converges on the repo's next
// delivery, a GitHub redelivery, or an admin force.
package reloadgate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// fetchDepth is how much history every fetch pulls — the ordering window:
// a green status only switches the tree when its sha is within this many
// commits of the freshly-fetched tip and not older than what is already
// serving. Deep enough for any realistic delivery reordering, cheap for a
// hooks repo.
const fetchDepth = 100

// GitRepo is the narrow slice of git the gate needs, implemented by
// *hooks.Repo (an interface so tests can script it).
type GitRepo interface {
	// Head returns the commit the working tree is checked out at.
	Head() (string, error)
	// FetchBranch fetches the tracked branch at the given depth without
	// touching the working tree, and returns the fetched tip.
	FetchBranch(depth int) (tip string, err error)
	// RecentCommits lists up to max commits from the last fetch
	// (FETCH_HEAD), newest first.
	RecentCommits(max int) ([]string, error)
	// ResetTo hard-resets the working tree to an already-fetched sha.
	ResetTo(sha string) error
	// FetchSHA fetches one commit by sha (GitHub serves reachable-sha
	// fetches) — the startup last-good restore path.
	FetchSHA(sha string, depth int) error
}

// Config wires a Gate.
type Config struct {
	Repo GitRepo
	// Branch is the configured tracked branch; empty means the repo's
	// default branch (resolved per delivery from the payload).
	Branch string
	// Context is the gating commit-status context (e.g. "all-builds").
	Context string
	// StatePath is the persisted gate state file
	// (<data-dir>/reload-gate.json).
	StatePath string
	// Apply reloads hooks from the (already reset) working tree — the
	// serve loop's loadAndApply closure.
	Apply     func()
	Events    *events.Recorder
	Attention *attention.Aggregator
	Logger    *slog.Logger
}

// Gate authorizes and serializes hooks-repo working-tree moves.
type Gate struct {
	repo      GitRepo
	branch    string
	context   string
	statePath string
	apply     func()
	events    *events.Recorder
	attention *attention.Aggregator
	log       *slog.Logger

	mu           sync.Mutex
	servingSHA   string
	verified     bool   // a green gating status (or operator force) vouched for servingSHA
	pendingSHA   string // a newer commit fetched but not yet green ("" = none)
	pendingState string // "pending", "failure", or "error"
}

// gateState is the persisted JSON shape at StatePath.
type gateState struct {
	ServingSHA   string    `json:"serving_sha"`
	Verified     bool      `json:"verified"`
	PendingSHA   string    `json:"pending_sha,omitempty"`
	PendingState string    `json:"pending_state,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// New builds a Gate, loading persisted state from cfg.StatePath. A missing
// file is the zero state; a file that exists but does not parse is a hard
// error — booting with the last-good record unreadable would silently
// drop the gate's whole point (the overrides-store rule).
func New(cfg Config) (*Gate, error) {
	if cfg.Repo == nil {
		return nil, errors.New("reloadgate: repo required")
	}
	if cfg.Context == "" {
		return nil, errors.New("reloadgate: gating context required")
	}
	if cfg.StatePath == "" {
		return nil, errors.New("reloadgate: state path required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	g := &Gate{
		repo:      cfg.Repo,
		branch:    cfg.Branch,
		context:   cfg.Context,
		statePath: cfg.StatePath,
		apply:     cfg.Apply,
		events:    cfg.Events,
		attention: cfg.Attention,
		log:       cfg.Logger,
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o700); err != nil {
		return nil, fmt.Errorf("reloadgate: create dir: %w", err)
	}
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return g, nil
		}
		return nil, fmt.Errorf("reloadgate: read %s: %w", cfg.StatePath, err)
	}
	var st gateState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("reloadgate: parse %s (refusing to start with the last-good record unreadable): %w", cfg.StatePath, err)
	}
	g.servingSHA, g.verified = st.ServingSHA, st.Verified
	g.pendingSHA, g.pendingState = st.PendingSHA, st.PendingState
	return g, nil
}

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
			Message: fmt.Sprintf("hooks repo %s awaiting %s (last known: %s); serving %s",
				short(g.pendingSHA), g.context, g.pendingStateLocked(), short(g.servingSHA)),
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
	tip, terr := g.repo.FetchBranch(fetchDepth)
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

// HandleEvent processes one HMAC-verified /_reload delivery. The returned
// status goes into the HTTP response body; a non-nil error means the
// delivery is answered 500 (GitHub records a red, redeliverable delivery).
func (g *Gate) HandleEvent(event string, body []byte) (string, error) {
	switch event {
	case "push":
		return g.handlePush(body)
	case "status":
		return g.handleStatus(body)
	case "ping":
		return "ignored", nil
	default:
		// Never reload on an unrecognized event; admin POST /reload is
		// the manual path.
		return fmt.Sprintf("ignored: unhandled event %q (push records, status switches; admin POST /reload forces)", event), nil
	}
}

// handlePush is bookkeeping + visibility only — a push NEVER moves the
// tree: fetch, and if the tip differs from what is serving, record it
// pending and say so loudly. The status event is the switch authority, so
// a runner that missed a push still converges when the green arrives.
func (g *Gate) handlePush(body []byte) (string, error) {
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		g.events.Record("reload.ignored", "unparseable push payload ignored: "+err.Error(), nil)
		return "ignored", nil
	}
	tracked := g.trackedBranch(p.Repository.DefaultBranch)
	if p.Ref != "" && p.Ref != "refs/heads/"+tracked {
		return "ignored", nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	tip, err := g.repo.FetchBranch(fetchDepth)
	if err != nil {
		g.events.Record("git.pull_failed", "hooks repo fetch failed: "+err.Error(), nil)
		return "", fmt.Errorf("fetch hooks repo: %w", err)
	}
	if tip == g.servingSHA {
		if g.pendingSHA != "" {
			g.pendingSHA, g.pendingState = "", ""
			g.persistLocked()
			g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadHeld)
		}
		return "up-to-date", nil
	}
	g.pendingSHA, g.pendingState = tip, "pending"
	g.persistLocked()
	msg := fmt.Sprintf("hooks repo %s awaiting %s; serving %s", short(tip), g.context, short(g.servingSHA))
	g.log.Info("hooks repo reload held", "pending", tip, "serving", g.servingSHA, "context", g.context)
	g.events.Record("reload.held", msg, nil)
	g.attention.Report(attention.Entry{
		Source:  attention.SourceReload,
		Key:     attention.KeyReloadHeld,
		Message: msg,
	})
	return "held", nil
}

// handleStatus is the switch authority: a success for the gating context,
// on the tracked branch, that passes the ordering rule moves the tree.
func (g *Gate) handleStatus(body []byte) (string, error) {
	var p struct {
		SHA      string `json:"sha"`
		State    string `json:"state"`
		Context  string `json:"context"`
		Branches []struct {
			Name string `json:"name"`
		} `json:"branches"`
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		g.events.Record("reload.ignored", "unparseable status payload ignored: "+err.Error(), nil)
		return "ignored", nil
	}
	// The context filter drops the bulk quietly: every PR head gets the
	// gating context too, plus foreign contexts.
	if p.Context != g.context {
		return "ignored", nil
	}
	if p.SHA == "" || p.State == "" {
		g.events.Record("reload.ignored", "status payload missing sha/state; ignored", nil)
		return "ignored", nil
	}
	// Branch prefilter: a status naming branches that don't include the
	// tracked one is for someone else's head — drop it before any git op.
	// GitHub lists at most 10 branches; an empty list falls through to the
	// ordering rule, which is the real authority anyway.
	tracked := g.trackedBranch(p.Repository.DefaultBranch)
	if len(p.Branches) > 0 {
		found := false
		for _, b := range p.Branches {
			if b.Name == tracked {
				found = true
				break
			}
		}
		if !found {
			return "ignored", nil
		}
	}
	switch p.State {
	case "pending":
		// The hold is already visible from reload.held; a pending gating
		// status adds nothing.
		return "ignored", nil
	case "failure", "error":
		return g.holdRed(p.SHA, p.State)
	case "success":
		return g.trySwitch(p.SHA)
	default:
		return "ignored", nil
	}
}

// holdRed records a red gating status: the pending commit (or a newer
// commit whose push we never saw) failed CI — the tree stays put, loudly.
func (g *Gate) holdRed(sha, state string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha != g.pendingSHA && !(g.pendingSHA == "" && sha != g.servingSHA) {
		// A red for the serving commit (nothing to move), or for some
		// other commit while a different one is pending — neither changes
		// what the gate is waiting on.
		return "ignored", nil
	}
	g.pendingSHA, g.pendingState = sha, state
	g.persistLocked()
	msg := fmt.Sprintf("%s %s for %s; serving %s unchanged", g.context, state, short(sha), short(g.servingSHA))
	g.log.Warn("hooks repo reload held on red ci", "sha", sha, "state", state, "serving", g.servingSHA)
	g.events.Record("reload.held_red", msg, nil)
	g.attention.Report(attention.Entry{
		Source:  attention.SourceReload,
		Key:     attention.KeyReloadHeld,
		Message: msg,
	})
	return "held", nil
}

// trySwitch handles a green gating status: verify a redelivery for the
// serving commit, or — when the sha passes the ordering rule against a
// fresh fetch — reset the tree to it and reload.
func (g *Gate) trySwitch(sha string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha == g.servingSHA {
		if !g.verified {
			g.verified = true
			g.persistLocked()
			msg := fmt.Sprintf("serving tree %s verified (%s green)", short(sha), g.context)
			g.log.Info("hooks repo serving tree verified", "sha", sha, "context", g.context)
			g.events.Record("reload.verified", msg, nil)
			g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadUnverified)
		}
		return "already-serving", nil
	}
	if _, err := g.repo.FetchBranch(fetchDepth); err != nil {
		g.events.Record("git.pull_failed", "hooks repo fetch failed: "+err.Error(), nil)
		return "", fmt.Errorf("fetch hooks repo: %w", err)
	}
	commits, err := g.repo.RecentCommits(fetchDepth)
	if err != nil {
		g.events.Record("reload.failed", "hooks repo history read failed: "+err.Error(), nil)
		return "", fmt.Errorf("list hooks repo history: %w", err)
	}
	// The ordering rule: the sha must be in the freshly-fetched recent
	// history (covers ancient redeliveries, force-push-removed shas, and
	// branch-prefilter leaks) and not older than what is serving (a green
	// for B arriving after we already switched to C).
	posS := indexOf(commits, sha)
	if posS == -1 {
		msg := fmt.Sprintf("green %s for %s not in recent %s history; serving %s unchanged",
			g.context, short(sha), g.branchLabel(), short(g.servingSHA))
		g.log.Warn("stale green gating status ignored", "sha", sha, "serving", g.servingSHA)
		g.events.Record("reload.ignored_stale", msg, nil)
		return "ignored-stale", nil
	}
	if posServing := indexOf(commits, g.servingSHA); posServing != -1 && posServing < posS {
		msg := fmt.Sprintf("green %s for %s is older than serving %s; unchanged", g.context, short(sha), short(g.servingSHA))
		g.log.Warn("out-of-order green gating status ignored", "sha", sha, "serving", g.servingSHA)
		g.events.Record("reload.ignored_stale", msg, nil)
		return "ignored-stale", nil
	}
	if err := g.repo.ResetTo(sha); err != nil {
		g.events.Record("reload.failed", "hooks repo reset to "+short(sha)+" failed: "+err.Error(), nil)
		return "", fmt.Errorf("reset hooks repo to %s: %w", sha, err)
	}
	g.servingSHA, g.verified = sha, true
	// Pending bookkeeping: switching to the pending commit (or past it)
	// clears the hold; a green for an INTERMEDIATE commit keeps the newer
	// pending held — and says so.
	if pos := indexOf(commits, g.pendingSHA); g.pendingSHA != "" && g.pendingSHA != sha && pos != -1 && pos < posS {
		g.attention.Report(attention.Entry{
			Source:  attention.SourceReload,
			Key:     attention.KeyReloadHeld,
			Message: fmt.Sprintf("switched to %s; still awaiting %s for %s", short(sha), g.context, short(g.pendingSHA)),
		})
	} else {
		g.pendingSHA, g.pendingState = "", ""
		g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadHeld)
	}
	g.persistLocked()
	msg := fmt.Sprintf("hooks repo switched to %s (%s green)", short(sha), g.context)
	g.log.Info("hooks repo switched", "sha", sha, "context", g.context)
	g.events.Record("reload.switched", msg, nil)
	g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadUnverified)
	if g.apply != nil {
		g.apply()
	}
	return "reloaded", nil
}

// Force is the operator's manual bypass (admin POST /reload): fetch, jump
// to the remote tip, and record it verified — the operator vouched.
// Deliberately loud about skipping the gate.
func (g *Gate) Force() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	tip, err := g.repo.FetchBranch(fetchDepth)
	if err != nil {
		g.events.Record("git.pull_failed", "hooks repo fetch failed: "+err.Error(), nil)
		return fmt.Errorf("fetch hooks repo: %w", err)
	}
	if err := g.repo.ResetTo(tip); err != nil {
		g.events.Record("reload.failed", "hooks repo reset to "+short(tip)+" failed: "+err.Error(), nil)
		return fmt.Errorf("reset hooks repo to %s: %w", tip, err)
	}
	g.servingSHA, g.verified = tip, true
	g.pendingSHA, g.pendingState = "", ""
	g.persistLocked()
	g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadHeld)
	g.attention.Resolve(attention.SourceReload, "", attention.KeyReloadUnverified)
	msg := fmt.Sprintf("operator forced switch to %s, bypassing ci gate", short(tip))
	g.log.Warn("hooks repo force-switched, ci gate bypassed", "sha", tip)
	g.events.Record("reload.forced", msg, nil)
	if g.apply != nil {
		g.apply()
	}
	return nil
}

// trackedBranch resolves the branch the gate tracks: the configured one
// when set, else the delivery payload's repository.default_branch.
func (g *Gate) trackedBranch(payloadDefault string) string {
	if g.branch != "" {
		return g.branch
	}
	return payloadDefault
}

// branchLabel names the tracked branch in feed messages.
func (g *Gate) branchLabel() string {
	if g.branch != "" {
		return g.branch
	}
	return "branch"
}

// pendingStateLocked returns the recorded pending CI state, defaulting to
// "pending"; caller holds g.mu.
func (g *Gate) pendingStateLocked() string {
	if g.pendingState == "" {
		return "pending"
	}
	return g.pendingState
}

// persistLocked atomically rewrites the state file (temp+rename, the
// overrides pattern); caller holds g.mu. A persist failure must not take
// reloads down, so the in-memory state stands and the failure is loud
// (log + event) — the file is a restart restore record, not the live
// authority.
func (g *Gate) persistLocked() {
	st := gateState{
		ServingSHA:   g.servingSHA,
		Verified:     g.verified,
		PendingSHA:   g.pendingSHA,
		PendingState: g.pendingState,
		UpdatedAt:    time.Now().UTC(),
	}
	if err := writeState(g.statePath, st); err != nil {
		g.log.Error("reload gate state persist failed", "path", g.statePath, "err", err)
		g.events.Record("reload.state_write_failed",
			"reload gate state persist failed (in-memory state continues; a restart loses it): "+err.Error(), nil)
	}
}

func writeState(path string, st gateState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reload-gate.json.tmp-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func indexOf(list []string, s string) int {
	if s == "" {
		return -1
	}
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// short abbreviates a sha for feed messages.
func short(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
