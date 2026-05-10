package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadDir walks the given directory and loads every immediate child folder
// that contains a hook.json file. Folders without hook.json are silently
// skipped so the same directory can hold non-hook artifacts (e.g. README,
// scripts, etc.).
//
// The returned map is keyed by hook ID (the folder name). On error, any
// hooks that loaded successfully before the failure are still returned —
// callers can decide whether to use them.
func LoadDir(root string) (map[string]*Hook, []error) {
	hooks := make(map[string]*Hook)
	var errs []error

	entries, err := os.ReadDir(root)
	if err != nil {
		errs = append(errs, fmt.Errorf("read hooks dir %s: %w", root, err))
		return hooks, errs
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if id == "" || id[0] == '.' {
			continue
		}
		h, err := LoadOne(root, id)
		if err != nil {
			if errors.Is(err, errNoHookJSON) {
				continue
			}
			errs = append(errs, fmt.Errorf("hook %q: %w", id, err))
			continue
		}
		hooks[id] = h
	}
	return hooks, errs
}

var errNoHookJSON = errors.New("no hook.json")

// LoadOne loads a single hook by ID from the given root directory. The
// returned error wraps errNoHookJSON if hook.json does not exist, allowing
// LoadDir to skip non-hook folders.
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
