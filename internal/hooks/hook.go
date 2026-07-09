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

// DefaultTimeout is applied when a hook does not specify one explicitly.
const DefaultTimeout = 5 * time.Minute

// DockerfileName is the file every hook must ship next to its hook.json:
// hooks run images built from their own directory, code baked in.
const DockerfileName = "Dockerfile"

const DefaultSignatureHeader = "X-Signature-Ed25519"
const LegacySignatureHeader = "X-Hub-Signature-256"
const DefaultAPIKeyHeader = "X-API-Key"

// Hook is the parsed in-memory representation of a single hook.json file.
//
// The ID is derived from the parent directory name and is not part of the
// JSON document.
type Hook struct {
	ID          string `json:"-"`
	SourcePath  string `json:"-"`
	Schema      string `json:"$schema,omitempty"`
	Description string `json:"description"`
	// Command optionally overrides the image's CMD. Every hook runs the
	// image built from its directory's Dockerfile (tagged by content
	// hash), so code is baked in and immutable per run.
	Command []string `json:"command,omitempty"`
	// Tests are argv arrays run by `webhook-runner test` in this hook's
	// built image, so tests exercise the exact baked code. They never run
	// when the hook is triggered.
	Tests      [][]string        `json:"tests,omitempty"`
	Networks   []string          `json:"networks,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	Workdir    string            `json:"workdir,omitempty"`
	TimeoutRaw string            `json:"timeout,omitempty"`

	// IdleTimeoutRaw, when set, kills a run once its container has produced
	// NO output (stdout or stderr) for this long — a progress-aware timeout
	// for hooks whose healthy runtime varies too much for a tight total
	// ceiling (a hook that logs progress every few seconds may legitimately
	// run for an hour). Any output byte resets the idle clock. It follows
	// the same arming rule as timeout: the clock starts only once the
	// concurrency-group slot is acquired and the container launches, so a
	// queued run never idles out (see runner.execute). Independent of
	// timeout — both may be set, and whichever fires first kills the run
	// with status "timeout" (an idle kill carries a distinguishable
	// "idle timeout ... (no output)" message). Empty means no idle limit.
	//
	// Like state/concurrency_group/schedule, idle_timeout is a newer
	// hook.json field, so Parse's DisallowUnknownFields means old binaries
	// reject it — deploy a webhook-runner that supports it before merging a
	// hook that sets it.
	IdleTimeoutRaw string `json:"idle_timeout,omitempty"`

	ExtraDockerArgs []string            `json:"extra_docker_args,omitempty"`
	GitHubStatus    *GitHubStatusConfig `json:"github_status,omitempty"`

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

	// State, when true, opts the hook into the persistent KV store: the
	// runner bind-mounts the KV API's Unix socket into the container and
	// injects HOOK_KV_SOCKET, HOOK_KV_URL, and HOOK_KV_TOKEN (a per-hook
	// bearer token scoped to a namespace == this hook's ID), so the hook
	// reaches the state API over that socket — no networking. The hook's data
	// survives across its runs and across server restarts, isolated from
	// every other hook. Omitted (the default) means no KV access.
	State bool `json:"state,omitempty"`

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
}

// GitHubStatusConfig configures the optional GitHub commit status update
// posted before and after a hook run.
type GitHubStatusConfig struct {
	Enabled   bool   `json:"enabled"`
	Context   string `json:"context"`
	TargetURL string `json:"target_url,omitempty"`
}

// Timeout returns the parsed timeout, falling back to DefaultTimeout when
// not set. Validation has already happened at load time, so the parse here
// cannot fail.
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

// IdleTimeout returns the parsed idle timeout, or 0 when the hook sets none
// (no idle limit — silence is bounded only by the total timeout). Validation
// has already happened at load time, so a parse failure here is treated as
// "no idle limit" rather than panicking.
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
	if !h.hasDockerfile() {
		return nil, errors.New("hook must ship a Dockerfile next to hook.json (every hook runs an image built from its directory)")
	}
	if err := h.validate(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hook) hasDockerfile() bool {
	dir := h.Dir()
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, DockerfileName))
	return err == nil && !fi.IsDir()
}

// ContentHash digests every file under the hook's directory (relative
// path + content). It tags the image built for a Dockerfile hook, so a
// changed hook rebuilds on its next run while an unchanged one reuses
// the already built image.
func (h *Hook) ContentHash() (string, error) {
	dir := h.Dir()
	if dir == "" {
		return "", errors.New("hook has no source directory")
	}
	digest := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "%s\x00", filepath.ToSlash(rel))
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
	if err != nil {
		return "", fmt.Errorf("hash hook dir %s: %w", dir, err)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

// ReservedEnvKey reports whether the runner sets this env key itself; hook
// env entries must not declare it and secrets-file entries are skipped.
func ReservedEnvKey(k string) bool {
	switch k {
	case "HOOK_PAYLOAD_FILE", "HOOK_HEADERS_FILE", "HOOK_ID", "HOOK_RUN_ID",
		"HOOK_KV_URL", "HOOK_KV_TOKEN", "HOOK_KV_SOCKET":
		return true
	}
	return false
}

func (h *Hook) validate() error {
	if h.Schema == "" {
		return errors.New("$schema is required (point it at https://wow-look-at-my.github.io/webhook-runner/hook.schema.json)")
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
	for k := range h.Env {
		if ReservedEnvKey(k) {
			return fmt.Errorf("env key %q is reserved", k)
		}
	}
	if err := h.validateAuth(); err != nil {
		return err
	}
	if h.GitHubStatus != nil && h.GitHubStatus.Enabled && h.GitHubStatus.Context == "" {
		return errors.New("github_status.context is required when github_status.enabled is true")
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

// stripComments returns a reader over the input with // and /* */ comments
// removed, since the hook.json format documented to users contains
// JSONC-style comments. The shared implementation lives in internal/jsonc.
func stripComments(in []byte) *strings.Reader {
	return jsonc.NewReader(in)
}
