// Package githubstatus posts commit status updates to the GitHub API as
// hooks start and finish.
package githubstatus

import (
	"encoding/json"
)

// ParseRepoSHA extracts the GitHub repository ("owner/repo") and the
// relevant commit SHA from a webhook payload.
//
// The SHA is searched in this order:
// . top-level "after" (push events)
// . "pull_request.head.sha" (pull_request events)
// . "check_suite.head_sha" (check_suite events)
// . "head_commit.id" (legacy push)
//
// repository.full_name is required. Missing values return empty strings;
// callers should treat empty as "skip the GitHub update for this run".
func ParseRepoSHA(payload []byte) (repo, sha string) {
	var raw struct {
		After      string `json:"after"`
		HeadCommit *struct {
			ID string `json:"id"`
		} `json:"head_commit"`
		PullRequest *struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		CheckSuite *struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_suite"`
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return "", ""
	}
	if raw.Repository != nil {
		repo = raw.Repository.FullName
	}
	switch {
	case raw.After != "" && !isZeroSHA(raw.After):
		sha = raw.After
	case raw.PullRequest != nil && raw.PullRequest.Head.SHA != "":
		sha = raw.PullRequest.Head.SHA
	case raw.CheckSuite != nil && raw.CheckSuite.HeadSHA != "":
		sha = raw.CheckSuite.HeadSHA
	case raw.HeadCommit != nil && raw.HeadCommit.ID != "":
		sha = raw.HeadCommit.ID
	}
	return repo, sha
}

// isZeroSHA returns true when s is the all- SHA GitHub sends on
// branch deletion events (we don't want to post a status to that).
func isZeroSHA(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}
