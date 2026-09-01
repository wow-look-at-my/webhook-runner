package cli

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// A commit-status server that records the credential it was handed, so the
// tests assert what actually reaches GitHub rather than what was configured.
func statusServer(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"statuses": []map[string]string{{"context": "all-builds", "state": "success"}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The failure that motivated the whole thing: a deployment with neither
// credential configured answers every reconciliation poll with an error, and
// that error has to say how to fix it -- BOTH ways, since the environment
// variable is no longer the only .
func TestNoCredentialFailsClosedAndNamesBothFixes(t *testing.T) {
	gh, err := newGitHubStatusClient(&serveOptions{}, quietLogger())
	require.Nil(t, err)

	assert.False(t, gh.Enabled())

	_, err = gh.ContextState(context.Background(), "o/r", "deadbeef", "all-builds")
	require.NotNil(t, err)

	for _, want := range []string{"WEBHOOK_RUNNER_GITHUB_TOKEN", "WEBHOOK_RUNNER_SECRET_SERVER_TOKEN", "PRIVATE_ORG_REPO_READ"} {
		assert.Contains(t, err.Error(), want)

	}
}

func TestEnvTokenIsUsedDirectly(t *testing.T) {
	var seen string
	srv := statusServer(t, &seen)

	gh, err := newGitHubStatusClient(&serveOptions{ghToken: "ghp_env"}, quietLogger())
	require.Nil(t, err)

	gh.SetAPIURL(srv.URL)
	state, err := gh.ContextState(context.Background(), "o/r", "deadbeef", "all-builds")
	require.Nil(t, err)

	assert.Equal(t, "success", state)

	assert.Equal(t, "Bearer ghp_env", seen)

}

// The fix for the reported failure, end to end: with only a machine token
// configured, the status read is authenticated with PRIVATE_ORG_REPO_READ
// fetched from secret-server.
func TestSecretServerSuppliesTheCredential(t *testing.T) {
	secrets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sst_machine" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"PRIVATE_ORG_REPO_READ":"ghp_from_secret_server"}`))
	}))
	defer secrets.Close()

	var seen string
	srv := statusServer(t, &seen)

	gh, err := newGitHubStatusClient(&serveOptions{
		secretServerTok: "sst_machine",
		secretServerURL: secrets.URL,
		ghTokenSecret:   defaultGitHubTokenSecret,
	}, quietLogger())
	require.Nil(t, err)

	assert.True(t, gh.Enabled())

	gh.SetAPIURL(srv.URL)

	state, err := gh.ContextState(context.Background(), "o/r", "deadbeef", "all-builds")
	require.Nil(t, err)

	assert.Equal(t, "success", state)

	assert.Equal(t, "Bearer ghp_from_secret_server", seen)

}

// Explicit beats derived: setting both is a preference, not an error.
func TestEnvTokenWinsOverSecretServer(t *testing.T) {
	var seen string
	srv := statusServer(t, &seen)

	gh, err := newGitHubStatusClient(&serveOptions{
		ghToken:         "ghp_env",
		secretServerTok: "sst_machine",
		secretServerURL: "http://127.0.0.1:1", // never contacted
		ghTokenSecret:   defaultGitHubTokenSecret,
	}, quietLogger())
	require.Nil(t, err)

	gh.SetAPIURL(srv.URL)
	_, err = gh.ContextState(context.Background(), "o/r", "deadbeef", "all-builds")
	require.Nil(t, err)

	assert.Equal(t, "Bearer ghp_env", seen)

}

// A malformed machine token is a boot failure. Shipping a deployment that
// looks healthy and cannot read a commit status is the exact outcome this
// whole path exists to prevent.
func TestMalformedMachineTokenFailsStartup(t *testing.T) {
	_, err := newGitHubStatusClient(&serveOptions{
		secretServerTok: "not_an_sst_token",
		ghTokenSecret:   defaultGitHubTokenSecret,
	}, quietLogger())
	require.NotNil(t, err)

	assert.Contains(t, err.Error(), "WEBHOOK_RUNNER_SECRET_SERVER_TOKEN")

}

// An unreachable secret-server is NOT a boot failure: the credential resolves
// per call, so an outage during a restart heals on the next poll instead of
// leaving the process blind until someone restarts it again.
func TestUnreachableSecretServerStillBootsAndHeals(t *testing.T) {
	var down = true
	secrets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"PRIVATE_ORG_REPO_READ":"ghp_late"}`))
	}))
	defer secrets.Close()

	var seen string
	srv := statusServer(t, &seen)

	gh, err := newGitHubStatusClient(&serveOptions{
		secretServerTok: "sst_machine",
		secretServerURL: secrets.URL,
		ghTokenSecret:   defaultGitHubTokenSecret,
	}, quietLogger())
	require.Nil(t, err)

	gh.SetAPIURL(srv.URL)

	_, err = gh.ContextState(context.Background(), "o/r", "deadbeef", "all-builds")
	require.NotNil(t, err)

	down = false
	state, err := gh.ContextState(context.Background(), "o/r", "deadbeef", "all-builds")
	require.Nil(t, err)

	assert.False(t, state != "success" || seen != "Bearer ghp_late")

}
