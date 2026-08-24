package reloadgate

import "fmt"

// withChecks appends the commit's /checks-tab link to msg, so an operator
// can jump straight to why a held sha is stuck. It returns msg unchanged
// when the repo slug or sha is empty.
func (g *Gate) withChecks(msg, sha string) string {
	if g.repoSlug == "" || sha == "" {
		return msg
	}
	// Full sha, not the abbreviation msg carries, so the link is unambiguous.
	return fmt.Sprintf("%s — https://github.com/%s/commit/%s/checks", msg, g.repoSlug, sha)
}
