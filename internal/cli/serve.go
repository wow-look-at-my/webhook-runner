package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/runstore"
	"github.com/wow-look-at-my/webhook-runner/internal/scheduler"
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
	kvMaxKeys       int
	runRetention    time.Duration
	runRetentionMax int
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
	if o.kvMaxKeys <= 0 {
		// Positive integers only; unset or unparseable falls back to the
		// store's built-in default.
		if n, err := strconv.Atoi(os.Getenv("WEBHOOK_RUNNER_KV_MAX_KEYS")); err == nil && n > 0 {
			o.kvMaxKeys = n
		}
	}
	if o.runRetention <= 0 {
		// Go duration (e.g. "72h"); unset or unparseable falls back to the
		// run store's built-in 48h default.
		if d, err := time.ParseDuration(os.Getenv("WEBHOOK_RUNNER_RUN_RETENTION")); err == nil && d > 0 {
			o.runRetention = d
		}
	}
	if o.runRetentionMax <= 0 {
		if n, err := strconv.Atoi(os.Getenv("WEBHOOK_RUNNER_RUN_RETENTION_MAX")); err == nil && n > 0 {
			o.runRetentionMax = n
		}
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
	// WEBHOOK_RUNNER_KV_MAX_KEYS overrides the per-namespace key cap (zero
	// here means kv.New applies its built-in default).
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
	kvStore, err := kv.New(kv.Config{Dir: filepath.Join(dataDir, "kv"), MaxKeysPerNS: o.kvMaxKeys}, stateSecret, logger)
	if err != nil {
		return fmt.Errorf("state store: %w", err)
	}
	kvStore.StartSweeper()
	defer kvStore.Close()

	// Persistent run history: every run is written to a single bbolt file
	// under the data dir the moment it reaches a terminal status (the
	// tracker's OnFinish seam), and the admin read endpoints merge it behind
	// the live tracker — so completed runs survive restarts. A run still in
	// flight at shutdown never completed and is not in the store. Retention
	// is time-based (WEBHOOK_RUNNER_RUN_RETENTION, default 48h); the
	// per-hook count cap (WEBHOOK_RUNNER_RUN_RETENTION_MAX) is only a disk
	// safety net behind it.
	runStore, err := runstore.Open(runstore.Config{
		Path:       filepath.Join(dataDir, "runs.db"),
		Retention:  o.runRetention,
		MaxPerHook: o.runRetentionMax,
	}, logger)
	if err != nil {
		return fmt.Errorf("run store: %w", err)
	}
	runStore.StartSweeper()
	// Closed via defer, which runs after the shutdown path's rn.Wait() —
	// so every in-flight run has recorded its terminal state first.
	defer func() {
		if err := runStore.Close(); err != nil {
			logger.Warn("run store close", "err", err)
		}
	}()
	tracker.SetOnFinish(server.RunFinishCallback(kvStore, runStore.Record, rec, logger))

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
	// The KV proxy shim is webhook-runner's own (static) binary, copied to the
	// host-shared tmp dir so the runner can bind-mount it into state hooks as
	// their entrypoint — same host-shared-path requirement as the socket and
	// payload files. It proxies http://localhost:9002 to the socket so hooks
	// use a plain URL with any client.
	shimPath := filepath.Join(tmpDir, "whr-shim")
	if err := copyExecutable(shimPath); err != nil {
		return fmt.Errorf("kv proxy shim: %w", err)
	}

	// Operator overrides — the kill switch: per-hook disable switches and
	// concurrency limit overrides, flipped from the admin dashboard.
	// Operational state, not hooks-repo config: persisted under the data
	// dir (like kv and run history) and loaded BEFORE the first hooks load,
	// so the effective state after boot already reflects them. A corrupt
	// file fails startup rather than booting with kill switches silently
	// dropped.
	ovStore, err := overrides.Open(filepath.Join(dataDir, "overrides.json"))
	if err != nil {
		return fmt.Errorf("overrides store: %w", err)
	}

	// Concurrency groups (concurrency.json at the hooks root) gate how many
	// runs of a hook — or of several hooks sharing a group — execute at
	// once; the rest queue. The manager starts empty and is populated by
	// the initial load below — seeded first with the persisted operator
	// limit overrides so that very first Update already applies them (the
	// manager re-applies its overrides inside every Update, which is what
	// makes a hooks reload unable to silently revert one).
	concurrencyMgr := concurrency.NewManager(nil)
	for group, limit := range ovStore.ConcurrencyLimits() {
		if err := concurrencyMgr.SetLimitOverride(group, limit); err != nil {
			// A hand-edited overrides file can hold an invalid limit; say
			// so loudly and continue without it (never a silent drop).
			logger.Error("ignoring invalid persisted concurrency override", "group", group, "limit", limit, "err", err)
			rec.Record("override.invalid",
				fmt.Sprintf("ignoring persisted concurrency override for group %q: %v", group, err),
				map[string]string{"group": group})
		}
	}

	rn := runner.New(runner.Options{
		Tracker:  tracker,
		Logger:   logger,
		TmpDir:   tmpDir,
		Secrets:  secrets,
		Events:   rec,
		Groups:   concurrencyMgr,
		KV:       kvStore,
		KVSocket: socketPath,
		KVShim:   shimPath,
		OnStart: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostStart(context.Background(), h, r, payload)
		},
		OnFinish: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostFinish(context.Background(), h, r, payload)
		},
	})

	// Scheduler: fires hooks declaring a "schedule" interval on a timer,
	// through the very same run pipeline (so a scheduled run is tracked,
	// concurrency-gated, KV-enabled, and shown on the dashboard like any
	// other). See buildScheduleFire for the per-tick dispatch rules.
	sched := scheduler.New(scheduler.Options{
		Fire: buildScheduleFire(registry, tracker, ovStore, rn, logger, rec),
	})

	// loadAndApply reloads hooks, concurrency groups, and schedules together
	// so the registry, the manager, and the scheduler never drift: a hook
	// referencing an undeclared group is rejected (not registered) rather
	// than allowed to run unbounded. Both the filesystem watcher and the
	// admin/webhook reload path go through this one function.
	loadAndApply := buildLoadAndApply(o.hooksDir, registry, concurrencyMgr, sched, ovStore, logger, rec)

	onReload := buildReloadFunc(repo, loadAndApply, rec)

	// The build identity served by /health, /version, and the dashboard —
	// the same string the `version` command prints, so every surface
	// reports one consistent answer to "which build is deployed?".
	vcsRev, vcsTime := buildVCS()

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
		RunStore:     runStore,
		Overrides:    ovStore,
		Version:      server.VersionInfo{Version: versionString(), Revision: vcsRev, Time: vcsTime},
	})

	// Watcher runs for the lifetime of the server; its initial scan is what
	// first populates the registry, concurrency manager, and scheduler.
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- hooks.WatchFunc(watchCtx, o.hooksDir, loadAndApply, logger)
	}()

	// Scheduler loop runs for the lifetime of the server too; it does nothing
	// until the watcher's initial scan populates its schedule set, then fires
	// due hooks each tick.
	go sched.Run(watchCtx)

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
		"version", versionString(),
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
	// Disconnect /runs/stream clients FIRST: adminSrv.Shutdown waits for
	// in-flight handlers, and a stream handler holds its response open
	// until its subscription closes (or its client goes away).
	srv.CloseStreams()
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
// an undeclared group, then atomically updates the concurrency manager, the
// scheduler, and the registry. Folding the scheduler in here (rather than a
// second reload path) keeps the registry and the set of scheduled hooks from
// ever drifting apart.
//
// Operator overrides (ov) survive every reload by construction — the
// disable gate reads the override store at dispatch time, and the manager
// re-applies limit overrides inside Update — so a reload can never silently
// wipe a kill switch. What a reload CAN do is orphan an override (its hook
// or group no longer exists in the fresh config): the override is KEPT
// (inert; it re-applies if the target comes back) and announced with one
// override.orphaned event per orphaning, never silently dropped.
func buildLoadAndApply(hooksDir string, registry *hooks.Registry, mgr *concurrency.Manager, sched *scheduler.Scheduler, ov *overrides.Store, logger *slog.Logger, rec *events.Recorder) func() {
	// Orphan announcements are deduped per target across reloads: one event
	// when a reload first finds an override pointing at nothing, not one
	// per reload tick. A target that comes back is forgotten here, so a
	// later re-orphaning is announced again.
	var orphanMu sync.Mutex
	announced := map[string]struct{}{}
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

		// Extract the per-hook schedules from the (post-rejection) set so a
		// dropped hook is never scheduled.
		schedules := make(map[string]time.Duration, len(loaded))
		for id, h := range loaded {
			if iv := h.ScheduleInterval(); iv > 0 {
				schedules[id] = iv
			}
		}

		mgr.Update(cfg)
		if sched != nil {
			sched.Update(schedules)
		}
		registry.Replace(loaded)

		announceOrphanedOverrides(loaded, cfg, ov, &orphanMu, announced, logger, rec)

		logger.Info("hooks reloaded", "count", len(loaded), "concurrency_groups", len(cfg.Groups), "scheduled", len(schedules))
		rec.Record("hooks.reloaded",
			fmt.Sprintf("%d hook(s) loaded, %d concurrency group(s), %d scheduled, %d error(s)", len(loaded), len(cfg.Groups), len(schedules), len(errs)),
			nil)
	}
}

// announceOrphanedOverrides compares the operator overrides against the
// freshly loaded hooks/groups and records one override.orphaned event per
// override whose target vanished — once per orphaning, deduped in
// `announced` across reloads (targets that return are forgotten so a later
// re-orphaning is announced again). Orphaned overrides are never removed:
// they stay stored and re-apply if the hook/group comes back.
func announceOrphanedOverrides(loaded map[string]*hooks.Hook, cfg *concurrency.Config, ov *overrides.Store, mu *sync.Mutex, announced map[string]struct{}, logger *slog.Logger, rec *events.Recorder) {
	type orphan struct {
		msg    string
		fields map[string]string
	}
	current := map[string]orphan{}
	for _, id := range ov.DisabledHooks() {
		if _, ok := loaded[id]; !ok {
			current["hook:"+id] = orphan{
				msg:    fmt.Sprintf("disable override for hook %q is orphaned: the hook no longer exists (override kept; it re-applies if the hook returns)", id),
				fields: map[string]string{"hook": id},
			}
		}
	}
	for group := range ov.ConcurrencyLimits() {
		if !cfg.Has(group) {
			current["group:"+group] = orphan{
				msg:    fmt.Sprintf("concurrency limit override for group %q is orphaned: the group is no longer declared (override kept; it re-applies if the group returns)", group),
				fields: map[string]string{"group": group},
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for key, o := range current {
		if _, seen := announced[key]; seen {
			continue
		}
		announced[key] = struct{}{}
		logger.Warn("operator override is orphaned", "target", key)
		rec.Record("override.orphaned", o.msg, o.fields)
	}
	for key := range announced {
		if _, still := current[key]; !still {
			delete(announced, key)
		}
	}
}

// buildScheduleFire returns the scheduler's Fire callback: look the hook up
// fresh each tick (it may have been reloaded/removed), honor the operator
// kill switch, apply skip-if-already-running overlap protection via the
// tracker, and dispatch with context.Background() like other async runs
// (so shutting down the scheduler never kills a live run).
func buildScheduleFire(registry *hooks.Registry, tracker *runs.Tracker, ov *overrides.Store, rn *runner.Runner, logger *slog.Logger, rec *events.Recorder) func(string) {
	return func(hookID string) {
		h, ok := registry.Get(hookID)
		if !ok {
			return // schedule removed between the tick and now
		}
		if ov.HookDisabled(hookID) {
			// The kill switch gates dispatch everywhere: HTTP deliveries
			// 503 and scheduled runs are skipped — loudly, on the feed.
			logger.Info("scheduled run skipped; hook disabled by operator", "hook", hookID)
			rec.Record("schedule.skipped",
				fmt.Sprintf("%s: hook is disabled by operator; skipping scheduled run", hookID),
				map[string]string{"hook": hookID, "reason": "disabled by operator"})
			return
		}
		if tracker.HasActive(hookID) {
			logger.Info("scheduled run skipped; previous run still active", "hook", hookID)
			rec.Record("schedule.skipped",
				fmt.Sprintf("%s: previous scheduled run still in flight; skipping this tick", hookID),
				map[string]string{"hook": hookID})
			return
		}
		logger.Info("scheduled run firing", "hook", hookID, "schedule", h.Schedule)
		rec.Record("schedule.fired",
			fmt.Sprintf("%s: scheduled run starting (every %s)", hookID, h.Schedule),
			map[string]string{"hook": hookID, "schedule": h.Schedule})
		// The friendly title resolves against the synthetic payload/headers a
		// tick actually delivers; ScheduleRunTitle falls back to "schedule"
		// when that yields nothing, so a tick chip is never gibberish.
		payload, headers := schedulePayload(hookID), scheduleHeaders(hookID)
		if _, err := rn.Start(context.Background(), h, payload, headers, h.ScheduleRunTitle(payload, headers)); err != nil {
			logger.Error("scheduled run failed to start", "hook", hookID, "err", err)
		}
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

// schedulePayload is the synthetic request body a scheduled run receives in
// HOOK_PAYLOAD_FILE. It marks the run as schedule-triggered (vs an HTTP
// caller) and carries the fire time, so a hook can tell the two apart.
func schedulePayload(hookID string) []byte {
	b, _ := json.Marshal(map[string]string{
		"trigger": "schedule",
		"hook":    hookID,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
	return b
}

// scheduleHeaders are the synthetic request headers (written to
// HOOK_HEADERS_FILE) for a scheduled run.
func scheduleHeaders(hookID string) http.Header {
	return http.Header{
		"Content-Type":              []string{"application/json"},
		"X-Webhook-Runner-Schedule": []string{hookID},
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

// copyExecutable copies the running binary to dst (0755) via temp+rename, so
// it can be bind-mounted into hook containers as the KV proxy shim. The binary
// is static (CGO disabled), so it runs in any hook base image.
func copyExecutable(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
