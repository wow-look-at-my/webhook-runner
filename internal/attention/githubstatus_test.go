package attention

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

func ghHook(id, context string, enabled bool) *hooks.Hook {
	h := &hooks.Hook{ID: id}
	if context != "" || enabled {
		h.GitHubStatus = &hooks.GitHubStatusConfig{Enabled: enabled, Context: context}
	}
	return h
}

// The defect this pins: githubstatus.shouldPost returns false before a
// request is built, so an entity asking for a commit status on a
// credential-less runner published nothing and said nothing.
func TestGitHubStatusEntriesNamesEntitiesWithNoCredential(t *testing.T) {
	loaded := map[string]*hooks.Hook{
		"wants-status": ghHook("wants-status", "ci/webhook", true),
		"declared-off": ghHook("declared-off", "ci/off", false),
		"no-status":    ghHook("no-status", "", false),
	}
	mgrs := map[string]*hooks.Manager{
		"a-manager": {Hook: ghHook("a-manager", "ci/manager", true)},
	}

	entries := GitHubStatusEntries(loaded, mgrs, false)

	require.Len(t, entries, 2, "only the two entities that actually asked for a status")
	assert.Equal(t, "a-manager", entries[0].Hook, "entries are sorted by id")
	assert.Equal(t, "wants-status", entries[1].Hook)
	for _, e := range entries {
		assert.Equal(t, SourceGitHubStatus, e.Source)
		assert.Equal(t, KeyGitHubStatus, e.Key)
		assert.Contains(t, e.Message, "every commit status is dropped",
			"the operator must be told the consequence, not just the config gap")
		assert.Contains(t, e.Message, "WEBHOOK_RUNNER_SECRET_SERVER_TOKEN",
			"and both ways to fix it")
	}
	assert.Contains(t, entries[0].Message, "this manager declares github_status")
	assert.Contains(t, entries[1].Message, "this hook declares github_status")
	assert.Contains(t, entries[1].Message, "ci/webhook", "name the context that is going unpublished")
}

// A configured credential is the whole point of the entry existing: with
// , statuses post, so there is nothing to report even for entities that
// declare github_status.
func TestGitHubStatusEntriesSilentWhenConfigured(t *testing.T) {
	loaded := map[string]*hooks.Hook{"wants-status": ghHook("wants-status", "ci/webhook", true)}
	assert.Empty(t, GitHubStatusEntries(loaded, nil, true))
}

func TestGitHubStatusEntriesEmptyWhenNobodyAsks(t *testing.T) {
	loaded := map[string]*hooks.Hook{"plain": ghHook("plain", "", false)}
	assert.Empty(t, GitHubStatusEntries(loaded, nil, false))
	assert.Empty(t, GitHubStatusEntries(nil, nil, false))
}

// A manager whose embedded Hook is missing must not panic the reload path.
func TestGitHubStatusEntriesSkipsEmptyManagers(t *testing.T) {
	mgrs := map[string]*hooks.Manager{"broken": {}, "nil": nil}
	assert.Empty(t, GitHubStatusEntries(nil, mgrs, false))
}

// The source replaces wholesale on every reload, which is the clear rule:
// an entity that stops declaring github_status loses its entry.
func TestGitHubStatusEntriesClearOnReplace(t *testing.T) {
	a := New()
	loaded := map[string]*hooks.Hook{"wants-status": ghHook("wants-status", "ci/webhook", true)}
	a.ReplaceSource(SourceGitHubStatus, GitHubStatusEntries(loaded, nil, false))
	require.Equal(t, 1, a.Count())

	loaded["wants-status"] = ghHook("wants-status", "", false)
	a.ReplaceSource(SourceGitHubStatus, GitHubStatusEntries(loaded, nil, false))
	assert.Equal(t, 0, a.Count())
}
