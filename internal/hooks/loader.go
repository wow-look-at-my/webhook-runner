package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadDir detects the tree's layout (see layout.go) and loads every hook
// in it. Shorthand for LoadLayout(DetectLayout(root)) — serve, validate,
// and test all go through this, so the detection rule cannot drift
// between them.
func LoadDir(root string) (map[string]*Hook, []error) {
	return LoadLayout(DetectLayout(root))
}

// LoadLayout walks the layout's hooks directory and loads every immediate
// child folder that contains a hook.json file. Folders without hook.json
// are silently skipped so the same directory can hold non-hook artifacts
// (README, scripts, etc.).
//
// The returned map is keyed by hook ID (the folder name). On error, any
// hooks that loaded successfully before the failure are still returned —
// callers can decide whether to use them.
//
// Two loud, typed error conditions ride in the error list:
//
//   - ZeroHooksError when NOTHING loaded. A hooks root that yields zero
//     hooks is almost always a layout mistake (e.g. a src/-restructured
//     tree served by a binary predating layout detection scans the root,
//     finds only a "src" folder, and would otherwise take the whole fleet
//     offline with a green validate). Zero hooks must never be silent:
//     validate exits non-zero on it and serve records an error-grade
//     event.
//   - IgnoredLegacyDirError for each root-level hook directory found
//     while the src layout is active. Layouts are never mixed: the dir is
//     NOT loaded, and the error names it so the misplacement is visible
//     instead of a hook quietly vanishing from the registry.
func LoadLayout(l Layout) (map[string]*Hook, []error) {
	hooks := make(map[string]*Hook)
	var errs []error

	dir := l.HooksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		errs = append(errs, fmt.Errorf("read hooks dir %s: %w", dir, err))
		return hooks, errs
	}

	// SrcRoot is stored absolute, matching Hook.Dir()'s convention, so
	// hashing and builds behave identically however the root was given.
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

// HookLoadError attributes one hook directory's load/validation failure
// (unparseable hook.json, missing Dockerfile or $schema, malformed
// skip_if/run_title, …) to its hook ID, so consumers that retain load
// errors — the attention aggregator's "needs attention" surface — can pin
// the problem on the hook instead of string-parsing. The rendered text is
// unchanged from the historical `hook %q: %v` wrap.
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

// IgnoredLegacyDirError: the src layout is active but a root-level
// directory still carries a hook.json. Layouts are never mixed, so it was
// skipped — loudly, because a silently vanishing hook is exactly the
// failure mode layout detection exists to prevent.
type IgnoredLegacyDirError struct {
	Dir string // the skipped directory (absolute or as-given path)
}

func (e IgnoredLegacyDirError) Error() string {
	return fmt.Sprintf("ignored legacy-shaped hook dir %s: this tree uses the src layout (src/hooks exists), so root-level hook dirs are skipped — move it under src/hooks/ or delete it", e.Dir)
}

// findIgnoredLegacyDirs names every root-level dir that looks like a
// legacy hook (contains hook.json) while the src layout is active.
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
