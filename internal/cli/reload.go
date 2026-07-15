package cli

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
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
// the first hooks load, and never applies by itself. An empty gate context
// (or no repo) keeps the legacy path: any reload pulls-to-tip + reloads.
func buildReloadPath(repo *hooks.Repo, o *serveOptions, dataDir string, loadAndApply func(), rec *events.Recorder, agg *attention.Aggregator, logger *slog.Logger) (func() error, *reloadgate.Gate, error) {
	if repo == nil || o.gateContext == "" {
		return buildReloadFunc(repo, loadAndApply, rec), nil, nil
	}
	gate, err := reloadgate.New(reloadgate.Config{
		Repo:      repo,
		Branch:    o.hooksBranch,
		Context:   o.gateContext,
		StatePath: filepath.Join(dataDir, "reload-gate.json"),
		Apply:     loadAndApply,
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

// buildReloadFunc is the legacy (gate-less) reload: pull the hooks repo to
// its tip when one is configured, then reload from disk.
func buildReloadFunc(repo *hooks.Repo, loadAndApply func(), rec *events.Recorder) func() error {
	if repo != nil {
		return func() error {
			if err := repo.Pull(); err != nil {
				rec.Record("git.pull_failed", "hooks repo pull failed: "+err.Error(), nil)
				return err
			}
			rec.Record("git.pulled", "hooks repo pulled", nil)
			loadAndApply()
			return nil
		}
	}
	return func() error {
		loadAndApply()
		return nil
	}
}
