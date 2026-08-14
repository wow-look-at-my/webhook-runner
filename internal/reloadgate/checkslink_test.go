package reloadgate

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
)

// withChecks decorates a held-commit message with the commit's CI
// run-details URL. It is presentation only: no repo slug (a local path, an
// unparseable remote) or no sha must leave the message byte-identical, so a
// deployment that cannot build the link still gets the full message.
func TestWithChecks(t *testing.T) {
	sha := "7ba043715036c8a9f0d1e2b3a4c5d6e7f8091a2b"

	g := &Gate{repoSlug: "wow-look-at-my/webhooks"}
	assert.Equal(t,
		"all-builds failure — https://github.com/wow-look-at-my/webhooks/commit/"+sha+"/checks",
		g.withChecks("all-builds failure", sha))

	// The full sha, never the abbreviation the message text carries: the
	// link must stay unambiguous even though the prose is shortened.
	assert.Contains(t, g.withChecks("x", sha), sha)

	for name, gate := range map[string]*Gate{
		"no slug": {repoSlug: ""},
		"slug":    {repoSlug: "wow-look-at-my/webhooks"},
	} {
		if gate.repoSlug == "" {
			assert.Equal(t, "held", gate.withChecks("held", sha), "%s: message must survive untouched", name)
		}
		assert.Equal(t, "held", gate.withChecks("held", ""), "%s: an empty sha yields no link", name)
	}
}

// The red-hold path is what an operator actually reads in the banner and the
// feed, so prove the link reaches BOTH — not just the helper in isolation.
func TestRedHeldMessageCarriesChecksLink(t *testing.T) {
	repo := &fakeRepo{tip: "B", commits: []string{"B", "A"}}

	rec := events.NewRecorder(100)
	agg := attention.New()
	g, err := New(Config{
		Repo:      repo,
		Branch:    "master",
		Context:   "all-builds",
		StatePath: filepath.Join(t.TempDir(), "reload-gate.json"),
		Apply:     func() error { return nil },
		RepoSlug:  "wow-look-at-my/webhooks",
		Events:    rec,
		Attention: agg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	g.servingSHA, g.verified = "A", true

	_, err = g.HandleEvent("push", pushBody(t, "refs/heads/master", "B"))
	require.NoError(t, err)
	_, err = g.HandleEvent("status", statusBody(t, "B", "failure", "all-builds", "master"))
	require.NoError(t, err)

	want := "https://github.com/wow-look-at-my/webhooks/commit/B/checks"

	keys := attentionKeys(agg)
	require.Contains(t, keys, attention.KeyReloadHeld)
	assert.Contains(t, keys[attention.KeyReloadHeld], want, "the red banner must link the held commit")

	var held string
	for _, ev := range rec.List(0) {
		if ev.Kind == "reload.held_red" {
			held = ev.Msg
		}
	}
	require.NotEmpty(t, held, "expected a reload.held_red event")
	assert.Contains(t, held, want, "the activity feed line must link the held commit")
}
