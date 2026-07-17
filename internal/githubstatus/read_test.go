package githubstatus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func combinedStatusServer(t *testing.T, statuses []map[string]string, code int) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path + "?" + r.URL.RawQuery
		got.auth = r.Header.Get("Authorization")
		if code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":    "pending", // the cross-context rollup; ContextState ignores it
			"statuses": statuses,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestContextState(t *testing.T) {
	srv, got := combinedStatusServer(t, []map[string]string{
		{"context": "some-other-check", "state": "failure"},
		{"context": "all-builds", "state": "success"},
	}, http.StatusOK)
	c := New("tok", newSilentLogger())
	c.SetAPIURL(srv.URL)

	state, err := c.ContextState(context.Background(), "wow-look-at-my/webhooks", "abc123", "all-builds")
	require.NoError(t, err)
	assert.Equal(t, "success", state, "the requested context's state, not the rollup")
	assert.Equal(t, http.MethodGet, got.method)
	assert.Equal(t, "/repos/wow-look-at-my/webhooks/commits/abc123/status?per_page=100", got.path)
	assert.Equal(t, "Bearer tok", got.auth)

	state, err = c.ContextState(context.Background(), "wow-look-at-my/webhooks", "abc123", "some-other-check")
	require.NoError(t, err)
	assert.Equal(t, "failure", state)
}

func TestContextStateMissingContext(t *testing.T) {
	srv, _ := combinedStatusServer(t, []map[string]string{
		{"context": "some-other-check", "state": "success"},
	}, http.StatusOK)
	c := New("tok", newSilentLogger())
	c.SetAPIURL(srv.URL)

	state, err := c.ContextState(context.Background(), "o/r", "abc123", "all-builds")
	require.ErrorIs(t, err, ErrNoContextStatus)
	assert.Empty(t, state)
}

func TestContextStateNoToken(t *testing.T) {
	// No token = statuses on a private repo are unreadable: an immediate,
	// loud error — never a guessed answer, and no HTTP call to fail slow.
	c := New("", newSilentLogger())
	_, err := c.ContextState(context.Background(), "o/r", "abc123", "all-builds")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no GitHub token configured")
}

func TestContextStateHTTPError(t *testing.T) {
	srv, _ := combinedStatusServer(t, nil, http.StatusInternalServerError)
	c := New("tok", newSilentLogger())
	c.SetAPIURL(srv.URL)

	_, err := c.ContextState(context.Background(), "o/r", "abc123", "all-builds")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")
	assert.NotErrorIs(t, err, ErrNoContextStatus, "a transport failure is not a missing context")
}

func TestRepoFromGitURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"git@github.com:wow-look-at-my/webhooks.git", "wow-look-at-my/webhooks", true},
		{"git@github.com:wow-look-at-my/webhooks", "wow-look-at-my/webhooks", true},
		{"https://github.com/wow-look-at-my/webhooks.git", "wow-look-at-my/webhooks", true},
		{"https://github.com/wow-look-at-my/webhooks", "wow-look-at-my/webhooks", true},
		{"ssh://git@github.com/wow-look-at-my/webhooks.git", "wow-look-at-my/webhooks", true},
		{"  git@github.com:o/r.git  ", "o/r", true},
		{"file:///var/repos/hooks.git", "", false}, // no slug in a local path
		{"/var/repos/hooks", "", false},
		{"https://github.com/onlyowner", "", false},
		{"https://github.com/o/r/extra", "", false},
		{"git@github.com:", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := RepoFromGitURL(tc.in)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}
