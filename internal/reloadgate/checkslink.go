package reloadgate

import "fmt"

// withChecks appends a commit's CI run-details link to a message that names a held sha. Every "held"/"unchanged" message answers "which commit is stuck", and the operator's very next question is "why" — without the link they have to hand-assemble the URL from a 12-char abbreviation.
func (g *Gate) withChecks(msg, sha string) string {
	if g.repoSlug == "" || sha == "" {
		return msg
	}
	// The commit's /checks tab, not a specific run: it lists every check on the sha — exactly the aggregate the gating context summarizes — and.
	return fmt.Sprintf("%s — https://github.com/%s/commit/%s/checks", msg, g.repoSlug, sha)
}
