package server

// The admin dashboard's hooks-repo reload panel: which commit is live,
// what the reload gate is holding, the recent origin history with CI +
// src-layout verdicts, an on-demand re-evaluation, and the operator's
// manual commit switch (server-enforced informed override — a pick that
// fails the gate's checks answers 409 with every reason and switches only
// when the request explicitly carries override:true; the gate records the
// override loudly). Admin-port only, no auth by design (Zero Trust fronts
// the port, the same trust model as POST /reload).
//
// The GET endpoints are READ-ONLY and cheap by construction: /reload/status
// never touches the network beyond one (cached) CI lookup, and only
// /reload/commits — an explicit operator navigation — fetches the remote.
// The automatic reload paths (status events, the reconciliation poll) are
// completely untouched by this file.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
)

// ReloadRepo is the narrow read/inspect slice of the hooks-repo clone the
// admin reload panel uses (implemented by *hooks.Repo). Everything is
// working-tree-safe: fetches and object inspection only, never a checkout.
type ReloadRepo interface {
	Head() (string, error)
	FetchBranch(depth int) (tip string, err error)
	RecentCommits(max int) ([]string, error)
	CommitInfo(sha string) (subject string, date time.Time, err error)
	TreeHasDir(sha, path string) bool
}

// ReloadControl is the reload gate's manual-control surface (implemented
// by *reloadgate.Gate). nil = the CI gate is disabled (legacy mode: any
// reload pulls to tip; there is no per-commit switch authority).
type ReloadControl interface {
	Status() reloadgate.GateStatus
	Reconcile(ctx context.Context) string
	ManualSwitch(ctx context.Context, ref string, override bool) (reloadgate.SwitchOutcome, error)
	CIState(ctx context.Context, sha string) string
}

const (
	// reloadCommitsMax is how much origin history GET /reload/commits lists.
	reloadCommitsMax = 20
	// reloadCommitsFetchDepth matches the gate's ordering window.
	reloadCommitsFetchDepth = 100
	// reloadCIBudget bounds the whole CI-enrichment pass of one commits
	// listing; commits past the budget report "unknown" instead of waiting.
	reloadCIBudget = 15 * time.Second
	// CI verdict cache TTLs: terminal states are stable (a re-run can still
	// flip them, so not forever), live states go stale fast.
	reloadCITerminalTTL = 5 * time.Minute
	reloadCILiveTTL     = 20 * time.Second
)

// reloadCommitJSON is one commit as the panel renders it.
type reloadCommitJSON struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Date    string `json:"date,omitempty"` // RFC3339 committer date
	CIState string `json:"ci_state"`
	HasSrc  bool   `json:"has_src"`
	IsLive  bool   `json:"is_live,omitempty"`
}

type ciCacheEntry struct {
	state string
	at    time.Time
}

// reloadCIState reads (with a small cache) the gating context's state for
// sha: the gate's own CIState vocabulary — "unknown" whenever it cannot be
// read, never a guess. "unknown" itself is cached only on the live TTL so
// a recovered credential is picked up quickly.
func (s *Server) reloadCIState(ctx context.Context, sha string) string {
	if s.reloadControl == nil || sha == "" {
		return "unknown"
	}
	now := time.Now()
	s.ciMu.Lock()
	if e, ok := s.ciCache[sha]; ok {
		ttl := reloadCILiveTTL
		switch e.state {
		case "success", "failure", "error":
			ttl = reloadCITerminalTTL
		}
		if now.Sub(e.at) < ttl {
			s.ciMu.Unlock()
			return e.state
		}
	}
	s.ciMu.Unlock()

	state := s.reloadControl.CIState(ctx, sha)

	s.ciMu.Lock()
	if s.ciCache == nil {
		s.ciCache = map[string]ciCacheEntry{}
	}
	s.ciCache[sha] = ciCacheEntry{state: state, at: now}
	// The panel's working set is tiny (live + pending + one listing); a cap
	// keeps a long-lived process from accreting every sha it ever saw.
	if len(s.ciCache) > 4*reloadCommitsMax {
		clear(s.ciCache)
		s.ciCache[sha] = ciCacheEntry{state: state, at: now}
	}
	s.ciMu.Unlock()
	return state
}

// describeReloadCommit assembles one commit view: subject/date from local
// git objects, the src-layout tree probe, and the (cached, best-effort) CI
// state.
func (s *Server) describeReloadCommit(ctx context.Context, sha, liveSHA string) reloadCommitJSON {
	c := reloadCommitJSON{
		SHA:     sha,
		Short:   shortSHA(sha),
		CIState: s.reloadCIState(ctx, sha),
		IsLive:  sha != "" && sha == liveSHA,
	}
	if s.reloadRepo != nil && sha != "" {
		c.HasSrc = s.reloadRepo.TreeHasDir(sha, reloadgate.SrcMarkerDir)
	}
	s.enrichCommitInfo(&c)
	return c
}

// enrichCommitInfo fills a commit view's subject/date from local git
// objects, best-effort (an unknown commit just stays unenriched). Shared
// by the listing/status views and the switch response.
func (s *Server) enrichCommitInfo(c *reloadCommitJSON) {
	if s.reloadRepo == nil || c.SHA == "" {
		return
	}
	if subject, date, err := s.reloadRepo.CommitInfo(c.SHA); err == nil {
		c.Subject = subject
		if !date.IsZero() {
			c.Date = date.UTC().Format(time.RFC3339)
		}
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// handleReloadStatus is GET /reload/status (admin port): the reload
// panel's cheap snapshot — mode, branch, the live commit (with CI + src
// verdicts), and the gate's pending/held tip when one exists. No git
// fetch: local object reads plus one cached CI lookup.
func (s *Server) handleReloadStatus(w http.ResponseWriter, r *http.Request) {
	type pendingJSON struct {
		reloadCommitJSON
		State string `json:"state"`
		Why   string `json:"why"`
	}
	resp := struct {
		Mode        string            `json:"mode"` // "gated", "legacy", or "none"
		HooksBranch string            `json:"hooks_branch,omitempty"`
		GateContext string            `json:"gate_context,omitempty"`
		Verified    *bool             `json:"verified,omitempty"`
		Live        *reloadCommitJSON `json:"live,omitempty"`
		Pending     *pendingJSON      `json:"pending,omitempty"`
	}{Mode: "none"}

	if s.reloadRepo == nil {
		// No hooks repo configured (plain local directory): the panel hides.
		writeJSON(w, http.StatusOK, resp)
		return
	}

	liveSHA := ""
	if s.reloadControl != nil {
		st := s.reloadControl.Status()
		resp.Mode = "gated"
		resp.HooksBranch = st.Branch
		resp.GateContext = st.Context
		resp.Verified = &st.Verified
		liveSHA = st.ServingSHA
		if st.PendingSHA != "" {
			p := pendingJSON{
				reloadCommitJSON: s.describeReloadCommit(r.Context(), st.PendingSHA, liveSHA),
				State:            st.PendingState,
			}
			p.Why = "awaiting " + st.Context + " (last known: " + st.PendingState + ")"
			resp.Pending = &p
		}
	} else {
		resp.Mode = "legacy"
		resp.HooksBranch = s.hooksBranch
	}
	if liveSHA == "" {
		// Legacy mode, or a gate that has not settled a serving sha yet:
		// the working tree's HEAD is what serves.
		if head, err := s.reloadRepo.Head(); err == nil {
			liveSHA = head
		}
	}
	if liveSHA != "" {
		live := s.describeReloadCommit(r.Context(), liveSHA, liveSHA)
		resp.Live = &live
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleReloadCommits is GET /reload/commits (admin port): fetch the
// tracked branch fresh, then list its recent history with per-commit
// subject/date, src-layout verdict, best-effort CI state, and the is_live
// marker. CI lookups share one bounded budget; commits past it read
// "unknown" rather than stalling the listing.
func (s *Server) handleReloadCommits(w http.ResponseWriter, r *http.Request) {
	if s.reloadRepo == nil {
		writeError(w, http.StatusNotFound, "no hooks repo configured")
		return
	}
	if _, err := s.reloadRepo.FetchBranch(reloadCommitsFetchDepth); err != nil {
		s.events.Record("git.pull_failed", "reload panel: hooks repo fetch failed: "+err.Error(), nil)
		writeError(w, http.StatusBadGateway, "fetch hooks repo: "+err.Error())
		return
	}
	shas, err := s.reloadRepo.RecentCommits(reloadCommitsMax)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list hooks repo history: "+err.Error())
		return
	}
	liveSHA := ""
	branch := s.hooksBranch
	if s.reloadControl != nil {
		st := s.reloadControl.Status()
		liveSHA, branch = st.ServingSHA, st.Branch
	}
	if liveSHA == "" {
		if head, herr := s.reloadRepo.Head(); herr == nil {
			liveSHA = head
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), reloadCIBudget)
	defer cancel()
	commits := make([]reloadCommitJSON, 0, len(shas))
	for _, sha := range shas {
		commits = append(commits, s.describeReloadCommit(ctx, sha, liveSHA))
	}
	writeJSON(w, http.StatusOK, struct {
		Branch  string             `json:"branch,omitempty"`
		Commits []reloadCommitJSON `json:"commits"`
	}{Branch: branch, Commits: commits})
}

// handleReloadCheck is POST /reload/check (admin port): reload on demand.
// Gated mode runs ONE pass of the existing reconciliation logic
// (Gate.Reconcile — fetch the tip, switch only on an affirmative green,
// hold loudly on anything else) and reports its outcome; legacy mode runs
// the legacy pull+reload. Neither forks the underlying path.
func (s *Server) handleReloadCheck(w http.ResponseWriter, r *http.Request) {
	if s.reloadControl == nil {
		s.events.Record("reload.requested", "reload requested via admin port (check & reload)", map[string]string{"source": "admin"})
		s.runReload(w) // the legacy pull-to-tip + reload, verbatim
		return
	}
	s.events.Record("reload.check", "operator requested an immediate reload re-evaluation (reconcile pass)", map[string]string{"source": "admin"})
	outcome := s.reloadControl.Reconcile(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"mode": "gated", "outcome": outcome})
}

// handleReloadSwitch is POST /reload/switch (admin port): the operator's
// manual commit pick, body {"ref": "<sha-or-branch>", "override": bool}.
// The server is authoritative about the informed-override contract: a
// commit failing any gate check (CI not affirmatively green — "unknown"
// counts as not green — or a tree without src/hooks) is answered 409 with
// every reason and requires_override:true, and the tree only moves when
// the request explicitly carries override:true (recorded loudly by the
// gate). Routed through the gate's Force-style apply path, so rollback to
// an older commit works and a wedged gate (unreadable CI) can always be
// overridden.
func (s *Server) handleReloadSwitch(w http.ResponseWriter, r *http.Request) {
	if s.reloadControl == nil {
		writeError(w, http.StatusConflict,
			"the reload CI gate is disabled (legacy mode): per-commit switching needs the gate; POST /reload pulls to the tip")
		return
	}
	var req struct {
		Ref      string `json:"ref"`
		Override bool   `json:"override"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	req.Ref = strings.TrimSpace(req.Ref)
	if req.Ref == "" {
		writeError(w, http.StatusBadRequest, `"ref" is required (a commit sha or branch/tag name)`)
		return
	}

	out, err := s.reloadControl.ManualSwitch(r.Context(), req.Ref, req.Override)
	if err != nil {
		if errors.Is(err, reloadgate.ErrUnknownRef) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	commit := s.describeReloadSwitchCommit(out)
	if !out.Switched {
		writeJSON(w, http.StatusConflict, struct {
			Error            string           `json:"error"`
			RequiresOverride bool             `json:"requires_override"`
			Reasons          []string         `json:"reasons"`
			Commit           reloadCommitJSON `json:"commit"`
		}{
			Error:            "switch refused: the commit does not pass the reload gate (retry with override:true to proceed anyway)",
			RequiresOverride: true,
			Reasons:          out.Reasons,
			Commit:           commit,
		})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status     string           `json:"status"`
		Overridden bool             `json:"overridden"`
		Reasons    []string         `json:"reasons,omitempty"`
		Commit     reloadCommitJSON `json:"commit"`
	}{Status: "switched", Overridden: out.Overridden(), Reasons: out.Reasons, Commit: commit})
}

// describeReloadSwitchCommit builds the switch response's commit view from
// the outcome's own (authoritative, decision-time) CI/src verdicts, with
// subject/date enrichment from the repo.
func (s *Server) describeReloadSwitchCommit(out reloadgate.SwitchOutcome) reloadCommitJSON {
	c := reloadCommitJSON{SHA: out.SHA, Short: shortSHA(out.SHA), CIState: out.CIState, HasSrc: out.HasSrc}
	s.enrichCommitInfo(&c)
	return c
}
