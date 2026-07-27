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

// Manager is the parsed in-memory representation of a manager.json file — a
// persistent, single-instance watcher the runner supervises as ONE
// long-lived container (restarted flat on exit, exactly one instance
// fleet-wide via the supervisor's lease), as opposed to a hook's
// per-delivery containers.
//
// The model wraps a *Hook carrying the FULL hook feature set — auth trio,
// env, container knobs, skip_if, tests, timeout, script/command, dind,
// concurrency_group, run_title, github_status, synchronous — so every
// existing mechanism works on a manager, with manager-shaped semantics
// where the per-delivery-run concepts need remapping:
//
//   - timeout: the wedged-instance bound — the watchdog runs only while an
//     inbox event is checked out (or queued unconsumed); an idle parked
//     manager is never reaped.
//   - concurrency_group: the INSTANCE holds one slot of the group for its
//     whole session (acquired before the container starts, queued like any
//     run while the group is full, released when the session ends). Do not
//     share a group between a manager and bursty hooks unless that is the
//     intent — a persistent holder is a persistent slot.
//   - run_title: the INSTANCE's panel title template (rendered at session
//     start; the manager overrides it live via POST /title).
//   - synchronous: a DELIVERY is held until the manager finishes
//     processing that inbox event (its next /inbox/next call), then
//     answered; the hold degrades to the async 202 on the sync timeout,
//     exactly the hook rule.
//   - github_status: posted per DELIVERY — pending when the delivery is
//     accepted, success when the manager finishes processing its event,
//     error when the event is abandoned (dropped on overflow, or the
//     session died mid-event).
//   - schedule is the ONE hook field with no manager form: it is
//     SUPERSEDED by reconcile_interval (a schedule boots containers; a
//     manager is already running — the tick is its schedule).
//
// State is forced true (a manager cannot function without its inbox and
// KV); it is implied, never a field.
//
// Manager IDs share ONE namespace with hook IDs — the public endpoint
// (POST /hook/{id}), the KV namespace, the overrides store, and the spawn
// allowlist are all keyed by bare id, which is what makes a hook→manager
// migration a directory move with full state continuity. A collision is a
// load error (the loader side drops the manager, loudly).
type Manager struct {
	*Hook

	// ReconcileIntervalRaw is the manager.json reconcile_interval field: a
	// Go duration on which the runner enqueues synthetic {"kind":"tick"}
	// inbox events (flat cadence, coalesced, plus one at session start).
	// Empty means EVENT-ONLY: no ticks ever — the manager receives a
	// {"kind":"start"} event at session start and otherwise only
	// deliveries.
	ReconcileIntervalRaw string

	// SpawnTargets is the manager.json spawn_targets field: the hook ids
	// this manager may start via POST /spawn. THE manifest-sourced spawn
	// allowlist — deny-by-default (absent/empty = this manager spawns
	// nothing), loaded from the hooks tree like every other declaration,
	// so granting a spawn is a GitHub change, never host env. Entries must
	// name declared HOOKS (managers are supervised, never spawnable);
	// unknown ids fail load/validation loudly.
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

// ReconcileInterval returns the parsed reconcile cadence, or 0 for an
// event-only manager. Validation happened at load time, so a parse failure
// here reads as event-only rather than panicking.
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

// ManagerFileName is the declaration file every manager ships, next to its
// mandatory Dockerfile, under <root>/src/managers/<id>/.
const ManagerFileName = "manager.json"

// managerJSON is the exact manager.json field set — the FULL hook set
// minus `state` (implied true) and `schedule` (superseded by
// reconcile_interval), plus reconcile_interval. A separate decode struct
// (rather than reusing Hook's) so DisallowUnknownFields rejects exactly
// those two, loudly, instead of half-accepting a pasted hook.json.
type managerJSON struct {
	Schema      string `json:"$schema"`
	Description string `json:"description"`
	Enable      *bool  `json:"enable"`

	ReconcileInterval string `json:"reconcile_interval"`
	Timeout           string `json:"timeout"`

	Command         []string          `json:"command"`
	Script          *Script           `json:"script"`
	Tests           [][]string        `json:"tests"`
	Networks        []string          `json:"networks"`
	Volumes         []string          `json:"volumes"`
	Env             map[string]string `json:"env"`
	User            string            `json:"user"`
	Workdir         string            `json:"workdir"`
	ExtraDockerArgs []string          `json:"extra_docker_args"`
	Dind            bool              `json:"dind"`

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
		Env:              mj.Env,
		User:             mj.User,
		Workdir:          mj.Workdir,
		ExtraDockerArgs:  mj.ExtraDockerArgs,
		Dind:             mj.Dind,
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

		// A manager cannot function without the state socket (its inbox,
		// KV, locks, /spawn all ride it) — State is implied, never a field.
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
	return m, nil
}

// ManagerLoadError attributes one manager directory's load/validation
// failure to its id — the HookLoadError analog, so the attention surface
// can pin the problem without string-parsing.
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
// child folder that contains a manager.json. Folders without one are
// silently skipped, like the hooks loader. ZERO managers is NOT an error —
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
		// SDK build-context + content-hash semantics, exactly like hooks:
		// the docker context is src/, the hash covers managers/<id>/ plus
		// src/sdk (an sdk edit re-tags every manager AND every hook).
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
