// Package concurrency implements named, centrally-declared concurrency
// groups for hook runs — think GitHub Actions concurrency, but the set of
// valid group names is fixed by file at the hooks root rather than an
// arbitrary per-job expression.
//
// A hook opts into a group via hook.json's "concurrency_group". At most
// that group's limit run at ; the rest queue. A hook that names no
// group runs unbounded. A hook that names a group NOT declared in
// concurrency.json is a load/validation error — the whole point is that
// groups are a defined, shared resource, not a free-for-all.
//
// The file at <hooks-root>/concurrency.json looks like:
//
//	{
//
// "$schema": "https://sites.pazer.build/webhook-runner/branch/master/concurrency.schema.json",
// "groups": {
// "ollama-local": { "description": "...", "limit": }
// }
//
//	}
//
// limit defaults to (full serialization) and must be >= .
package concurrency

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wow-look-at-my/webhook-runner/internal/jsonc"
)

// FileName is the central concurrency-groups file, read from the hooks root (the same directory hook folders live under).
const FileName = "concurrency.json"

// DefaultLimit is a group's max concurrency when it doesn't set .
const DefaultLimit = 1

// Group is declared concurrency group.
type Group struct {
	// Limit is the maximum number of runs in this group that may execute at ; the rest queue. Defaults to DefaultLimit, must be >= .
	Limit int `json:"limit,omitempty"`
	// Description is optional human-facing documentation for the group.
	Description string `json:"description,omitempty"`
}

// Config is the parsed concurrency.json document.
type Config struct {
	Schema string           `json:"$schema,omitempty"`
	Groups map[string]Group `json:"groups,omitempty"`
}

// Parse decodes and validates a concurrency.json document. Unknown fields
// are rejected (typo protection), JSONC comments are allowed, omitted
// limits default to DefaultLimit, and every limit must be >= .
func Parse(data []byte) (*Config, error) {
	dec := json.NewDecoder(jsonc.NewReader(data))
	dec.DisallowUnknownFields()
	c := &Config{}
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", FileName, err)
	}
	if c.Groups == nil {
		c.Groups = map[string]Group{}
	}
	for name, g := range c.Groups {
		if name == "" {
			return nil, fmt.Errorf("%s: group name must not be empty", FileName)
		}
		if g.Limit == 0 {
			g.Limit = DefaultLimit
			c.Groups[name] = g
		}
		if g.Limit < 0 {
			return nil, fmt.Errorf("%s: group %q limit must be >= 1, got %d", FileName, name, g.Limit)
		}
	}
	return c, nil
}

// Load reads <root>/concurrency.json — the LEGACY location.
func Load(root string) (*Config, error) {
	return LoadFile(filepath.Join(root, FileName))
}

// LoadFile reads a concurrency.json at an explicit path (the hooks layout decides where that is: <root>/concurrency.json for legacy trees, <root>/cfg/concurrency.json for the src layout).
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Groups: map[string]Group{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data)
}

// Has reports whether a group with the given name is declared.
func (c *Config) Has(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.Groups[name]
	return ok
}

// Limit returns the declared limit for a group, or when it isn't declared.
func (c *Config) Limit(name string) int {
	if c == nil {
		return 0
	}
	return c.Groups[name].Limit
}

// Names returns the declared group names, sorted.
func (c *Config) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Groups))
	for name := range c.Groups {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RefError reports a hook that references an undeclared concurrency group.
type RefError struct {
	HookID string
	Group  string
}

func (e RefError) Error() string {
	return fmt.Sprintf("hook %q references undeclared concurrency group %q (declare it in %s or remove the reference)",
		e.HookID, e.Group, FileName)
}

// CheckRefs validates that every hook's referenced group is declared in
// cfg. refs maps hook ID -> referenced group name; entries with an empty
// group (unbounded hooks) are ignored. The result is sorted by hook ID so
// callers get deterministic output.
func CheckRefs(cfg *Config, refs map[string]string) []RefError {
	var bad []RefError
	for id, group := range refs {
		if group == "" {
			continue
		}
		if !cfg.Has(group) {
			bad = append(bad, RefError{HookID: id, Group: group})
		}
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].HookID < bad[j].HookID })
	return bad
}
