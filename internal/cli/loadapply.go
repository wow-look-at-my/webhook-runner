package cli

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/managers"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/scheduler"
)

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
//
// The attention aggregator (agg) is re-derived here too: the collected
// load errors become the current "load"/"zero-hooks" problem sets, and
// ApplyServeProbe statically re-checks each LOADED hook's ${NAME}
// api_key/env references and sops decrypt (via the shared secrets loader)
// — serve-path only, exactly like the reload itself; `validate` stays
// environment-independent. That per-reload re-derivation IS the clear
// rule for those sources: fix the config, reload, entry gone.
func buildLoadAndApply(hooksDir string, registry *hooks.Registry, mgr *concurrency.Manager, sched *scheduler.Scheduler, sup *managers.Supervisor, ov *overrides.Store, agg *attention.Aggregator, secrets *hooks.SecretsLoader, logger *slog.Logger, rec *events.Recorder) func() {
	// Orphan announcements are deduped per target across reloads: one event
	// when a reload first finds an override pointing at nothing, not one
	// per reload tick. A target that comes back is forgotten here, so a
	// later re-orphaning is announced again.
	var orphanMu sync.Mutex
	announced := map[string]struct{}{}
	return func() {
		// Layout detection runs on EVERY reload: a hooks-repo pull can
		// restructure the tree (legacy <-> src), and the load must follow
		// it without a restart.
		layout := hooks.DetectLayout(hooksDir)
		loaded, errs := hooks.LoadLayout(layout)

		// Managers load alongside hooks (src/managers under the SDK
		// layout; legacy trees have none). One id namespace: a manager
		// colliding with a hook is dropped loudly — the hook wins, since
		// it predates the entity.
		loadedManagers, merrs := hooks.LoadManagers(layout)
		errs = append(errs, merrs...)
		for id := range loadedManagers {
			if _, clash := loaded[id]; clash {
				errs = append(errs, hooks.ManagerLoadError{ManagerID: id,
					Err: fmt.Errorf("id collides with a hook of the same name (hooks and managers share one id namespace)")})
				delete(loadedManagers, id)
			}
		}

		cfg, cerr := concurrency.LoadFile(layout.ConcurrencyPath())
		if cerr != nil {
			// An unparseable concurrency.json means we can't trust any
			// group reference; treat the set as empty so referencing hooks
			// fail closed below rather than running unbounded.
			errs = append(errs, cerr)
			cfg = &concurrency.Config{Groups: map[string]concurrency.Group{}}
		}

		// A hook (or manager) naming an undeclared group is a
		// misconfiguration: drop it so it can't be triggered (and can't run
		// without its intended backpressure).
		refs := make(map[string]string, len(loaded)+len(loadedManagers))
		for id, h := range loaded {
			refs[id] = h.ConcurrencyGroup
		}
		for id, m := range loadedManagers {
			refs[id] = m.ConcurrencyGroup
		}
		for _, re := range concurrency.CheckRefs(cfg, refs) {
			errs = append(errs, re)
			delete(loaded, re.HookID)
			delete(loadedManagers, re.HookID)
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
		registry.ReplaceManagers(loadedManagers)
		if sup != nil {
			sup.Update(loadedManagers)
		}

		// Re-derive the state-sourced attention entries from THIS load:
		// the retained per-hook errors above, the zero-hooks guard, and
		// the static resolvability probe of every loaded hook. Entries
		// whose problem persisted keep their Since; fixed ones clear.
		loadEnts, zeroEnts := attention.FromLoadErrors(errs)
		agg.ReplaceSource(attention.SourceLoad, loadEnts)
		agg.ReplaceSource(attention.SourceZeroHooks, zeroEnts)
		attention.ApplyServeProbe(agg, loaded, secrets)

		announceOrphanedOverrides(loaded, cfg, ov, &orphanMu, announced, logger, rec)

		logger.Info("hooks reloaded", "count", len(loaded), "managers", len(loadedManagers), "layout", layout.String(), "concurrency_groups", len(cfg.Groups), "scheduled", len(schedules))
		rec.Record("hooks.reloaded",
			fmt.Sprintf("%d hook(s) + %d manager(s) loaded, %d concurrency group(s), %d scheduled, %d error(s)", len(loaded), len(loadedManagers), len(cfg.Groups), len(schedules), len(errs)),
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
	for id, enabled := range ov.HookOverrides() {
		if _, ok := loaded[id]; !ok {
			kind := "disable"
			if enabled {
				kind = "enable"
			}
			current["hook:"+id] = orphan{
				msg:    fmt.Sprintf("%s override for hook %q is orphaned: the hook no longer exists (override kept; it re-applies if the hook returns)", kind, id),
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
