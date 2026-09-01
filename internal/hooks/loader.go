package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadDir detects the tree's layout (see layout.go) and loads every hook
func LoadDir(root string) (map[string]*Hook, []error) {
	return LoadLayout(DetectLayout(root))
}

// LoadLayout walks the layout's hooks directory and loads each child
// folder with a hook.json (others are skipped). The result is keyed by
// hook ID; hooks loaded before a later failure are still returned.
// ZeroHooksError and IgnoredLegacyDirError can ride the error list — see
// their own doc comments for when each fires.
func LoadLayout(l Layout) (map[string]*Hook, []error) {
	hooks := make(map[string]*Hook)
	var errs []error

	dir := l.HooksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		errs = append(errs, fmt.Errorf("read hooks dir %s: %w", dir, err))
		return hooks, errs
	}

	// Absolute, to match Hook.Dir().
	srcRoot := ""
	if l.SDK {
		srcRoot = l.SrcDir()
		if abs, err := filepath.Abs(srcRoot); err == nil {
			srcRoot = abs
		}
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if id == "" || id[0] == '.' {
			continue
		}
		h, err := LoadOne(dir, id)
		if err != nil {
			if errors.Is(err, errNoHookJSON) {
				continue
			}
			errs = append(errs, HookLoadError{HookID: id, Err: err})
			continue
		}
		if l.SDK {
			h.SrcRoot = srcRoot
		}
		hooks[id] = h
	}

	if l.SDK {
		errs = append(errs, findIgnoredLegacyDirs(l)...)
	}
	if len(hooks) == 0 {
		errs = append(errs, ZeroHooksError{Dir: l.Root, Layout: l.String()})
	}
	return hooks, errs
}

// HookLoadError attributes hook's load/validation failure to its
// hook ID, so the attention aggregator can name the offending hook.
type HookLoadError struct {
	HookID string
	Err    error
}

func (e HookLoadError) Error() string { return fmt.Sprintf("hook %q: %v", e.HookID, e.Err) }
func (e HookLoadError) Unwrap() error { return e.Err }

// ZeroHooksError: a hooks root yielded no hooks at all. Loud on purpose —
// see LoadLayout.
type ZeroHooksError struct {
	Dir    string
	Layout string
}

func (e ZeroHooksError) Error() string {
	return fmt.Sprintf("no hooks loaded from %s (%s layout): an empty or mis-laid-out tree serves nothing — expected %s, or <root>/src/hooks/<id>/hook.json for the src layout",
		e.Dir, e.Layout, "<root>/<id>/hook.json")
}

// IgnoredLegacyDirError fires when the src layout is active
type IgnoredLegacyDirError struct {
	Dir string // the offending top-level hook dir (absolute or as-given path)
}

func (e IgnoredLegacyDirError) Error() string {
	return fmt.Sprintf("mixed hook layout: top-level hook directory %s is not allowed when src/hooks/ exists (src layout active) — it is NOT loaded and this is a hard error, not a silent skip; move it under src/hooks/<id>/ or delete it", e.Dir)
}

// findIgnoredLegacyDirs names every root-level dir that looks like a
// legacy hook (contains hook.json) while the src layout is active — the
// MIXED-layout guard. It is called ONLY from LoadLayout under l.SDK, so a
// pure-legacy tree (no src/hooks/) never reaches it and can never trip
// the error. Each hit becomes an IgnoredLegacyDirError, a hard load error
// (fails validate, logged + recorded on every serve reload) — a stray
// top-level hook must be loud, not silently dropped.
func findIgnoredLegacyDirs(l Layout) []error {
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		return nil // the hooks dir itself already loaded; root scan is advisory
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "src" || e.Name() == "" || e.Name()[0] == '.' {
			continue
		}
		if _, err := os.Stat(filepath.Join(l.Root, e.Name(), "hook.json")); err == nil {
			errs = append(errs, IgnoredLegacyDirError{Dir: filepath.Join(l.Root, e.Name())})
		}
	}
	return errs
}

var errNoHookJSON = errors.New("no hook.json")

// LoadOne loads a single hook by ID from the given hooks directory (the
// layout's HooksDir, NOT necessarily the tree root). The returned error
// wraps errNoHookJSON if hook.json does not exist, allowing LoadLayout to
// skip non-hook folders.
func LoadOne(root, id string) (*Hook, error) {
	p := filepath.Join(root, id, "hook.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoHookJSON
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return Parse(id, p, data)
}
