package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
)

// buildReloadPath wires the reload machinery: the OnReload callback served
// by admin POST /reload, and — in gate mode — the reload CI gate handed to
// the server for POST /_reload.
//
// With a hooks repo configured and a non-empty gate context, /_reload
// becomes event-aware: a push only records the pending tip; a green gating
// commit status is what moves the tree — and admin /reload becomes the
// operator's deliberate bypass (gate.Force). The gate's Startup restores
// the persisted last-good commit BEFORE the watcher's initial scan performs
// the first hooks load, and never applies by itself. The gate also carries
// the reconciliation poll's status reader (see buildGateStatusFunc) so
// Reconcile can read a newer tip's gating status from the GitHub API. An
// empty gate context (or no repo) keeps the legacy path: any reload
// pulls-to-tip + reloads.
func buildReloadPath(repo *hooks.Repo, o *serveOptions, dataDir string, loadAndApply func() error, gh *githubstatus.Client, rec *events.Recorder, agg *attention.Aggregator, logger *slog.Logger) (func() error, *reloadgate.Gate, error) {
	if repo == nil || o.gateContext == "" {
		return buildReloadFunc(repo, loadAndApply, rec), nil, nil
	}
	// Same derivation the poll's status reader uses; here it only decorates
	// held-commit messages with a run-details link, so an unparseable URL
	// costs the link, never the gating.
	repoSlug, _ := githubstatus.RepoFromGitURL(o.hooksRepo)
	gate, err := reloadgate.New(reloadgate.Config{
		Repo:      repo,
		Branch:    o.hooksBranch,
		Context:   o.gateContext,
		StatePath: filepath.Join(dataDir, "reload-gate.json"),
		Apply:     loadAndApply,
		Status:    buildGateStatusFunc(gh, o, logger),
		RepoSlug:  repoSlug,
		Events:    rec,
		Attention: agg,
		Logger:    logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reload gate: %w", err)
	}
	gate.Startup()
	return gate.Force, gate, nil
}

// buildGateStatusFunc returns the reconciliation poll's status reader: the
// gating context's state, read from the hooks repo's combined commit
// status with the same WEBHOOK_RUNNER_GITHUB_TOKEN credential the
// githubstatus client already carries (owner/repo derived from the
// configured hooks-repo URL). A URL no owner/repo can be derived from
// yields a reader that always errors, so the poll holds loudly on tip
// changes instead of guessing — fail closed, never fail open.
func buildGateStatusFunc(gh *githubstatus.Client, o *serveOptions, logger *slog.Logger) reloadgate.StatusFunc {
	repoSlug, ok := githubstatus.RepoFromGitURL(o.hooksRepo)
	if !ok {
		hooksRepo := o.hooksRepo
		logger.Warn("reload poll: cannot derive owner/repo from hooks repo URL; polls will hold on tip changes", "url", hooksRepo)
		return func(context.Context, string) (string, error) {
			return "", fmt.Errorf("cannot derive owner/repo from hooks repo URL %q", hooksRepo)
		}
	}
	gateContext := o.gateContext
	return func(ctx context.Context, sha string) (string, error) {
		state, err := gh.ContextState(ctx, repoSlug, sha, gateContext)
		if errors.Is(err, githubstatus.ErrNoContextStatus) {
			// No status for the context yet — a determinate answer the
			// gate holds as pending (CI hasn't reported), not blindness.
			return "", nil
		}
		return state, err
	}
}

// buildReloadFunc is the legacy (gate-less) reload: pull the hooks repo to
// its tip when one is configured, then reload from disk.
func buildReloadFunc(repo *hooks.Repo, loadAndApply func() error, rec *events.Recorder) func() error {
	if repo != nil {
		return func() error {
			if err := repo.Pull(); err != nil {
				rec.Record("git.pull_failed", "hooks repo pull failed: "+err.Error(), nil)
				return err
			}
			rec.Record("git.pulled", "hooks repo pulled", nil)
			return loadAndApply()
		}
	}
	return loadAndApply
}
