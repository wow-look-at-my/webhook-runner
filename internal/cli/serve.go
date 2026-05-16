package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/server"
)

type serveOptions struct {
	addr            string
	adminAddr       string
	hooksDir        string
	logFormat       string
	ghToken         string
	hooksRepo       string
	hooksBranch     string
	hooksRepoSecret string
	hooksRepoToken  string
}

func applyServeEnv(o *serveOptions) {
	if o.addr == "" {
		o.addr = firstNonEmpty(os.Getenv("WEBHOOK_RUNNER_ADDR"), ":9000")
	}
	if o.adminAddr == "" {
		o.adminAddr = firstNonEmpty(os.Getenv("WEBHOOK_RUNNER_ADMIN_ADDR"), ":9001")
	}
	if o.hooksDir == "" {
		o.hooksDir = os.Getenv("WEBHOOK_RUNNER_HOOKS_DIR")
	}
	if o.logFormat == "" {
		o.logFormat = firstNonEmpty(os.Getenv("WEBHOOK_RUNNER_LOG_FORMAT"), "text")
	}
	o.ghToken = os.Getenv("WEBHOOK_RUNNER_GITHUB_TOKEN")
	if o.hooksRepo == "" {
		o.hooksRepo = os.Getenv("WEBHOOK_RUNNER_HOOKS_REPO")
	}
	if o.hooksBranch == "" {
		o.hooksBranch = os.Getenv("WEBHOOK_RUNNER_HOOKS_BRANCH")
	}
	if o.hooksRepoSecret == "" {
		o.hooksRepoSecret = os.Getenv("WEBHOOK_RUNNER_HOOKS_REPO_SECRET")
	}
	if o.hooksRepoToken == "" {
		o.hooksRepoToken = os.Getenv("WEBHOOK_RUNNER_HOOKS_REPO_TOKEN")
	}
}

func runServe(ctx context.Context, o *serveOptions) error {
	logger := newLogger(o.logFormat)
	slog.SetDefault(logger)

	// If a hooks repo is configured, clone/pull it.
	var repo *hooks.Repo
	if o.hooksRepo != "" {
		if o.hooksDir == "" {
			o.hooksDir = "/var/lib/webhook-runner/hooks"
		}
		var err error
		repo, err = hooks.CloneRepo(o.hooksRepo, o.hooksBranch, o.hooksDir, o.hooksRepoToken, logger)
		if err != nil {
			return fmt.Errorf("hooks repo: %w", err)
		}
	}

	if o.hooksDir == "" {
		return errors.New("hooks directory required (positional arg, WEBHOOK_RUNNER_HOOKS_DIR, or WEBHOOK_RUNNER_HOOKS_REPO)")
	}

	registry := hooks.NewRegistry()
	tracker := runs.NewTracker()
	gh := githubstatus.New(o.ghToken, logger)

	rn := runner.New(runner.Options{
		Tracker: tracker,
		Logger:  logger,
		OnStart: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostStart(context.Background(), h, r, payload)
		},
		OnFinish: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostFinish(context.Background(), h, r, payload)
		},
	})

	onReload := buildReloadFunc(repo, o.hooksDir, registry, logger)

	srv := server.New(server.Options{
		Registry:     registry,
		Runner:       rn,
		Tracker:      tracker,
		GitHub:       gh,
		Logger:       logger,
		ReloadSecret: o.hooksRepoSecret,
		OnReload:     onReload,
	})

	// Watcher runs for the lifetime of the server.
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- hooks.Watch(watchCtx, o.hooksDir, registry, logger)
	}()

	hookSrv := &http.Server{
		Addr:              o.addr,
		Handler:           srv.HookHandler(),
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	adminSrv := &http.Server{
		Addr:              o.adminAddr,
		Handler:           srv.AdminHandler(),
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Trap signals for graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpErr := make(chan error, 2)
	go func() {
		logger.Info("hook server listening", "addr", o.addr)
		if err := hookSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- fmt.Errorf("hook server: %w", err)
		}
	}()
	go func() {
		logger.Info("admin server listening", "addr", o.adminAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- fmt.Errorf("admin server: %w", err)
		}
	}()

	attrs := []any{
		"hook_addr", o.addr,
		"admin_addr", o.adminAddr,
		"hooks_dir", o.hooksDir,
		"github_status", gh.Enabled(),
	}
	if o.hooksRepo != "" {
		attrs = append(attrs, "hooks_repo", o.hooksRepo)
	}
	logger.Info("webhook-runner started", attrs...)

	select {
	case err := <-httpErr:
		return err
	case err := <-watchErr:
		if err != nil {
			return fmt.Errorf("watcher: %w", err)
		}
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hookSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("hook server shutdown", "err", err)
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("admin server shutdown", "err", err)
	}
	cancelWatch()
	rn.Wait()
	return nil
}

func buildReloadFunc(repo *hooks.Repo, hooksDir string, registry *hooks.Registry, logger *slog.Logger) func() error {
	reloadFromDisk := func() {
		loaded, errs := hooks.LoadDir(hooksDir)
		for _, e := range errs {
			logger.Error("hook reload error", "err", e)
		}
		registry.Replace(loaded)
		logger.Info("hooks reloaded", "count", len(loaded))
	}
	if repo != nil {
		return func() error {
			if err := repo.Pull(); err != nil {
				return err
			}
			reloadFromDisk()
			return nil
		}
	}
	return func() error {
		reloadFromDisk()
		return nil
	}
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	default:
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
}

func firstNonEmpty(parts ...string) string {
	for _, p := range parts {
		if p != "" {
			return p
		}
	}
	return ""
}
