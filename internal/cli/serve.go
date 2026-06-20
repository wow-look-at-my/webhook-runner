package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/server"
)

type serveOptions struct {
	addr            string
	adminAddr       string
	hooksDir        string
	dataDir         string
	logFormat       string
	ghToken         string
	hooksRepo       string
	hooksBranch     string
	hooksRepoSecret string
	hookBaseURL     string
	stateSocket     string
	stateSecret     string
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
	if o.dataDir == "" {
		o.dataDir = os.Getenv("WEBHOOK_RUNNER_DATA_DIR")
	}
	if o.stateSocket == "" {
		o.stateSocket = os.Getenv("WEBHOOK_RUNNER_STATE_SOCKET")
	}
	if o.stateSecret == "" {
		o.stateSecret = os.Getenv("WEBHOOK_RUNNER_STATE_SECRET")
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
	if o.hookBaseURL == "" {
		o.hookBaseURL = os.Getenv("WEBHOOK_RUNNER_HOOK_BASE_URL")
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
		sshKeyPath, err := hooks.EnsureSSHKey(filepath.Join(filepath.Dir(o.hooksDir), "id_ed25519"), logger)
		if err != nil {
			return fmt.Errorf("hooks repo ssh key: %w", err)
		}
		repo, err = hooks.CloneRepo(o.hooksRepo, o.hooksBranch, o.hooksDir, sshKeyPath, logger)
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
	// Activity feed for the admin dashboard (in-memory, bounded — same
	// persistence model as run history).
	rec := events.NewRecorder(500)
	rec.Record("server.started", "webhook-runner started", map[string]string{
		"hook_addr": o.addr, "admin_addr": o.adminAddr, "hooks_dir": o.hooksDir,
	})
	// A containerized server whose temp dir isn't host-shared breaks every
	// hook run (payload mounts resolve on the docker HOST) — detect the
	// topology at startup and say so loudly. See runner.WarnIfContainerized.
	runner.WarnIfContainerized(logger, rec, "/.dockerenv", "/run/.containerenv")
	// Per-hook sops secrets (secrets.sops.env next to a hook.json). The sops
	// binary comes from PATH unless WEBHOOK_RUNNER_SOPS_BIN overrides it;
	// key material (e.g. SOPS_AGE_KEY_FILE) is plain sops configuration on
	// this process's environment.
	secrets := hooks.NewSecretsLoader(os.Getenv("WEBHOOK_RUNNER_SOPS_BIN"))

	// Persistent KV state store backing the state port. State lives under the
	// data dir (default: alongside the hooks clone and deploy key) so it
	// survives restarts; hooks opt in with "state": true. The secret signs
	// per-hook namespace tokens — supply WEBHOOK_RUNNER_STATE_SECRET to share
	// one across replicas, else it's generated and persisted.
	dataDir := o.dataDir
	if dataDir == "" {
		dataDir = filepath.Dir(o.hooksDir)
	}
	stateSecret := []byte(o.stateSecret)
	if len(stateSecret) == 0 {
		s, err := kv.EnsureSecret(filepath.Join(dataDir, "state-secret"))
		if err != nil {
			return fmt.Errorf("state secret: %w", err)
		}
		stateSecret = s
	}
	kvStore, err := kv.New(kv.Config{Dir: filepath.Join(dataDir, "kv")}, stateSecret, logger)
	if err != nil {
		return fmt.Errorf("state store: %w", err)
	}
	kvStore.StartSweeper()
	defer kvStore.Close()

	// The state KV API is served on a Unix socket (no networking). It must
	// live in the same host-shared dir the runner mounts per-run files from
	// (TMPDIR), so a sibling hook container resolves the same host path when
	// the runner bind-mounts it in. os.TempDir() honors $TMPDIR and matches
	// the runner's default tmp dir.
	tmpDir := os.TempDir()
	socketPath := o.stateSocket
	if socketPath == "" {
		socketPath = filepath.Join(tmpDir, "whr-state.sock")
	}

	// Concurrency groups (concurrency.json at the hooks root) gate how many
	// runs of a hook — or of several hooks sharing a group — execute at
	// once; the rest queue. The manager starts empty and is populated by
	// the initial load below.
	concurrencyMgr := concurrency.NewManager(nil)

	rn := runner.New(runner.Options{
		Tracker:  tracker,
		Logger:   logger,
		TmpDir:   tmpDir,
		Secrets:  secrets,
		Events:   rec,
		Groups:   concurrencyMgr,
		KV:       kvStore,
		KVSocket: socketPath,
		OnStart: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostStart(context.Background(), h, r, payload)
		},
		OnFinish: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostFinish(context.Background(), h, r, payload)
		},
	})

	// loadAndApply reloads hooks and concurrency groups together so the
	// registry and the manager never drift: a hook referencing an
	// undeclared group is rejected (not registered) rather than allowed to
	// run unbounded. Both the filesystem watcher and the admin/webhook
	// reload path go through this one function.
	loadAndApply := buildLoadAndApply(o.hooksDir, registry, concurrencyMgr, logger, rec)

	onReload := buildReloadFunc(repo, loadAndApply, rec)

	srv := server.New(server.Options{
		Registry:     registry,
		Runner:       rn,
		Tracker:      tracker,
		GitHub:       gh,
		Secrets:      secrets,
		Concurrency:  concurrencyMgr,
		Events:       rec,
		Logger:       logger,
		ReloadSecret: o.hooksRepoSecret,
		OnReload:     onReload,
		HooksRepo:    o.hooksRepo,
		HookBaseURL:  o.hookBaseURL,
		KV:           kvStore,
	})

	// Watcher runs for the lifetime of the server; its initial scan is what
	// first populates the registry and concurrency manager.
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- hooks.WatchFunc(watchCtx, o.hooksDir, loadAndApply, logger)
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
	// The state KV API listens on a Unix socket bind-mounted into state hooks,
	// not a network port. Clear any stale socket left by a crashed prior run.
	_ = os.Remove(socketPath)
	stateLn, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("state socket %s: %w", socketPath, err)
	}
	// World-accessible so hooks running as non-root users can connect; the
	// per-hook bearer token (not file perms) is what authorizes access.
	if err := os.Chmod(socketPath, 0o666); err != nil {
		logger.Warn("chmod state socket", "path", socketPath, "err", err)
	}
	stateSrv := &http.Server{
		Handler:           srv.StateHandler(),
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Trap signals for graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpErr := make(chan error, 3)
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
	go func() {
		logger.Info("state socket listening", "path", socketPath)
		if err := stateSrv.Serve(stateLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- fmt.Errorf("state server: %w", err)
		}
	}()

	attrs := []any{
		"hook_addr", o.addr,
		"admin_addr", o.adminAddr,
		"state_socket", socketPath,
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
	if err := stateSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("state server shutdown", "err", err)
	}
	cancelWatch()
	rn.Wait()
	return nil
}

// buildLoadAndApply returns the single reload routine shared by the
// filesystem watcher and the admin/webhook reload path. It loads the hooks
// and the concurrency-group config from disk, rejects hooks that reference
// an undeclared group, then atomically updates the concurrency manager and
// the registry.
func buildLoadAndApply(hooksDir string, registry *hooks.Registry, mgr *concurrency.Manager, logger *slog.Logger, rec *events.Recorder) func() {
	return func() {
		loaded, errs := hooks.LoadDir(hooksDir)

		cfg, cerr := concurrency.Load(hooksDir)
		if cerr != nil {
			// An unparseable concurrency.json means we can't trust any
			// group reference; treat the set as empty so referencing hooks
			// fail closed below rather than running unbounded.
			errs = append(errs, cerr)
			cfg = &concurrency.Config{Groups: map[string]concurrency.Group{}}
		}

		// A hook naming an undeclared group is a misconfiguration: drop it
		// so it can't be triggered (and can't run without its intended
		// backpressure).
		refs := make(map[string]string, len(loaded))
		for id, h := range loaded {
			refs[id] = h.ConcurrencyGroup
		}
		for _, re := range concurrency.CheckRefs(cfg, refs) {
			errs = append(errs, re)
			delete(loaded, re.HookID)
		}

		for _, e := range errs {
			logger.Error("hook reload error", "err", e)
			rec.Record("hook.load_error", e.Error(), nil)
		}

		mgr.Update(cfg)
		registry.Replace(loaded)
		logger.Info("hooks reloaded", "count", len(loaded), "concurrency_groups", len(cfg.Groups))
		rec.Record("hooks.reloaded",
			fmt.Sprintf("%d hook(s) loaded, %d concurrency group(s), %d error(s)", len(loaded), len(cfg.Groups), len(errs)),
			nil)
	}
}

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
