package reloadgate

import "fmt"

// withChecks appends a commit's CI run-details link to a message that names
// a held sha. Every "held"/"unchanged" message answers "which commit is
// stuck", and the operator's very next question is "why" — without the link
// they have to hand-assemble the URL from a 12-char abbreviation.
//
// The message stays useful without it: an empty repo slug (a local path, an
// unparseable remote) or an empty sha returns the message untouched, so the
// link is never load-bearing for gating. It rides in the message TEXT rather
// than a structured field on purpose — that is the one form which reaches
// all three surfaces the message lands on (the slog line, the activity feed,
// the needs-attention banner), and the dashboard already linkifies URLs in
// any message it renders.
func (g *Gate) withChecks(msg, sha string) string {
	if g.repoSlug == "" || sha == "" {
		return msg
	}
	// The commit's /checks tab, not a specific run: it lists every check on
	// the sha — exactly the aggregate the gating context summarizes — and
	// needs no run id we would otherwise have to go look up. The FULL sha,
	// not the abbreviation the prose carries, so the link is unambiguous.
	return fmt.Sprintf("%s — https://github.com/%s/commit/%s/checks", msg, g.repoSlug, sha)
}
