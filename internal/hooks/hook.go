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

const DefaultSignatureHeader = "X-Signature-Ed25519"
const LegacySignatureHeader = "X-Hub-Signature-256"
const DefaultAPIKeyHeader = "X-API-Key"

// Script configures a hook to run a script file from the hook directory without spelling out the command: the interpreter determines it (e.g.
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

	// SrcRoot is the absolute path of the hooks repo's src/ directory when this hook was loaded from the src (SDK) layout, "" for legacy hooks.
	SrcRoot     string `json:"-"`
	Schema      string `json:"$schema,omitempty"`
	Description string `json:"description"`
	// Base names a shared image under src/base/<name>/ that this entity's build is given as the BASE_IMAGE build arg. See BaseDir.
	Base     string     `json:"base,omitempty"`
	Command  []string   `json:"command,omitempty"`
	Script   *Script    `json:"script,omitempty"`
	Tests    [][]string `json:"tests,omitempty"`
	Networks []string   `json:"networks,omitempty"`
	Volumes  []string   `json:"volumes,omitempty"`
	// Devices are --device passthroughs (host device node -> container node,
	Devices []string `json:"devices,omitempty"`
	// Settings is the hook's OWN configuration: arbitrary JSON this runner
	Settings json.RawMessage `json:"settings,omitempty"`
	// manifestSettings preserves the document as hook.json declared it, before any operator override was merged into Settings.
	manifestSettings json.RawMessage `json:"-"`
	// manifestRaw is the whole comment-stripped hook.json this Hook parsed from, kept so the admin config view can render the document as.
	manifestRaw json.RawMessage `json:"-"`
	User        string          `json:"user,omitempty"`
	Workdir     string          `json:"workdir,omitempty"`

	// TimeoutRaw, when set, is the absolute processing ceiling: the run is killed it has been processing this long, regardless of output.
	TimeoutRaw string `json:"timeout,omitempty"`

	// IdleTimeoutRaw, when set, kills a run its container has produced NO output (stdout or stderr) for this long — a progress-aware timeout.
	IdleTimeoutRaw string `json:"idle_timeout,omitempty"`

	GitHubStatus *GitHubStatusConfig `json:"github_status,omitempty"`

	// ConcurrencyGroup, when set, names a concurrency group the hook's runs must be scheduled through: at most that group's limit run at and.
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`

	APIKey       string `json:"api_key,omitempty"`
	APIKeyHeader string `json:"api_key_header,omitempty"`

	PublicKey       string `json:"public_key,omitempty"`
	SignatureHeader string `json:"signature_header,omitempty"`

	// Legacy HMAC-SHA — prefer api_key or public_key.
	Secret string `json:"secret,omitempty"`

	// Synchronous, when true, makes the server hold the HTTP connection open until the container exits (subject to its timeout).
	Synchronous bool `json:"synchronous,omitempty"`

	// Enable, when explicitly false, loads the hook DISABLED by default: deliveries are rejected () and scheduled runs are skipped exactly.
	Enable *bool `json:"enable,omitempty"`

	// State, when true, opts the hook into the persistent KV store: the runner bind-mounts the KV API's Unix socket into the container and injects.
	State bool `json:"state,omitempty"`

	// Dind, when true, gives the hook's container STORAGE a nested container daemon can use: an anonymous volume at /var/lib/docker (--mount.
	Dind bool `json:"dind,omitempty"`

	// Seccomp narrows the container's syscall filter policy.
	Seccomp *SeccompConfig `json:"seccomp,omitempty"`

	// Schedule, when set, makes the scheduler fire this hook on a fixed interval (a Go duration, e.g. "m"), in addition to any HTTP trigger.
	Schedule string `json:"schedule,omitempty"`

	// SkipIf declares conditions under which an (authenticated) delivery is SKIPPED instead of run: answered immediately, recorded as a.
	SkipIf SkipConditions `json:"skip_if,omitempty"`

	// RunTitle, when set, is a template for the friendly display title of this hook's runs —.
	RunTitle string `json:"run_title,omitempty"`

	// titleTmpl is RunTitle parsed by validate() at load time, so rendering never re-parses and a malformed template can never load.
	titleTmpl *titleTemplate
}

// SeccompConfig is the `seccomp` block: named relaxation per field, so every syscall privilege a container gets is greppable and reviewable.
type SeccompConfig struct {
	// Userns, when true, allows the container to create unprivileged user namespaces: the runner passes a profile that is docker's default plus.
	Userns bool `json:"userns,omitempty"`
}

// UsernsAllowed reports whether the hook opted into unprivileged user namespaces.
func (h *Hook) UsernsAllowed() bool {
	return h != nil && h.Seccomp != nil && h.Seccomp.Userns
}

// ManifestJSON returns the comment-stripped hook.json this Hook parsed from.
func (h *Hook) ManifestJSON() json.RawMessage {
	if h == nil {
		return nil
	}
	return h.manifestRaw
}

// GitHubStatusConfig configures the optional GitHub commit status update
// posted before and after a hook run.
type GitHubStatusConfig struct {
	Enabled   bool   `json:"enabled"`
	Context   string `json:"context"`
	TargetURL string `json:"target_url,omitempty"`
}

// Timeout returns the parsed timeout, or when the hook sets none (no absolute ceiling — the run is bounded only by its idle_timeout, if set, or by the.
func (h *Hook) Timeout() time.Duration {
	if h.TimeoutRaw == "" {
		return 0
	}
	d, err := time.ParseDuration(h.TimeoutRaw)
	if err != nil {
		return 0
	}
	return d
}

// IdleTimeout returns the parsed idle timeout, or when the hook sets none (no idle limit — silence is bounded only by the total timeout, if is set).
func (h *Hook) IdleTimeout() time.Duration {
	if h.IdleTimeoutRaw == "" {
		return 0
	}
	d, err := time.ParseDuration(h.IdleTimeoutRaw)
	if err != nil {
		return 0
	}
	return d
}

// EnabledByDefault reports the hook.json `enable` default: true unless the hook explicitly sets "enable": false.
func (h *Hook) EnabledByDefault() bool {
	return h.Enable == nil || *h.Enable
}

// ScheduleInterval returns the parsed schedule duration, or when the hook is not scheduled.
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

// SigHeader returns the configured signature header or a sensible default based on the auth method.
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

// Dir returns the absolute path of the directory containing this hook's hook.json, or "" for hooks not loaded from disk (tests).
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

// SDKLayout reports whether this hook was loaded from the src (SDK) layout — see internal/hooks/layout.go.
func (h *Hook) SDKLayout() bool { return h.SrcRoot != "" }

// BuildContext is the docker build context for this hook's image: the hook's own directory under the legacy layout, the repo's src/.
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
	// Keep the comment-stripped manifest so the admin view can show the hook's config AS AUTHORED (filtered through a key whitelist) rather than.
	if stripped, err := io.ReadAll(stripComments(data)); err == nil {
		h.manifestRaw = stripped
	}
	if err := h.resolveScript(); err != nil {
		return nil, err
	}
	if !h.hasDockerfile() {
		return nil, errors.New("hook must ship a Dockerfile next to hook.json (every hook runs an image built from its directory)")
	}
	if err := h.validate(); err != nil {
		return nil, err
	}
	// Then the PUBLISHED schema (see schemacheck.go) -- the contract a hooks repo validates against in CI, enforced here by the same.
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
	// Checked HERE, before the args are folded into Command: the author wrote script.args, so that is the key the error must name (see.
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

// ContentHash digests the files that determine this hook's image, tagging the build so a changed hook rebuilds on its next run while an unchanged reuses the already built image. LEGACY layout: every file under the hook's directory, hashed as relative path + content — byte-identical to the historical algorithm (existing deployments must not re-tag on upgrade). SDK (src/) layout: a deterministic walk of src/hooks/<id>/ AND every SHARED dir (see SharedDirs — src/sdk, src/actions-runner, whatever the tree has) — never sibling entity dirs — hashed as src-relative path + file mode + content. A shared-code edit re-tags every src-layout entity (lazy rebuild on its next run, intended even for non-consumers); an edit to hook A never re-tags hook B.
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
		// A src tree without shared code is fine: no shared dirs simply contribute nothing.
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
	// The hook's OWN configuration, checked against the contract it ships (settings.schema.json).
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
	if h.IdleTimeoutRaw != "" {
		d, err := time.ParseDuration(h.IdleTimeoutRaw)
		if err != nil {
			return fmt.Errorf("invalid idle_timeout %q: %w", h.IdleTimeoutRaw, err)
		}
		if d <= 0 {
			return fmt.Errorf("idle_timeout must be positive, got %s", d)
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
	// A manifest is not a place to write shell (see shellsafe.go): nested command/process substitution in an argv entry is a load error.
	if err := checkNoShellSubstitution("command", h.Command); err != nil {
		return err
	}
	// Compiles every skip_if regex too, so evaluation never compiles at
	// request time and a bad pattern can never load.
	if err := h.SkipIf.compile(); err != nil {
		return err
	}
	// Same rule for the run_title template: parsed here, , so a
	// malformed is a load error — never a silently titleless run.
	if err := h.compileRunTitle(); err != nil {
		return err
	}
	if err := h.validateAuth(); err != nil {
		return err
	}
	if h.GitHubStatus != nil && h.GitHubStatus.Enabled && h.GitHubStatus.Context == "" {
		return errors.New("github_status.context is required when github_status.enabled is true")
	}
	// --privileged runs seccomp UNCONFINED, so a profile passed beside it NARROWS
	// the container that dind exists to widen -- the opposite of what declaring
	// both reads as.
	if h.Dind && h.UsernsAllowed() {
		return errors.New("dind and seccomp.userns are mutually exclusive: dind already runs the container with seccomp unconfined, and a profile alongside it narrows the container instead")
	}
	return nil
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

// stripComments returns a reader over the input with // and /* */ comments removed, since the hook.json format documented to users contains.
func stripComments(in []byte) *strings.Reader {
	return jsonc.NewReader(in)
}
