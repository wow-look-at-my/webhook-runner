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
//     while the src layout is active — a MIXED layout. Layouts are never
//     mixed: the dir is NOT loaded, and the error is a HARD failure, never
//     a silent skip — it rides this error list, so validate exits
//     non-zero and every serve reload logs + records it (hook.load_error).
//     It fires ONLY when src/hooks/ exists, so a pure-legacy tree is never
//     scanned for it and is completely unaffected.
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
			errs = append(errs, fmt.Errorf("hook %q: %w", id, err))
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

// IgnoredLegacyDirError: the src layout is active (<root>/src/hooks/
// exists) but a root-level directory still carries a hook.json — a MIXED
// layout. This is a HARD ERROR, never a silent skip: the dir is NOT
// loaded into the registry AND the error rides the load-error list, so
// `validate` exits non-zero and every `serve` reload logs + records it
// (hook.load_error). Naming each offending dir is the failsafe the
// operator asked for — a stray top-level hook left behind by an
// incomplete move to the src layout turns CI RED instead of quietly
// vanishing from the fleet. (The type name predates this framing; the
// dir is "ignored" only in the sense of not-loaded — it is emphatically
// not overlooked.)
//
// SCOPE: this fires ONLY on a mixed layout. It is emitted exclusively
// from findIgnoredLegacyDirs, which LoadLayout calls only when l.SDK is
// true (src/hooks/ present). A pure-legacy tree with no src/hooks/
// sibling — e.g. this repo's own examples/hooks/ and e2e/hooks/ fixtures
// — is never scanned for it and stays 100% valid.
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
