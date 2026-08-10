package cli

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
// THE FLEET IS ALL-OR-NOTHING, AND THAT IS THE POINT.
//
// A load in which ANY entity failed is REFUSED: nothing is swapped, the
// previous registry keeps serving, and the caller gets an error. It used to
// apply the partial set, which fails OPEN in the one situation that produces
// fleet-wide load errors -- a binary and a tree that disagree about the
// manifest contract. Every entity using the disputed field just stopped
// serving, silently, with a "hooks reloaded" line to match; the reload gate
// could not roll back, because on the binary-changed side the tree never
// moved. Recovery was manual, and the damage was invisible until someone
// noticed webhooks had stopped arriving.
//
// Refusing instead makes both deploy directions safe and loud:
//
//   - The TREE moved (a manifest using a field this binary lacks): the gate
//     resets to the commit that was serving and applies that, so the fleet
//     keeps running and the deploy is HELD, not half-applied.
//   - The BINARY moved (a field this tree still uses was removed): the
//     startup load fails and serve exits non-zero naming the entities, so
//     the deploy fails visibly instead of coming up serving a fraction of
//     the fleet.
//
// This is only safe to make strict because a load error cannot reach a
// gated deploy by the normal path: the reload gate requires the hooks repo's
// own CI green, and that CI runs `validate` through this same loader and the
// same embedded schemas. What remains is the binary/tree contract mismatch --
// exactly the case where serving "whatever still parses" is wrong.
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
func buildLoadAndApply(hooksDir string, registry *hooks.Registry, mgr *concurrency.Manager, sched *scheduler.Scheduler, sup *managers.Supervisor, ov *overrides.Store, agg *attention.Aggregator, secrets *hooks.SecretsLoader, logger *slog.Logger, rec *events.Recorder) func() error {
	// Orphan announcements are deduped per target across reloads: one event
	// when a reload first finds an override pointing at nothing, not one
	// per reload tick. A target that comes back is forgotten here, so a
	// later re-orphaning is announced again.
	var orphanMu sync.Mutex
	announced := map[string]struct{}{}
	return func() error {
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

		// A manager's spawn_targets must name declared hooks — the
		// manifest IS the spawn allowlist, so an undeclared target is a
		// misconfiguration and the manager is dropped (fail closed), the
		// undeclared-group rule. Checked against the post-rejection sets.
		for _, se := range hooks.CheckSpawnTargets(loaded, loadedManagers) {
			errs = append(errs, se)
			delete(loadedManagers, se.ManagerID)
		}

		// Operator settings overrides are applied to the freshly loaded
		// entities, AFTER the manifest passed its own validation and
		// BEFORE anything is served. Deliberately not part of `errs`: see
		// applySettingsOverrides for why a bad override must not be able
		// to refuse the tree.
		applySettingsOverrides(loaded, loadedManagers, ov, logger, rec)

		for _, e := range errs {
			logger.Error("hook reload error", "err", e)
			rec.Record("hook.load_error", e.Error(), nil)
		}

		// REFUSE: apply nothing. The per-entity errors still reach the
		// attention surface (that is how an operator sees WHICH entity and
		// WHICH field), but the serving registry, scheduler, concurrency
		// config and manager set are left exactly as they were.
		if len(errs) > 0 {
			loadEnts, zeroEnts := attention.FromLoadErrors(errs)
			agg.ReplaceSource(attention.SourceLoad, loadEnts)
			agg.ReplaceSource(attention.SourceZeroHooks, zeroEnts)
			agg.ReplaceSource(attention.SourceTreeRefused,
				attention.TreeRefusedEntries(len(errs), serving(registry)))

			refusal := refusedError{errs: errs, serving: serving(registry)}
			logger.Error("hooks tree REFUSED — keeping the previous fleet",
				"errors", len(errs), "serving_entities", refusal.serving)
			rec.Record("hooks.refused", refusal.Error(), nil)
			return refusal
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

		// A clean load clears every entry the refusal path may have left:
		// the problem is gone precisely because this tree loaded whole.
		agg.ReplaceSource(attention.SourceLoad, nil)
		agg.ReplaceSource(attention.SourceZeroHooks, nil)
		agg.ReplaceSource(attention.SourceTreeRefused, nil)

		attention.ApplyServeProbe(agg, loaded, secrets)

		announceOrphanedOverrides(loaded, cfg, ov, &orphanMu, announced, logger, rec)

		logger.Info("hooks reloaded", "count", len(loaded), "managers", len(loadedManagers), "layout", layout.String(), "concurrency_groups", len(cfg.Groups), "scheduled", len(schedules))
		rec.Record("hooks.reloaded",
			fmt.Sprintf("%d hook(s) + %d manager(s) loaded, %d concurrency group(s), %d scheduled", len(loaded), len(loadedManagers), len(cfg.Groups), len(schedules)),
			nil)
		return nil
	}
}

// applySettingsOverrides merges the operator's pinned settings fields into
// the freshly loaded entities.
//
// A REJECTED OVERRIDE MUST NEVER REFUSE THE TREE. Everything else in this
// file fails the whole load when one entity is bad, and that is right for a
// manifest: the tree is reviewed, CI-gated, and rollback-able. An override
// is none of those — it is a value typed into a dashboard and stored under
// the data dir, and the tree it was valid against can move underneath it
// (a manifest that renames the field, a schema that tightens the range).
// If that could refuse the load, one stale override would take the entire
// fleet down on the next reload, with the fix reachable only by hand-editing
// overrides.json on the runner host. So the degrade is per entity: drop THAT
// entity's overrides for this load, serve its manifest values, and be loud.
//
// Loud means all three surfaces an operator actually reads: the log, the
// activity feed, and (via the settings API's `rejected` field) the editor
// itself, which shows the exact schema error next to the field. The stored
// override is NOT deleted — the operator may be mid-way through a manifest
// change, and silently discarding what they typed is its own failure.
func applySettingsOverrides(loaded map[string]*hooks.Hook, loadedManagers map[string]*hooks.Manager, ov *overrides.Store, logger *slog.Logger, rec *events.Recorder) {
	all := ov.AllSettingsOverrides()
	if len(all) == 0 {
		return
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h, ok := loaded[id]
		if !ok {
			// A Manager embeds *Hook and shares the id namespace, so the
			// same merge applies to both. An override for neither is
			// orphaned, which announceOrphanedOverrides already reports.
			m, mok := loadedManagers[id]
			if !mok {
				continue
			}
			h = m.Hook
		}
		if err := h.ApplySettingsOverrides(all[id]); err != nil {
			logger.Error("settings override REJECTED — serving the manifest values for this entity",
				"entity", id, "err", err)
			rec.Record("settings.override_rejected",
				fmt.Sprintf("%s: settings override rejected, serving the manifest values instead (the override is kept, not deleted): %v", id, err),
				map[string]string{"hook": id})
		}
	}
}

// serving reports how many entities the registry is currently serving —
// zero means nothing has ever been applied, i.e. this is the startup load.
func serving(registry *hooks.Registry) int {
	return len(registry.All()) + len(registry.AllManagers())
}

// refusedError is what a refused load returns. It names the count and the
// first few offending entities, because the actionable part of a
// binary/tree mismatch is WHICH field the two disagree about.
type refusedError struct {
	errs    []error
	serving int
}

// refusedErrorSamples bounds the entity list in the message: a
// contract mismatch fails EVERY entity, and a 13-line error string buries
// the one sentence that says what to do.
const refusedErrorSamples = 3

func (e refusedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d entit(y/ies) failed to load, so the tree was REFUSED as a whole", len(e.errs))
	if e.serving > 0 {
		fmt.Fprintf(&b, " (still serving the previous %d)", e.serving)
	}
	b.WriteString(": ")
	for i, err := range e.errs {
		if i >= refusedErrorSamples {
			fmt.Fprintf(&b, "; +%d more", len(e.errs)-refusedErrorSamples)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(err.Error())
	}
	if e.serving == 0 {
		b.WriteString(" -- this is the startup load, so there is no previous fleet to fall back to; " +
			"a binary that cannot load the deployed tree must not serve a fraction of it")
	}
	return b.String()
}

func (e refusedError) Unwrap() []error { return e.errs }

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
