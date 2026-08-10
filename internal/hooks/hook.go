// Package hooks defines the hook configuration model and the in-memory
// registry of loaded hooks.
package hooks

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/jsonc"
)

// DefaultTimeout is applied when a hook does not specify one explicitly:
// five minutes of no container output kills the run (timeout is
// activity-based — see Hook.TimeoutRaw), so every hook has hang protection
// by default.
const DefaultTimeout = 5 * time.Minute

// DockerfileName is the file every hook must ship next to its hook.json:
// hooks run images built from their own directory, code baked in.
const DockerfileName = "Dockerfile"

const DefaultSignatureHeader = "X-Signature-Ed25519"
const LegacySignatureHeader = "X-Hub-Signature-256"
const DefaultAPIKeyHeader = "X-API-Key"

// Script configures a hook to run a script file from the hook directory
// without spelling out the command: the interpreter determines it
// (e.g. "tsx <file>"). The script is baked into the hook's image like all
// hook code, so the interpreter must be installed in that image. An
// explicit Command overrides the derived one.
type Script struct {
	File        string   `json:"file"`
	Interpreter string   `json:"interpreter"`
	Args        []string `json:"args,omitempty"`
}

// Hook is the parsed in-memory representation of a single hook.json file.
//
// The ID is derived from the parent directory name and is not part of the
// JSON document.
type Hook struct {
	ID         string `json:"-"`
	SourcePath string `json:"-"`

	// SrcRoot is the absolute path of the hooks repo's src/ directory when
	// this hook was loaded from the src (SDK) layout, "" for legacy hooks.
	// Set by the loader, never by JSON. It selects the docker build context
	// (src/ instead of the hook dir) and widens the content hash to include
	// src/sdk — see BuildContext and ContentHash.
	SrcRoot     string     `json:"-"`
	Schema      string     `json:"$schema,omitempty"`
	Description string     `json:"description"`
	Command     []string   `json:"command,omitempty"`
	Script      *Script    `json:"script,omitempty"`
	Tests       [][]string `json:"tests,omitempty"`
	Networks    []string   `json:"networks,omitempty"`
	Volumes     []string   `json:"volumes,omitempty"`
	// Settings is the hook's OWN configuration: arbitrary JSON this runner
	// never interprets, validated at load against the settings.schema.json
	// shipped next to the manifest, and handed to the container as a file
	// (HOOK_SETTINGS_FILE). It replaces the old `env` block, which mixed
	// hook-private config into the runner's own parsed keys. See settings.go.
	Settings json.RawMessage `json:"settings,omitempty"`
	// manifestSettings preserves the document as hook.json declared it,
	// before any operator override was merged into Settings. The editor
	// needs it to answer "what would revert restore?" — reading Settings
	// there would show the override itself. Unexported and json:"-": it is
	// derived state, never part of the manifest contract.
	manifestSettings json.RawMessage     `json:"-"`
	User             string              `json:"user,omitempty"`
	Workdir          string              `json:"workdir,omitempty"`
	TimeoutRaw       string              `json:"timeout,omitempty"`
	ExtraDockerArgs  []string            `json:"extra_docker_args,omitempty"`
	GitHubStatus     *GitHubStatusConfig `json:"github_status,omitempty"`

	// ConcurrencyGroup, when set, names a concurrency group the hook's runs
	// must be scheduled through: at most that group's limit run at once and
	// the rest queue (staying "pending" with their timeout NOT yet counting
	// — see runner.execute). The group must be declared in the central
	// concurrency.json at the hooks root; referencing an undeclared group is
	// a load/validation error. Empty means unbounded (no queueing).
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`

	APIKey       string `json:"api_key,omitempty"`
	APIKeyHeader string `json:"api_key_header,omitempty"`

	PublicKey       string `json:"public_key,omitempty"`
	SignatureHeader string `json:"signature_header,omitempty"`

	// Legacy HMAC-SHA256 — prefer api_key or public_key.
	Secret string `json:"secret,omitempty"`

	// Synchronous, when true, makes the server hold the HTTP connection
	// open until the container exits (subject to its timeout). When false
	// (the default), the server returns 202 immediately and the caller
	// must poll /runs/{run_id} for completion. The query parameter
	// ?wait=true on a request also forces synchronous behavior.
	Synchronous bool `json:"synchronous,omitempty"`

	// Enable, when explicitly false, loads the hook DISABLED by default:
	// deliveries are rejected (503) and scheduled runs are skipped exactly
	// as if the operator kill switch were flipped off — until an operator
	// explicitly enables it (the dashboard switch / POST /hooks/{id}/enable,
	// a persisted runtime override that always wins over this default, in
	// both directions). Absent (nil) or true means enabled by default, so
	// existing hooks are unchanged. Like the other newer hook.json fields,
	// old binaries reject it via DisallowUnknownFields: deploy a
	// webhook-runner that supports it before merging a hook that sets it.
	Enable *bool `json:"enable,omitempty"`

	// State, when true, opts the hook into the persistent KV store: the
	// runner bind-mounts the KV API's Unix socket into the container and
	// injects HOOK_KV_SOCKET, HOOK_KV_URL, and HOOK_KV_TOKEN (a per-hook
	// bearer token scoped to a namespace == this hook's ID), so the hook
	// reaches the state API over that socket — no networking. The hook's data
	// survives across its runs and across server restarts, isolated from
	// every other hook. Omitted (the default) means no KV access.
	State bool `json:"state,omitempty"`

	// Dind, when true, grants the hook's container the privileges to run its
	// OWN nested container daemon: the runner adds --privileged and an
	// anonymous volume at /var/lib/docker (--mount
	// type=volume,dst=/var/lib/docker), so a dockerd started inside the
	// container has container-local storage on a real filesystem (an inner
	// daemon can't run its overlay storage driver on top of the outer
	// container's overlay — /var/lib/docker must be a volume, not the layered
	// rootfs). The host's own docker daemon is NEVER exposed — no docker
	// socket is mounted; the nested daemon is fully isolated from it. --rm
	// (always passed) auto-removes the anonymous volume when the run ends, so
	// inner storage never leaks between runs. The SAME two flags apply on the
	// `webhook-runner test` path, so a dind hook's declared tests can start a
	// nested daemon too.
	//
	// This is a host-root-equivalent capability (--privileged) — enable it
	// only for trusted, operator-curated hooks. Like the other newer hook.json
	// fields, old binaries reject it via DisallowUnknownFields: deploy a
	// webhook-runner that supports it before merging a hook that sets it.
	Dind bool `json:"dind,omitempty"`

	// Scratch names absolute CONTAINER paths whose writes must land on the
	// operator's scratch filesystem (WEBHOOK_RUNNER_SCRATCH_DIR) instead of
	// under docker's data-root. Each listed path is bind-mounted from a
	// per-run directory that is removed when the run ends.
	//
	// Declaring a path here is a REQUIREMENT, not a hint: with no scratch
	// root configured the run fails rather than quietly writing to the disk
	// the declaration exists to spare. A dind hook that lists
	// /var/lib/docker gets its inner daemon's store from scratch, and the
	// anonymous volume is not added — two mounts on one destination is a
	// docker error.
	//
	// see docs/internals/scratch-dirs.md
	Scratch []string `json:"scratch,omitempty"`

	// Tmpfs names absolute CONTAINER paths backed by RAM (--tmpfs) instead of
	// disk. The companion to Scratch: scratch takes what must survive on a
	// disk or is too big for memory, tmpfs takes the rest, and between them a
	// hook can account for every path it writes to.
	//
	// An entry may carry docker's option suffix ("/tmp:size=4g"). SIZE IT: an
	// unbounded tmpfs may grow to half of host RAM, and several on one
	// container — times the concurrency limit — can take the host down. See
	// TmpfsPath.
	Tmpfs []string `json:"tmpfs,omitempty"`

	// ReadOnlyRootfs runs the container with --read-only, so the ONLY writable
	// locations are the mounts above. Without it, "the heavy paths are on
	// scratch" is a claim about the paths somebody remembered to list: any
	// other write still lands in the container's writable layer under docker's
	// data-root, silently. With it, an unlisted write fails loudly instead.
	//
	// It is opt-in because it can only be proven per image — a program that
	// writes somewhere unlisted breaks under it. Prove it in the hook's own
	// `tests`, which run with these same flags.
	ReadOnlyRootfs bool `json:"read_only_rootfs,omitempty"`

	// Schedule, when set, makes the scheduler fire this hook on a fixed
	// interval (a Go duration, e.g. "5m"), in addition to any HTTP trigger. A
	// scheduled run is dispatched through the exact same pipeline as an
	// HTTP-triggered one — tracked, gated by the hook's concurrency_group,
	// KV-enabled, shown on the dashboard — with a synthetic payload that marks
	// it as schedule-triggered. To stop a long sweep stacking on itself, the
	// scheduler skips a tick whenever a previous run of the same hook is still
	// in flight (skip-if-already-running). On startup (and when newly added or
	// when its interval changes) the hook fires immediately, then every
	// interval thereafter. Empty (the default) means HTTP-triggered only.
	//
	// Like state/concurrency_group, schedule is a newer hook.json field, so
	// Parse's DisallowUnknownFields means old binaries reject it — deploy a
	// webhook-runner that supports it before merging a hook that sets it.
	Schedule string `json:"schedule,omitempty"`

	// SkipIf declares conditions under which an (authenticated) delivery is
	// SKIPPED instead of run: answered immediately, recorded as a
	// first-class run with status "skipped" naming the matched condition,
	// and given NO container — no image build, no concurrency slot, no
	// docker run. List entries are ORed; keys within one condition are
	// ANDed. Keys address the parsed JSON payload by dotted path or a
	// request header via the "header:" prefix; matchers are a bare string
	// (equality) or {eq,ne,in,exists,prefix,regex} — see skip.go. Malformed
	// conditions (bad regex, unknown operator, empty condition) fail the
	// hook's load/validation. Like state/concurrency_group/schedule this is
	// a newer hook.json field: deploy a webhook-runner that supports it
	// before merging a hook that sets it (old binaries reject it via
	// DisallowUnknownFields).
	SkipIf SkipConditions `json:"skip_if,omitempty"`

	// RunTitle, when set, is a template for the friendly display title of
	// this hook's runs — "{{repository.full_name}}#{{pull_request.number}}"
	// renders "wow-look-at-my/go-toolchain#47" on the dashboard where the
	// opaque run id used to be. {{...}} placeholders name a dotted payload
	// path or a request header via the "header:" prefix — skip_if's exact
	// key syntax and bounded traversal (see title.go for the resolution
	// semantics: graceful, never blocking, all-placeholders-empty means no
	// title). Resolved once at run creation, BEFORE skip evaluation, so
	// skipped runs are titled too; a malformed template is a load/validation
	// error. Like the other newer hook.json fields, old binaries reject it
	// via DisallowUnknownFields: deploy a webhook-runner that supports it
	// before merging a hook that sets it.
	RunTitle string `json:"run_title,omitempty"`

	// titleTmpl is RunTitle parsed by validate() at load time, so rendering
	// never re-parses and a malformed template can never load. Hooks
	// constructed in code (tests) may leave it nil — RenderRunTitle then
	// parses on demand.
	titleTmpl *titleTemplate
}

// GitHubStatusConfig configures the optional GitHub commit status update
// posted before and after a hook run.
type GitHubStatusConfig struct {
	Enabled   bool   `json:"enabled"`
	Context   string `json:"context"`
	TargetURL string `json:"target_url,omitempty"`
}

// Timeout returns the parsed no-output (activity) timeout, falling back to
// DefaultTimeout when not set. Validation has already happened at load
// time, so the parse here cannot fail.
func (h *Hook) Timeout() time.Duration {
	if h.TimeoutRaw == "" {
		return DefaultTimeout
	}
	d, err := time.ParseDuration(h.TimeoutRaw)
	if err != nil {
		return DefaultTimeout
	}
	return d
}

// EnabledByDefault reports the hook.json `enable` default: true unless the
// hook explicitly sets "enable": false. This is only the DEFAULT position
// of the kill switch — a persisted operator override (internal/overrides)
// takes precedence over it everywhere.
func (h *Hook) EnabledByDefault() bool {
	return h.Enable == nil || *h.Enable
}

// ScheduleInterval returns the parsed schedule duration, or 0 when the hook
// is not scheduled. Validation has already happened at load time, so a parse
// failure here is treated as "not scheduled" rather than panicking.
func (h *Hook) ScheduleInterval() time.Duration {
	if h.Schedule == "" {
		return 0
	}
	d, err := time.ParseDuration(h.Schedule)
	if err != nil {
		return 0
	}
	return d
}

// SigHeader returns the configured signature header or a sensible default
// based on the auth method. For ed25519 public_key auth it defaults to
// X-Signature-Ed25519; for legacy HMAC it defaults to X-Hub-Signature-256.
func (h *Hook) SigHeader() string {
	if h.SignatureHeader != "" {
		return h.SignatureHeader
	}
	if h.Secret != "" {
		return LegacySignatureHeader
	}
	return DefaultSignatureHeader
}

func (h *Hook) APIKeyHdr() string {
	if h.APIKeyHeader != "" {
		return h.APIKeyHeader
	}
	return DefaultAPIKeyHeader
}

// Dir returns the absolute path of the directory containing this hook's
// hook.json, or "" for hooks not loaded from disk (tests). For Dockerfile
// hooks it is the docker build context, so code and assets ship alongside
// hook.json and get baked into the image.
func (h *Hook) Dir() string {
	if h.SourcePath == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Dir(h.SourcePath))
	if err != nil {
		return ""
	}
	return abs
}

// SDKLayout reports whether this hook was loaded from the src (SDK)
// layout — see internal/hooks/layout.go.
func (h *Hook) SDKLayout() bool { return h.SrcRoot != "" }

// BuildContext is the docker build context for this hook's image: the
// hook's own directory under the legacy layout, the repo's src/ directory
// under the SDK layout (so Dockerfiles COPY with the tree-mirror
// convention — `COPY sdk/ /app/sdk/` + `COPY hooks/<id>/ /app/hooks/<id>/`
// — and a hook's relative ../../sdk import resolves identically in-repo
// and in-image). The Dockerfile itself is always the hook's own (the
// runner passes -f for SDK builds).
func (h *Hook) BuildContext() string {
	if h.SrcRoot != "" {
		return h.SrcRoot
	}
	return h.Dir()
}

// Parse decodes a hook.json document and validates the resulting hook.
// The id and sourcePath are not part of the JSON; the caller supplies
// them based on the file's location on disk.
func Parse(id, sourcePath string, data []byte) (*Hook, error) {
	dec := json.NewDecoder(stripComments(data))
	dec.DisallowUnknownFields()
	h := &Hook{}
	if err := dec.Decode(h); err != nil {
		return nil, fmt.Errorf("decode hook.json: %w", err)
	}
	h.ID = id
	h.SourcePath = sourcePath
	if err := h.resolveScript(); err != nil {
		return nil, err
	}
	if !h.hasDockerfile() {
		return nil, errors.New("hook must ship a Dockerfile next to hook.json (every hook runs an image built from its directory)")
	}
	if err := h.validate(); err != nil {
		return nil, err
	}
	// Then the PUBLISHED schema (see schemacheck.go) -- the contract a hooks
	// repo validates against in CI, enforced here by the same implementation.
	// It runs LAST because the checks above produce better messages for what
	// they cover ("invalid schedule 5 minutes" beats a pattern mismatch); what
	// it adds is everything a Go struct cannot express -- enums, patterns,
	// formats, minimums -- which until now was checked in CI and nowhere else.
	if err := ValidateHookJSON(sourcePath, data); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hook) resolveScript() error {
	if h.Script == nil {
		return nil
	}
	s := h.Script
	if s.File == "" {
		return errors.New("script.file is required")
	}
	// Checked HERE, before the args are folded into Command: the author wrote
	// script.args, so that is the key the error must name (see shellsafe.go).
	if err := checkNoShellSubstitution("script.args", s.Args); err != nil {
		return err
	}
	if s.Interpreter == "" {
		return errors.New("script.interpreter is required")
	}

	hookDir := filepath.Dir(h.SourcePath)
	scriptAbs := filepath.Join(hookDir, s.File)

	realScript, err := filepath.EvalSymlinks(scriptAbs)
	if err != nil {
		return fmt.Errorf("script.file %q: %w", s.File, err)
	}
	realHookDir, err := filepath.EvalSymlinks(hookDir)
	if err != nil {
		return fmt.Errorf("resolve hook directory: %w", err)
	}
	if !strings.HasPrefix(realScript, realHookDir+string(filepath.Separator)) {
		return fmt.Errorf("script.file %q resolves outside hook directory", s.File)
	}

	if len(h.Command) == 0 {
		switch s.Interpreter {
		case "bash":
			h.Command = append([]string{"bash", s.File}, s.Args...)
		case "pwsh":
			h.Command = append([]string{"pwsh", "-File", s.File}, s.Args...)
		case "node":
			h.Command = append([]string{"node", s.File}, s.Args...)
		case "tsx":
			h.Command = append([]string{"tsx", s.File}, s.Args...)
		default:
			return fmt.Errorf("unsupported script.interpreter %q (must be bash, pwsh, node, or tsx)", s.Interpreter)
		}
	}
	return nil
}

func (h *Hook) hasDockerfile() bool {
	dir := h.Dir()
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, DockerfileName))
	return err == nil && !fi.IsDir()
}

// ContentHash digests the files that determine this hook's image, tagging
// the build so a changed hook rebuilds on its next run while an unchanged
// one reuses the already built image.
//
// LEGACY layout: every file under the hook's directory, hashed as
// relative path + content — byte-identical to the historical algorithm
// (existing deployments must not re-tag on upgrade).
//
// SDK (src/) layout: a deterministic walk of src/hooks/<id>/ AND every
// SHARED dir (see SharedDirs — src/sdk, src/actions-runner, whatever the
// tree has) — never sibling entity dirs — hashed as src-relative path +
// file mode + content. A shared-code edit re-tags every src-layout entity
// (lazy rebuild on its next run, intended even for non-consumers); an edit
// to hook A never re-tags hook B. The COPY-surface convention follows from
// this: an SDK-layout Dockerfile may COPY only from a shared dir and its
// own hooks/<id>/ — a sibling entity's dir is undefined-staleness territory
// (builds don't fail, but edits there never re-tag).
func (h *Hook) ContentHash() (string, error) {
	dir := h.Dir()
	if dir == "" {
		return "", errors.New("hook has no source directory")
	}
	digest := sha256.New()
	if h.SrcRoot != "" {
		if err := hashTree(digest, h.SrcRoot, dir, true); err != nil {
			return "", fmt.Errorf("hash hook dir %s: %w", dir, err)
		}
		// A src tree without shared code is fine: no shared dirs simply
		// contribute nothing.
		shared, err := SharedDirs(h.SrcRoot)
		if err != nil {
			return "", fmt.Errorf("list shared dirs under %s: %w", h.SrcRoot, err)
		}
		for _, sd := range shared {
			if err := hashTree(digest, h.SrcRoot, sd, true); err != nil {
				return "", fmt.Errorf("hash shared dir %s: %w", sd, err)
			}
		}
		return hex.EncodeToString(digest.Sum(nil))[:16], nil
	}
	if err := hashTree(digest, dir, dir, false); err != nil {
		return "", fmt.Errorf("hash hook dir %s: %w", dir, err)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

// hashTree feeds every file under root into digest, ordered by
// filepath.WalkDir's lexical walk: relative-to-base path, optionally the
// file mode (the SDK layout hashes modes; legacy predates that and must
// stay byte-identical), then the content.
func hashTree(digest io.Writer, base, root string, withMode bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "%s\x00", filepath.ToSlash(rel))
		if withMode {
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(digest, "%o\x00", info.Mode().Perm())
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(digest, f)
		f.Close()
		if cpErr != nil {
			return cpErr
		}
		fmt.Fprint(digest, "\x00")
		return nil
	})
}

// ReservedEnvKey reports whether the runner sets this env key itself; hook
// env entries must not declare it and secrets-file entries are skipped.
func ReservedEnvKey(k string) bool {
	switch k {
	case "HOOK_PAYLOAD_FILE", "HOOK_HEADERS_FILE", "HOOK_SETTINGS_FILE", "HOOK_ID", "HOOK_RUN_ID",
		"HOOK_KV_URL", "HOOK_KV_TOKEN", "HOOK_KV_SOCKET":
		return true
	}
	return false
}

func (h *Hook) validate() error {
	if h.Schema == "" {
		return errors.New("$schema is required (point it at https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json)")
	}
	// The hook's OWN configuration, checked against the contract it ships
	// (settings.schema.json). Fail closed like every other load gate: a hook
	// configured wrongly must not run at all, because the alternative is a
	// container that starts, finds its config missing, and reports whatever it
	// decides to report.
	if err := h.ValidateSettings(); err != nil {
		return err
	}
	for i, tc := range h.Tests {
		if len(tc) == 0 {
			return fmt.Errorf("tests[%d] must not be empty", i)
		}
	}
	if h.TimeoutRaw != "" {
		d, err := time.ParseDuration(h.TimeoutRaw)
		if err != nil {
			return fmt.Errorf("invalid timeout %q: %w", h.TimeoutRaw, err)
		}
		if d <= 0 {
			return fmt.Errorf("timeout must be positive, got %s", d)
		}
	}
	if h.Schedule != "" {
		d, err := time.ParseDuration(h.Schedule)
		if err != nil {
			return fmt.Errorf("invalid schedule %q: %w", h.Schedule, err)
		}
		if d <= 0 {
			return fmt.Errorf("schedule must be positive, got %s", d)
		}
	}
	// A manifest is not a place to write shell (see shellsafe.go): nested
	// command/process substitution in an argv entry is a load error. script.args
	// is checked in resolveScript, before it becomes part of Command, so each
	// error names the key the author actually wrote.
	if err := checkNoShellSubstitution("command", h.Command); err != nil {
		return err
	}
	// Compiles every skip_if regex too, so evaluation never compiles at
	// request time and a bad pattern can never load.
	if err := h.SkipIf.compile(); err != nil {
		return err
	}
	// Same rule for the run_title template: parsed here, once, so a
	// malformed one is a load error — never a silently titleless run.
	if err := h.compileRunTitle(); err != nil {
		return err
	}
	if err := h.validateScratch(); err != nil {
		return err
	}
	if err := h.validateAuth(); err != nil {
		return err
	}
	if h.GitHubStatus != nil && h.GitHubStatus.Enabled && h.GitHubStatus.Context == "" {
		return errors.New("github_status.context is required when github_status.enabled is true")
	}
	return nil
}

// validateScratch rejects a mount entry that could not be applied, or that
// would mount somewhere destructive. A relative path has no meaning as a mount
// destination; "/" would replace the whole container rootfs; a destination
// named twice — within one list or across both — is two mounts on one target,
// which docker refuses. Catching all of it at LOAD keeps it out of a running
// fleet entirely.
func (h *Hook) validateScratch() error {
	seen := make(map[string]string, len(h.Scratch)+len(h.Tmpfs))
	for _, list := range []struct {
		field string
		paths []string
	}{{"scratch", h.Scratch}, {"tmpfs", h.Tmpfs}} {
		for i, entry := range list.paths {
			p := entry
			if list.field == "tmpfs" {
				p = TmpfsPath(entry)
				if p == entry && strings.Contains(entry, ":") {
					return fmt.Errorf("tmpfs[%d] %q has an empty option list after %q", i, entry, ":")
				}
			}
			if !strings.HasPrefix(p, "/") {
				return fmt.Errorf("%s[%d] %q must be an absolute container path", list.field, i, entry)
			}
			clean := filepath.Clean(p)
			if clean != p {
				return fmt.Errorf("%s[%d] %q must be a clean path (%q)", list.field, i, entry, clean)
			}
			if clean == "/" {
				return fmt.Errorf("%s[%d] must not be %q", list.field, i, "/")
			}
			if prev, dup := seen[clean]; dup {
				return fmt.Errorf("%s[%d] %q is already mounted by %s", list.field, i, entry, prev)
			}
			seen[clean] = list.field
		}
	}
	return nil
}

// TmpfsPath returns the mount destination of a tmpfs entry, which may carry
// docker's option suffix ("/tmp:size=4g"). Options are passed through
// untouched — docker owns that grammar, and validating a copy of it here would
// only reject options docker gains later.
//
// Sizing is not cosmetic: an unbounded tmpfs may grow to half of host RAM, and
// several of them on one container can exhaust it. A path expected to hold
// gigabytes should name a size.
func TmpfsPath(entry string) string {
	if i := strings.IndexByte(entry, ':'); i >= 0 && i < len(entry)-1 {
		return entry[:i]
	}
	return entry
}

// ScratchCovers reports whether the hook declared dst as a scratch path.
func (h *Hook) ScratchCovers(dst string) bool {
	for _, p := range h.Scratch {
		if p == dst {
			return true
		}
	}
	return false
}

func (h *Hook) validateAuth() error {
	n := 0
	if h.APIKey != "" {
		n++
	}
	if h.PublicKey != "" {
		n++
	}
	if h.Secret != "" {
		n++
	}
	if n > 1 {
		return errors.New("only one of api_key, public_key, or secret may be set")
	}
	if h.PublicKey != "" {
		if _, err := parseEd25519PublicKey(h.PublicKey); err != nil {
			return fmt.Errorf("invalid public_key: %w", err)
		}
	}
	return nil
}

func parseEd25519PublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		b, err = hex.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("not valid base64 or hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}

// stripComments returns a reader over the input with // and /* */ comments
// removed, since the hook.json format documented to users contains
// JSONC-style comments. The shared implementation lives in internal/jsonc.
func stripComments(in []byte) *strings.Reader {
	return jsonc.NewReader(in)
}
