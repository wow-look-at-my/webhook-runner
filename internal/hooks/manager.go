package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Manager is the parsed manager.json: a persistent, single-instance
type Manager struct {
	*Hook

	// The manager.json reconcile_interval: a Go duration for synthetic
	ReconcileIntervalRaw string

	// The manager.json spawn_targets: hook ids this manager may start via
	SpawnTargets []string
}

// AllowedSpawnTarget reports whether this manager's manifest grants
// spawning the target hook. Nil-safe; exact id match.
func (m *Manager) AllowedSpawnTarget(target string) bool {
	for _, t := range m.SpawnTargets {
		if t == target {
			return true
		}
	}
	return false
}

// CheckSpawnTargets verifies every manager's spawn_targets against the
// loaded set: each entry must name a declared HOOK (spawn targets are
// always hooks; managers are supervised, never spawnable). Violations are
// load/validation errors — the referencing manager is dropped by callers,
// the undeclared-concurrency-group rule. Run this against the
// post-rejection sets so a target dropped for its own errors invalidates
// its referrers too.
func CheckSpawnTargets(loaded map[string]*Hook, managers map[string]*Manager) []ManagerLoadError {
	var errs []ManagerLoadError
	for id, m := range managers {
		for _, t := range m.SpawnTargets {
			if _, ok := loaded[t]; ok {
				continue
			}
			if _, isManager := managers[t]; isManager {
				errs = append(errs, ManagerLoadError{ManagerID: id,
					Err: fmt.Errorf("spawn_targets entry %q names a manager: only hooks are spawnable", t)})
				continue
			}
			errs = append(errs, ManagerLoadError{ManagerID: id,
				Err: fmt.Errorf("spawn_targets entry %q does not name a declared hook", t)})
		}
	}
	return errs
}

// EnabledByDefault is inherited from the embedded Hook: absent `enable`
// means enabled, exactly like hooks; the persisted dashboard override
// outranks the default in both directions.

// ReconcileInterval returns the parsed cadence, or for event-only.
// Load-time validation means the parse here can't fail in practice.
func (m *Manager) ReconcileInterval() time.Duration {
	if m.ReconcileIntervalRaw == "" {
		return 0
	}
	d, err := time.ParseDuration(m.ReconcileIntervalRaw)
	if err != nil {
		return 0
	}
	return d
}

// ManagerFileName is the manifest every manager ships, next to its Dockerfile.
const ManagerFileName = "manager.json"

// managerJSON is the exact manager.json field set — the FULL hook set
// minus `state` (implied true) and `schedule` (superseded by
// reconcile_interval), plus reconcile_interval. A separate decode struct
// (rather than reusing Hook's) so DisallowUnknownFields rejects exactly
// those , loudly, instead of half-accepting a pasted hook.json.
type managerJSON struct {
	Schema      string `json:"$schema"`
	Description string `json:"description"`
	Enable      *bool  `json:"enable"`

	ReconcileInterval string `json:"reconcile_interval"`
	Timeout           string `json:"timeout"`

	Command  []string        `json:"command"`
	Script   *Script         `json:"script"`
	Tests    [][]string      `json:"tests"`
	Networks []string        `json:"networks"`
	Volumes  []string        `json:"volumes"`
	Devices  []string        `json:"devices"`
	Settings json.RawMessage `json:"settings"`
	User     string          `json:"user"`
	Workdir  string          `json:"workdir"`
	Dind     bool            `json:"dind"`
	Seccomp  *SeccompConfig  `json:"seccomp"`

	ConcurrencyGroup string              `json:"concurrency_group"`
	RunTitle         string              `json:"run_title"`
	GitHubStatus     *GitHubStatusConfig `json:"github_status"`
	Synchronous      bool                `json:"synchronous"`
	SpawnTargets     []string            `json:"spawn_targets"`

	APIKey          string `json:"api_key"`
	APIKeyHeader    string `json:"api_key_header"`
	PublicKey       string `json:"public_key"`
	SignatureHeader string `json:"signature_header"`
	Secret          string `json:"secret"`

	SkipIf SkipConditions `json:"skip_if"`
}

// ParseManager decodes a manager.json document and validates the resulting
// manager. Like hooks.Parse: the id comes from the directory name, the
// Dockerfile is mandatory, $schema is required (presence only), skip_if
// compiles at load, and the auth trio is mutually exclusive.
func ParseManager(id, sourcePath string, data []byte) (*Manager, error) {
	dec := json.NewDecoder(stripComments(data))
	dec.DisallowUnknownFields()
	var mj managerJSON
	if err := dec.Decode(&mj); err != nil {
		return nil, fmt.Errorf("decode manager.json: %w", err)
	}

	h := &Hook{
		ID:         id,
		SourcePath: sourcePath,

		Schema:           mj.Schema,
		Description:      mj.Description,
		Enable:           mj.Enable,
		TimeoutRaw:       mj.Timeout,
		Command:          mj.Command,
		Script:           mj.Script,
		Tests:            mj.Tests,
		Networks:         mj.Networks,
		Volumes:          mj.Volumes,
		Devices:          mj.Devices,
		Settings:         mj.Settings,
		User:             mj.User,
		Workdir:          mj.Workdir,
		Dind:             mj.Dind,
		Seccomp:          mj.Seccomp,
		ConcurrencyGroup: mj.ConcurrencyGroup,
		RunTitle:         mj.RunTitle,
		GitHubStatus:     mj.GitHubStatus,
		Synchronous:      mj.Synchronous,
		APIKey:           mj.APIKey,
		APIKeyHeader:     mj.APIKeyHeader,
		PublicKey:        mj.PublicKey,
		SignatureHeader:  mj.SignatureHeader,
		Secret:           mj.Secret,
		SkipIf:           mj.SkipIf,

		// Implied: a manager can't function without the state socket.
		State: true,
	}
	m := &Manager{Hook: h, ReconcileIntervalRaw: mj.ReconcileInterval, SpawnTargets: mj.SpawnTargets}

	if err := h.resolveScript(); err != nil {
		return nil, err
	}
	if !h.hasDockerfile() {
		return nil, errors.New("manager must ship a Dockerfile next to manager.json (a manager runs an image built from its directory)")
	}
	// The shared validation set: $schema presence, timeout parse, reserved
	// env keys, skip_if compile, auth exclusivity, test-entry shape.
	if err := h.validate(); err != nil {
		return nil, err
	}
	for _, t := range mj.SpawnTargets {
		if strings.TrimSpace(t) == "" {
			return nil, errors.New("spawn_targets entries must be non-empty hook ids")
		}
	}
	if mj.ReconcileInterval != "" {
		d, err := time.ParseDuration(mj.ReconcileInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid reconcile_interval %q: %w", mj.ReconcileInterval, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("reconcile_interval must be positive, got %s", d)
		}
	}
	// Same published-schema gate as hooks; see schemacheck.go.
	if err := ValidateManagerJSON(sourcePath, data); err != nil {
		return nil, err
	}
	return m, nil
}

// ManagerLoadError attributes manager's load/validation failure to
// its id — the HookLoadError analog for the attention surface.
type ManagerLoadError struct {
	ManagerID string
	Err       error
}

func (e ManagerLoadError) Error() string {
	return fmt.Sprintf("manager %q: %v", e.ManagerID, e.Err)
}
func (e ManagerLoadError) Unwrap() error { return e.Err }

var errNoManagerJSON = errors.New("no manager.json")

// LoadManagers walks the layout's managers directory (src/managers under
// the SDK layout; legacy trees have no managers) and loads every immediate
// child folder that contains a manager.json. Folders without are
// silently skipped, like the hooks loader. managers is NOT an error —
// managers are an optional entity (unlike ZeroHooksError: a hooks tree
// exists to serve hooks; a managers dir is often simply absent).
//
// ID-collision with hooks is NOT checked here (this loader doesn't see the
// hook set); the reload path checks it and drops the colliding manager
// loudly.
func LoadManagers(l Layout) (map[string]*Manager, []error) {
	managers := make(map[string]*Manager)
	dir := l.ManagersDir()
	if dir == "" {
		return managers, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return managers, nil
		}
		return managers, []error{fmt.Errorf("read managers dir %s: %w", dir, err)}
	}

	srcRoot := l.SrcDir()
	if abs, err := filepath.Abs(srcRoot); err == nil {
		srcRoot = abs
	}

	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if id == "" || id[0] == '.' {
			continue
		}
		m, err := loadOneManager(dir, id)
		if err != nil {
			if errors.Is(err, errNoManagerJSON) {
				continue
			}
			errs = append(errs, ManagerLoadError{ManagerID: id, Err: err})
			continue
		}
		// Same build-context/hash semantics as hooks: hash covers src/sdk too.
		m.SrcRoot = srcRoot
		managers[id] = m
	}
	return managers, errs
}

func loadOneManager(root, id string) (*Manager, error) {
	p := filepath.Join(root, id, ManagerFileName)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoManagerJSON
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return ParseManager(id, p, data)
}
