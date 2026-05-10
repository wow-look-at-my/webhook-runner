// Package hooks defines the hook configuration model and the in-memory
// registry of loaded hooks.
package hooks

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultTimeout is applied when a hook does not specify one explicitly.
const DefaultTimeout = 5 * time.Minute

const DefaultSignatureHeader = "X-Signature-Ed25519"
const LegacySignatureHeader = "X-Hub-Signature-256"
const DefaultAPIKeyHeader = "X-API-Key"

// Hook is the parsed in-memory representation of a single hook.json file.
//
// The ID is derived from the parent directory name and is not part of the
// JSON document.
type Hook struct {
	ID              string             `json:"-"`
	SourcePath      string             `json:"-"`
	Description     string             `json:"description"`
	Image           string             `json:"image"`
	Command         []string           `json:"command"`
	Networks        []string           `json:"networks,omitempty"`
	Volumes         []string           `json:"volumes,omitempty"`
	Env             map[string]string  `json:"env,omitempty"`
	User            string             `json:"user,omitempty"`
	Workdir         string             `json:"workdir,omitempty"`
	TimeoutRaw      string             `json:"timeout,omitempty"`
	ExtraDockerArgs []string           `json:"extra_docker_args,omitempty"`
	GitHubStatus    *GitHubStatusConfig `json:"github_status,omitempty"`

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
	if err := h.validate(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hook) validate() error {
	if h.Image == "" {
		return errors.New("image is required")
	}
	if len(h.Command) == 0 {
		return errors.New("command is required and must not be empty")
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
	for k := range h.Env {
		if k == "HOOK_PAYLOAD_FILE" || k == "HOOK_HEADERS_FILE" {
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
// removed, since the hook.json format documented to users contains JSONC-style
// comments. The implementation is intentionally simple and string-state aware:
// it preserves bytes inside string literals exactly.
func stripComments(in []byte) *strings.Reader {
	var out strings.Builder
	out.Grow(len(in))
	const (
		stateNormal = iota
		stateString
		stateLineComment
		stateBlockComment
	)
	state := stateNormal
	escape := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch state {
		case stateNormal:
			if c == '/' && i+1 < len(in) {
				switch in[i+1] {
				case '/':
					state = stateLineComment
					i++
					continue
				case '*':
					state = stateBlockComment
					i++
					continue
				}
			}
			if c == '"' {
				state = stateString
			}
			out.WriteByte(c)
		case stateString:
			out.WriteByte(c)
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				state = stateNormal
			}
		case stateLineComment:
			if c == '\n' {
				state = stateNormal
				out.WriteByte(c)
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(in) && in[i+1] == '/' {
				state = stateNormal
				i++
			}
		}
	}
	return strings.NewReader(out.String())
}
