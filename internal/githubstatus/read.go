package githubstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrNoContextStatus reports that a commit's combined status carries no
// entry for the requested context — CI may not have started yet, or the
// status poster never ran for that commit.
var ErrNoContextStatus = errors.New("github_status: no status for context")

// ContextState fetches the combined commit status for sha in repo
// ("owner/name") and returns the current state of the given context:
// "success", "pending", "failure", or "error". The combined status
// endpoint carries the LATEST status per context, so no dedup is needed.
//
// Errors are authoritative "could not determine" answers — callers gating
// on the result must fail closed: a disabled client (no token) errors
// immediately (commit statuses on a private repo are unreadable without
// one), and a context with no status returns an error wrapping
// ErrNoContextStatus. The single page at per_page=100 covers any
// realistic context count; a context squeezed past it reads as missing,
// which also fails closed.
func (c *Client) ContextState(ctx context.Context, repo, sha, statusContext string) (string, error) {
	if c == nil || c.token == "" {
		return "", errors.New("github_status: no GitHub token configured (WEBHOOK_RUNNER_GITHUB_TOKEN)")
	}
	u := fmt.Sprintf("%s/repos/%s/commits/%s/status?per_page=100", c.apiURL, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("github_status: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github_status: combined status for %s@%s: %w", repo, sha, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("github_status: combined status for %s@%s: HTTP %d: %s",
			repo, sha, resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	var body struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("github_status: combined status for %s@%s: parse: %w", repo, sha, err)
	}
	for _, s := range body.Statuses {
		if s.Context == statusContext {
			return s.State, nil
		}
	}
	return "", fmt.Errorf("%w: %q on %s@%s", ErrNoContextStatus, statusContext, repo, sha)
}

// RepoFromGitURL derives the "owner/repo" slug from a git remote URL —
// the scp-like SSH form (git@github.com:owner/repo.git) and the URL forms
// (https://github.com/owner/repo[.git], ssh://git@github.com/owner/repo).
// Returns ok=false for anything no slug can be derived from (file://
// URLs, bare local paths, malformed input).
func RepoFromGitURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	var path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "file" || u.Host == "" {
			return "", false
		}
		path = u.Path
	case strings.Contains(s, "@") && strings.Contains(s, ":"):
		// scp-like: git@host:owner/repo(.git)
		path = s[strings.Index(s, ":")+1:]
	default:
		return "", false
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}
