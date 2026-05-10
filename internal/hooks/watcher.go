package hooks

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch starts a goroutine that observes the hooks directory and updates
// the registry as hook.json files are added, modified, or removed.
//
// The watcher debounces rapid-fire events (common when an editor saves via
// a write-rename pattern) by waiting briefly after each event before
// reloading. Reloads always re-scan the entire directory; this is simpler
// than tracking per-folder state and remains cheap for any reasonable
// number of hooks.
//
// Watch blocks until ctx is canceled. It returns nil on graceful shutdown
// or an error if the watcher cannot be initialized.
func Watch(ctx context.Context, root string, reg *Registry, log *slog.Logger) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := addRecursive(w, root); err != nil {
		return err
	}

	reload := func() {
		hooks, errs := LoadDir(root)
		for _, e := range errs {
			log.Error("hook reload error", "err", e)
		}
		reg.Replace(hooks)
		log.Info("hooks reloaded", "count", len(hooks))
	}
	reload()

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
			// Ignore events on files that are clearly unrelated
			// (e.g. editor swap files). hook.json + the parent
			// directory itself are the relevant inputs.
			base := filepath.Base(ev.Name)
			if base != "hook.json" && !strings.EqualFold(filepath.Ext(base), "") {
				if !isHookJSONEvent(ev.Name) {
					continue
				}
			}
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(debounce, reload)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Error("watcher error", "err", err)
		}
	}
}

func isHookJSONEvent(p string) bool {
	return filepath.Base(p) == "hook.json"
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
