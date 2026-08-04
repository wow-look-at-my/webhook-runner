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
	"sync"
	"syscall"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/backlog"
	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/managers"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/runstore"
	"github.com/wow-look-at-my/webhook-runner/internal/scheduler"
	"github.com/wow-look-at-my/webhook-runner/internal/server"
	"github.com/wow-look-at-my/webhook-runner/internal/spool"
)

func runServe(ctx context.Context, o *serveOptions) error {
	logger := newLogger(o.logFormat)
	slog.SetDefault(logger)

	// If a hooks repo is configured, clone (or open) it. Gate mode never
	// pulls-to-tip on boot: the reload gate restores the persisted
	// last-good commit itself (gate.Startup below); an empty gate context
	// keeps the exact legacy clone-or-pull behavior.
	var repo *hooks.Repo
	if o.hooksRepo != "" {
		if o.hooksDir == "" {
			o.hooksDir = "/var/lib/webhook-runner/hooks"
		}
		sshKeyPath, err := hooks.EnsureSSHKey(filepath.Join(filepath.Dir(o.hooksDir), "id_ed25519"), logger)
		if err != nil {
			return fmt.Errorf("hooks repo ssh key: %w", err)
		}
		if o.gateContext != "" {
			repo, err = hooks.OpenRepo(o.hooksRepo, o.hooksBranch, o.hooksDir, sshKeyPath, logger)
		} else {
			repo, err = hooks.CloneRepo(o.hooksRepo, o.hooksBranch, o.hooksDir, sshKeyPath, logger)
		}
		if err != nil {
			return fmt.Errorf("hooks repo: %w", err)
		}
	}

	if o.hooksDir == "" {
		return errors.New("hooks directory required (positional arg, WEBHOOK_RUNNER_HOOKS_DIR, or WEBHOOK_RUNNER_HOOKS_REPO)")
	}

	registry := hooks.NewRegistry()
	tracker := runs.NewTracker()
	gh, err := newGitHubStatusClient(o, logger)
	if err != nil {
		return err
	}
	// The runner's OWN GitHub client (commit statuses; the reload-gate
	// poll's status reads) rides the mirror like every container does —
	// unconditional, no knob (see runner.GSMBaseURL).
	gh.SetAPIURL(runner.GSMBaseURL)
	logger.Info("github api base", "base", runner.GSMBaseURL)
	// Activity feed for the admin dashboard (in-memory, bounded — same
	// persistence model as run history).
	rec := events.NewRecorder(500)
	rec.Record("server.started", "webhook-runner started", map[string]string{
		"hook_addr": o.addr, "admin_addr": o.adminAddr, "hooks_dir": o.hooksDir,
	})
	// The aggregated "needs attention" problem set behind GET /attention
	// and the dashboard's red banner: the persistent, self-clearing view
	// of ACTIVE misconfigurations (vs the feed's scroll-away events).
	// State-derived sources re-derive inside every loadAndApply below;
	// the standard rules subscribe the recognized event-derived classes.
	agg := attention.New()
	attention.RegisterStandardEventRules(agg)
	// A containerized server whose temp dir isn't host-shared breaks every
	// hook run (payload mounts resolve on the docker HOST) — detect the
	// topology at startup and say so loudly. See runner.WarnIfContainerized.
	// The verdict is boot-scoped attention state: a running process's env
	// can't change, so the entry stands until a restart with TMPDIR set.
	if runner.WarnIfContainerized(logger, rec, "/.dockerenv", "/run/.containerenv") {
		agg.Report(attention.Entry{
			Source:  attention.SourceServer,
			Key:     attention.KeyTmpDir,
			Message: runner.TmpDirHazardMessage,
		})
	}
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

	// Durable per-hook batch backlogs (internal/backlog): what a run could not
	// get to, behind the state port's /backlog routes. Disk-backed beside the
	// KV namespaces, because outliving the run — and the process — that filled
	// it is the whole point.
	backlogStore, err := backlog.New(backlog.Config{Dir: filepath.Join(dataDir, "backlogs")}, logger)
	if err != nil {
		return fmt.Errorf("backlog store: %w", err)
	}

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

	// The shutdown delivery spool: deliveries that arrive while this process
	// is draining are parked here and run by the NEXT one. Without it a
	// deploy window answers 503 and the delivery is gone — GitHub does not
	// re-send a failed one (see internal/spool).
	spoolStore, err := spool.Open(filepath.Join(dataDir, "spool"), logger)
	if err != nil {
		return fmt.Errorf("delivery spool: %w", err)
	}

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

	// The GLOBAL run cap: a server-wide ceiling on simultaneously running
	// hook containers (every container holds a Docker bridge-network IPv4
	// address; an unbounded flood exhausts the pool). Group caps still
	// apply first — the cap is the ceiling over ALL of them, never a
	// replacement. Precedence: the dashboard's persisted override
	// (overrides.json, seeded here like the group overrides) >
	// WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS > the built-in default 64.
	globalCap := concurrency.NewGlobal(o.maxConcurrentRuns)
	if limit, ok := ovStore.GlobalRunLimit(); ok {
		if err := globalCap.SetLimitOverride(limit); err != nil {
			logger.Error("ignoring invalid persisted global run cap override", "limit", limit, "err", err)
			rec.Record("override.invalid",
				fmt.Sprintf("ignoring persisted global run cap override: %v", err), nil)
		}
	}

	rn := runner.New(runner.Options{
		Tracker:   tracker,
		Logger:    logger,
		TmpDir:    tmpDir,
		Secrets:   secrets,
		Events:    rec,
		Groups:    concurrencyMgr,
		GlobalCap: globalCap,
		KV:        kvStore,
		KVSocket:  socketPath,
		KVShim:    shimPath,
		OnStart: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostStart(context.Background(), h, r, payload)
		},
		OnFinish: func(h *hooks.Hook, r *runs.Run, payload []byte) {
			gh.PostFinish(context.Background(), h, r, payload)
		},
	})

	// Reap hook containers orphaned by a previous server process (a
	// SIGKILL mid-drain, a crash): each one holds a bridge-network IP
	// forever with no owner. Safe here and only here — the run store's
	// bbolt flock above proves no concurrent serve process is live, and
	// this process has started no runs yet. See runner.SweepOrphanContainers.
	rn.SweepOrphanContainers()

	// The manager supervisor: one long-lived instance per declared manager,
	// exactly-one-fleet-wide behind the kernel-flock lease in the data dir.
	// Managers are FIRST-CLASS (never runs): instance lock release rides
	// OnInstanceEnd — the finish-seam analog — and their problems surface
	// through the aggregator's manager source.
	sup := managers.New(managers.Options{
		Runner:    rn,
		LeasePath: filepath.Join(dataDir, "managers.lock"),
		Disabled:  ovStore.HookDisabled,
		Events:    rec,
		Logger:    logger,
		OnAttention: func(entries []managers.AttentionEntry) {
			ents := make([]attention.Entry, 0, len(entries))
			for _, e := range entries {
				ents = append(ents, attention.Entry{
					Source:  attention.SourceManager,
					Hook:    e.ID,
					Key:     "instance",
					Message: e.Message,
				})
			}
			agg.ReplaceSource(attention.SourceManager, ents)
		},
		OnInstanceEnd: func(instanceID string) {
			if n := kvStore.ReleaseRunLocks(instanceID); n > 0 {
				rec.Record("lock.released_on_finish",
					fmt.Sprintf("released %d lock(s) still held by manager instance %s at instance end", n, instanceID),
					nil)
			}
		},
	})

	// The lock sweeper's liveness oracle. An expired lock may only be reaped
	// once its holder is CERTAINLY gone — expiry alone never frees a mutex
	// (internal/kv/lock.go) — and a holder is either a tracked run or a
	// manager's live instance, so both are asked. Without this the store
	// reaps nothing, which is the safe direction.
	kvStore.SetRunLiveness(func(id string) bool {
		if r := tracker.Get(id); r != nil && !r.Status().Terminal() {
			return true
		}
		return sup.AnyCurrentInstance(id)
	})

	// Scheduler: fires hooks declaring a "schedule" interval on a timer,
	// through the very same run pipeline (so a scheduled run is tracked,
	// concurrency-gated, KV-enabled, and shown on the dashboard like any
	// other). See buildScheduleFire for the per-tick dispatch rules.
	sched := scheduler.New(scheduler.Options{
		Fire: buildScheduleFire(registry, tracker, ovStore, rn, logger, rec),
	})

	// loadAndApply reloads hooks, MANAGERS, concurrency groups, and
	// schedules together so the registry, the concurrency manager, the
	// scheduler, and the supervisor never drift: a hook referencing an
	// undeclared group is rejected (not registered) rather than allowed to
	// run unbounded, and a manager id colliding with a hook is dropped
	// loudly. Both the filesystem watcher and the admin/webhook reload path
	// go through this one function.
	loadAndApply := buildLoadAndApply(o.hooksDir, registry, concurrencyMgr, sched, sup, ovStore, agg, secrets, logger, rec)

	onReload, gate, err := buildReloadPath(repo, o, dataDir, loadAndApply, gh, rec, agg, logger)
	if err != nil {
		return err
	}

	// The build identity served by /health, /version, and the dashboard —
	// the same string the `version` command prints, so every surface
	// reports one consistent answer to "which build is deployed?".
	vcsRev, vcsTime := buildVCS()

	srvOpts := server.Options{
		Registry:        registry,
		Runner:          rn,
		Tracker:         tracker,
		GitHub:          gh,
		Secrets:         secrets,
		Concurrency:     concurrencyMgr,
		GlobalCap:       globalCap,
		Events:          rec,
		Attention:       agg,
		Logger:          logger,
		ReloadSecret:    o.hooksRepoSecret,
		OnReload:        onReload,
		HooksRepo:       o.hooksRepo,
		HooksBranch:     o.hooksBranch,
		HookBaseURL:     o.hookBaseURL,
		KV:              kvStore,
		Backlogs:        backlogStore,
		RunStore:        runStore,
		Overrides:       ovStore,
		Managers:        sup,
		Version:         server.VersionInfo{Version: versionString(), Revision: vcsRev, Time: vcsTime},
		RestartMaxDefer: o.restartMaxDefer,
		Spool:           spoolStore,
	}
	if repo != nil {
		// The admin reload panel's read surface over the clone (status /
		// recent-commits views). Assigned only when non-nil so the
		// interface field stays truly nil for local-directory serving.
		srvOpts.ReloadRepo = repo
	}
	if gate != nil {
		// Assigned only when non-nil so the interface fields stay truly
		// nil (legacy flow) rather than wrapping a nil pointer. TreeState
		// rides the same nil check: /version reports the gate's hooks-tree
		// state only when a gate actually tracks the tree.
		srvOpts.Gate = gate
		srvOpts.TreeState = gate.TreeState
		// The panel's manual-control surface: gate snapshot, on-demand
		// reconcile, and the informed-override commit switch.
		srvOpts.ReloadControl = gate
	}
	srv := server.New(srvOpts)

	// Watcher runs for the lifetime of the server; its initial scan is what
	// first populates the registry, concurrency manager, and scheduler.
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watchErr := make(chan error, 1)
	// Replay parked deliveries exactly once, on the FIRST load that
	// populates the registry — event-driven off the watcher's initial scan
	// rather than polling for readiness, and necessarily after it, since a
	// replay needs its hook to exist.
	var replayOnce sync.Once
	loadThenReplay := func() error {
		err := loadAndApply()
		replayOnce.Do(func() {
			replaySpooledDeliveries(spoolStore, registry, rn, rec, logger)
		})
		return err
	}

	// THE STARTUP LOAD IS FATAL WHEN REFUSED. A binary that cannot load the
	// deployed tree has exactly two options: serve a FRACTION of the fleet,
	// or refuse to serve. The fraction is the dangerous one — it comes up
	// answering /health, the dashboard looks alive, and the entities the
	// binary could not parse have simply stopped existing, with no rollback
	// available because the tree never changed (the binary did).
	//
	// Exiting instead makes it a FAILED DEPLOY: loud, immediate, and
	// attributable to the image that just rolled out. This is the
	// binary-moved half of the fail-closed rule; the tree-moved half is the
	// reload gate's rollback (internal/reloadgate.applyOrRollbackLocked).
	if err := loadThenReplay(); err != nil {
		return fmt.Errorf("refusing to serve: %w", err)
	}
	go func() {
		watchErr <- hooks.WatchFunc(watchCtx, o.hooksDir, func() { _ = loadThenReplay() }, logger)
	}()

	// Scheduler loop runs for the lifetime of the server too; it does nothing
	// until the watcher's initial scan populates its schedule set, then fires
	// due hooks each tick.
	go sched.Run(watchCtx)

	// Scheduled-hook staleness watch: independent of reload (staleness is a
	// function of elapsed time, not a tree change), so it runs on its own
	// ticker for the server's lifetime. See attention.CheckStaleSchedules —
	// a scheduled tick is often the reliability BACKSTOP for whatever a hook
	// manages, and a backstop that silently stops succeeding looks, from the
	// outside, identical to one with nothing to do.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				agg.ReplaceSource(attention.SourceSchedule, attention.CheckStaleSchedules(
					sched.Schedules(),
					func(hookID string) []*runs.Run { return tracker.ListByHook(hookID, 50) },
					time.Now(),
				))
			}
		}
	}()

	// The manager supervisor: acquires the single-instance lease (flat
	// poll — during a rolling deploy the old process holds it until its
	// instances are down), then runs one loop per declared manager. Its
	// desired set arrives via loadAndApply above.
	go sup.Run(watchCtx)

	// Reload-gate reconciliation poll — the fallback that keeps a missed
	// status webhook from freezing deploys: one immediate pass at startup
	// (catching a green missed while down), then one per interval. Each
	// pass fetches the remote tip and, only when it differs from what is
	// serving, reads the gating context's commit status — switching solely
	// on an affirmative green through the gate's normal ordering-checked
	// path, holding loudly on anything else. Gated mode only: the legacy
	// (gate-disabled) flow keeps its exact reload-on-signed-POST semantics
	// with no timer.
	if o.hooksRepo != "" && o.reloadPollInterval > 0 {
		if gate == nil {
			logger.Info("reload poll not started: CI gate is disabled (legacy any-signed-POST reload mode)")
		} else {
			poller := reloadgate.NewPoller(reloadgate.PollerOptions{
				Interval: o.reloadPollInterval,
				Fire:     func() { gate.Reconcile(context.Background()) },
			})
			logger.Info("reload poll started", "interval", o.reloadPollInterval)
			go poller.Run(watchCtx)
		}
	} else if gate != nil {
		logger.Info("reload poll disabled (WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL=0); the gate is event-driven only")
	}

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
		gateLabel := "disabled"
		if o.gateContext != "" {
			gateLabel = o.gateContext
		}
		attrs = append(attrs, "hooks_repo", o.hooksRepo, "reload_gate", gateLabel)
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
	// Refuse NEW runs immediately: a run launched by this dying process
	// races the state-socket handover (its shim would dial a socket the
	// next server replaces). Deliveries are not rejected though — they are
	// PARKED (internal/spool) and answered 202, because GitHub does not
	// re-send a failed delivery. In-flight runs drain via rn.Wait below.
	rn.BeginShutdown()
	// Stop manager instances gracefully (docker stop; SIGTERM + grace)
	// BEFORE anything else winds down: the lease releases only when this
	// process exits, so the successor process's supervisor cannot start
	// replacement instances until ours are provably gone.
	sup.Shutdown()
	// Disconnect /runs/stream clients FIRST: adminSrv.Shutdown waits for
	// in-flight handlers, and a stream handler holds its response open
	// until its subscription closes (or its client goes away).
	srv.CloseStreams()
	adminCtx, cancelAdmin := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelAdmin()
	// The admin port has no dependents — it can go now.
	if err := adminSrv.Shutdown(adminCtx); err != nil {
		logger.Warn("admin server shutdown", "err", err)
	}
	// ORDER IS THE POINT. The hook listener and the state socket stay UP
	// across the drain:
	//   - the hook port, because rn.Wait can take as long as the longest
	//     run (a CI job is minutes). Stopping it first left the process
	//     alive with nothing listening for that entire stretch, and every
	//     delivery arriving in it got connection-refused — silently lost,
	//     since GitHub does not retry. Now they spool and answer 202.
	//   - the state socket, because DRAINING RUNS ARE STILL USING IT: locks,
	//     /wait, /title all ride it. Closing it before rn.Wait pulled the
	//     floor out from under the very runs being drained.
	rn.Wait()
	// Their grace window starts HERE, not before the drain — a deadline
	// armed pre-Wait would already be blown and turn a graceful close into
	// an abrupt one.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hookSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("hook server shutdown", "err", err)
	}
	if err := stateSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("state server shutdown", "err", err)
	}
	cancelWatch()
	return nil
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
		if ov.HookDisabled(hookID, h.EnabledByDefault()) {
			// The kill switch gates dispatch everywhere: HTTP deliveries
			// 503 and scheduled runs are skipped — loudly, on the feed.
			// Effective state: explicit operator override first, else the
			// hook.json `enable` default (false = born disabled).
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
