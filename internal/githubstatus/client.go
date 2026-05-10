package githubstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// State is the GitHub commit-status state value.
type State string

const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	StateError   State = "error"
)

// Client posts commit-status updates. A nil token disables posting (the
// methods become no-ops, which is convenient when no hook actually needs
// status updates).
type Client struct {
	token  string
	http   *http.Client
	log    *slog.Logger
	apiURL string // override for tests; defaults to https://api.github.com
}

// New returns a Client that uses the given personal-access or fine-grained
// token. If token is empty the client still works but every Post is a
// silent no-op.
func New(token string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		token:  token,
		http:   &http.Client{Timeout: 15 * time.Second},
		log:    log,
		apiURL: "https://api.github.com",
	}
}

// Enabled reports whether the client will actually make HTTP calls.
func (c *Client) Enabled() bool { return c != nil && c.token != "" }

// SetAPIURL overrides the API base URL. Tests use it to point at an
// httptest server.
func (c *Client) SetAPIURL(u string) { c.apiURL = strings.TrimRight(u, "/") }

// PostStart sends a "pending" status. The repo + sha are looked up from
// the payload; if either is missing, the call is silently skipped.
func (c *Client) PostStart(ctx context.Context, hook *hooks.Hook, run *runs.Run, payload []byte) {
	if !c.shouldPost(hook) {
		return
	}
	repo, sha := ParseRepoSHA(payload)
	if repo == "" || sha == "" {
		c.log.Debug("github_status: missing repo/sha, skipping",
			"hook", hook.ID, "run", run.ID())
		return
	}
	desc := fmt.Sprintf("Hook %s started", hook.ID)
	c.post(ctx, hook, run, repo, sha, StatePending, desc)
}

// PostFinish sends success / failure / error based on the run's terminal
// status. The description includes a truncated tail of the run output.
func (c *Client) PostFinish(ctx context.Context, hook *hooks.Hook, run *runs.Run, payload []byte) {
	if !c.shouldPost(hook) {
		return
	}
	repo, sha := ParseRepoSHA(payload)
	if repo == "" || sha == "" {
		return
	}
	state := StateSuccess
	switch run.Status() {
	case runs.StatusSuccess:
		state = StateSuccess
	case runs.StatusFailure:
		state = StateFailure
	case runs.StatusTimeout, runs.StatusError:
		state = StateError
	default:
		state = StateError
	}
	desc := buildDescription(hook.ID, run)
	c.post(ctx, hook, run, repo, sha, state, desc)
}

func (c *Client) shouldPost(hook *hooks.Hook) bool {
	if c == nil || c.token == "" {
		return false
	}
	if hook.GitHubStatus == nil || !hook.GitHubStatus.Enabled {
		return false
	}
	return true
}

func (c *Client) post(ctx context.Context, hook *hooks.Hook, run *runs.Run, repo, sha string, state State, desc string) {
	url := fmt.Sprintf("%s/repos/%s/statuses/%s", c.apiURL, repo, sha)
	body := map[string]string{
		"state":       string(state),
		"context":     hook.GitHubStatus.Context,
		"description": truncate(desc, 140),
	}
	if u := renderTargetURL(hook, run); u != "" {
		body["target_url"] = u
	}
	buf, err := json.Marshal(body)
	if err != nil {
		c.log.Error("github_status marshal", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		c.log.Error("github_status request", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("github_status post", "err", err, "repo", repo, "sha", sha)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.log.Error("github_status non-2xx",
			"status", resp.StatusCode,
			"repo", repo, "sha", sha, "body", strings.TrimSpace(string(excerpt)))
		return
	}
	c.log.Info("github_status posted",
		"hook", hook.ID, "run", run.ID(),
		"repo", repo, "sha", sha, "state", state)
}

func buildDescription(hookID string, run *runs.Run) string {
	tail := strings.Join(run.LastLines(3), " | ")
	switch run.Status() {
	case runs.StatusSuccess:
		if tail == "" {
			return fmt.Sprintf("%s succeeded", hookID)
		}
		return fmt.Sprintf("%s succeeded: %s", hookID, tail)
	case runs.StatusTimeout:
		return fmt.Sprintf("%s timed out: %s", hookID, tail)
	case runs.StatusFailure:
		return fmt.Sprintf("%s exit %d: %s", hookID, run.ExitCode(), tail)
	case runs.StatusError:
		return fmt.Sprintf("%s error: %s", hookID, firstNonEmpty(run.Error(), tail))
	}
	return fmt.Sprintf("%s %s", hookID, run.Status())
}

func renderTargetURL(hook *hooks.Hook, run *runs.Run) string {
	if hook.GitHubStatus == nil || hook.GitHubStatus.TargetURL == "" {
		return ""
	}
	tmpl, err := template.New("target").Parse(hook.GitHubStatus.TargetURL)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		HookID, RunID string
	}{HookID: hook.ID, RunID: run.ID()}); err != nil {
		return ""
	}
	return buf.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func firstNonEmpty(parts ...string) string {
	for _, p := range parts {
		if p != "" {
			return p
		}
	}
	return ""
}

// ErrDisabled is returned by callers that try to use a Client without a
// configured token. Reserved for future strict modes; the current code
// path silently skips instead.
var ErrDisabled = errors.New("github_status: client disabled (no token)")
