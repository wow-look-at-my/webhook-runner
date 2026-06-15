package hooks

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch observes the hooks directory and replaces the registry from disk
// on every change. It is a thin wrapper over WatchFunc for callers that
// only need the default "reload the registry" behavior.
//
// Watch blocks until ctx is canceled. It returns nil on graceful shutdown
// or an error if the watcher cannot be initialized.
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

// WatchFunc observes the hooks directory and invokes onChange once at
// startup and then, debounced, whenever a hook.json, the central
// concurrency.json, or a hook directory is added, modified, or removed.
// onChange owns the actual reload — this lets a caller fold the
// concurrency-group config and registry update into one place rather than
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

// isRelevantEvent reports whether a filesystem event should trigger a
// reload. The config files (hook.json and the central concurrency.json)
// and directory-level changes matter; edits to a hook's own code/assets
// (anything else with a file extension, e.g. a *.ts script) do not — those
// are baked into the image at build time, on the next run.
func isRelevantEvent(name string) bool {
	base := filepath.Base(name)
	if base == "hook.json" || base == "concurrency.json" {
		return true
	}
	return filepath.Ext(base) == ""
}

// addRecursive adds the root directory and every immediate child directory
// to the watcher. We don't go deeper than that — hook.json is always one
// level under the root.
func addRecursive(w *fsnotify.Watcher, root string) error {
	if err := w.Add(root); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_ = w.Add(filepath.Join(root, e.Name()))
	}
	return nil
}
