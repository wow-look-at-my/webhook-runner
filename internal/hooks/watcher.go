package hooks

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch observes the hooks directory and replaces the registry from disk on every change. It is a thin wrapper over WatchFunc for callers that only need the default "reload the registry" behavior. Watch blocks until ctx is canceled.
func Watch(ctx context.Context, root string, reg *Registry, log *slog.Logger) error {
	return WatchFunc(ctx, root, func() {
		hooks, errs := LoadDir(root)
		for _, e := range errs {
			log.Error("hook reload error", "err", e)
		}
		reg.Replace(hooks)
		log.Info("hooks reloaded", "count", len(hooks))
	}, log)
}

// WatchFunc observes the hooks directory and invokes onChange at
// startup and then, debounced, whenever a hook.json, the central
// concurrency.json, or a hook directory is added, modified, or removed.
// onChange owns the actual reload — this lets a caller fold the
// concurrency-group config and registry update into place rather than
// the watcher reloading hooks on its own.
//
// The watcher debounces rapid-fire events (common when an editor saves via
// a write-rename pattern) by waiting briefly after each event before
// firing. Edits to a hook's own baked-in code/assets are ignored: they
// only matter at image-build time, which happens on the next run.
//
// WatchFunc blocks until ctx is canceled. It returns nil on graceful
// shutdown or an error if the watcher cannot be initialized.
func WatchFunc(ctx context.Context, root string, onChange func(), log *slog.Logger) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := addRecursive(w, root); err != nil {
		return err
	}

	onChange()

	const debounce = 200 * time.Millisecond
	var pending *time.Timer

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// New folders need to be added to the watch set so we
			// notice their hook.json. Removed folders fall out
			// automatically.
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.Add(ev.Name)
				}
			}
			if !isRelevantEvent(ev.Name) {
				continue
			}
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(debounce, onChange)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Error("watcher error", "err", err)
		}
	}
}

// isRelevantEvent reports whether a filesystem event should trigger a reload.
func isRelevantEvent(name string) bool {
	base := filepath.Base(name)
	if base == "hook.json" || base == "concurrency.json" || base == "manager.json" {
		return true
	}
	return filepath.Ext(base) == ""
}

// addRecursive adds the directories whose config files drive reloads. For a legacy tree that is the root plus every immediate child (hook.json is level down). For a src-layout tree it additionally covers src/, src/hooks/ and its children, and cfg/ at the root (concurrency.json's home; addChildren(root) usually covers it already — the explicit Add states intent, and a cfg/ created later arrives via the Create handler like any new dir under the watched root). src/sdk is deliberately NOT watched: shared-code edits matter at image-build time (they change content hashes, so the next run rebuilds) — they don't change the loaded config.
func addRecursive(w *fsnotify.Watcher, root string) error {
	if err := w.Add(root); err != nil {
		return err
	}
	addChildren := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				_ = w.Add(filepath.Join(dir, e.Name()))
			}
		}
	}
	addChildren(root)
	if l := DetectLayout(root); l.SDK {
		_ = w.Add(l.SrcDir())
		_ = w.Add(l.HooksDir())
		addChildren(l.HooksDir())
		// Managers live beside hooks under src/; their manager.json edits drive reloads exactly like hook.json (a managers dir created later.
		_ = w.Add(l.ManagersDir())
		addChildren(l.ManagersDir())
		_ = w.Add(filepath.Join(root, "cfg"))
	}
	return nil
}
