package secretserver

import (
	"context"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, baseURL, token string) *Client {
	t.Helper()
	c, err := New(baseURL, token)
	require.Nil(t, err)

	return c
}

// The one route, the one credential shape, the one response shape.
func TestFetchSendsTheMachineTokenAndParsesTheSecretSet(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"PRIVATE_ORG_REPO_READ":"ghp_example","OTHER":"x"}`))
	}))
	defer srv.Close()

	secrets, err := newTestClient(t, srv.URL, "sst_abc").Fetch(context.Background())
	require.Nil(t, err)

	assert.Equal(t, "/github/v1/secrets", gotPath)

	assert.Equal(t, "Bearer sst_abc", gotAuth)

	assert.Equal(t, "ghp_example", secrets["PRIVATE_ORG_REPO_READ"])

}

// A credential entitled to nothing is a 200 with {} -- a configuration answer,
// not a transport failure, and the distinction is what tells an operator to go
// attach the secret rather than go debug the network.
func TestEntitledToNothingIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := NewProvider(newTestClient(t, srv.URL, "sst_abc"), time.Minute)
	_, err := p.Secret(context.Background(), "PRIVATE_ORG_REPO_READ")
	require.True(t, errors.Is(err, ErrNotEntitled))

	assert.Contains(t, err.Error(), "PRIVATE_ORG_REPO_READ")

}

func TestUnauthorizedNamesTheCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unknown token"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, "sst_abc").Fetch(context.Background())
	require.NotNil(t, err)

	assert.False(t, !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "revoked"))

}

// The prefix selects the server's validation path, so a credential without it
// would be validated as an OIDC JWT and rejected. Catching it here means the
// message says WHY instead of surfacing as a 401 an hour later.
func TestMachineTokenPrefixIsRequired(t *testing.T) {
	_, err := New("", "ghp_not_a_machine_token")
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "sst_", "the error must name the prefix")

	_, err = New("", "  ")
	require.NotNil(t, err)
}

func TestDefaultBaseURLIsUsedWhenUnset(t *testing.T) {
	c := newTestClient(t, "", "sst_abc")
	assert.Equal(t, DefaultBaseURL, c.baseURL)

	trailing := newTestClient(t, "https://example.test/", "sst_abc")
	assert.Equal(t, "https://example.test", trailing.baseURL)

}

// The cache exists to bound request volume, not to pin a value forever.
func TestProviderCachesWithinTTLAndRefetchesAfter(t *testing.T) {
	var calls int
	value := "first"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"PRIVATE_ORG_REPO_READ":"` + value + `"}`))
	}))
	defer srv.Close()

	now := time.Unix(1_700_000_000, 0)
	p := NewProvider(newTestClient(t, srv.URL, "sst_abc"), time.Minute)
	p.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		got, err := p.Secret(context.Background(), "PRIVATE_ORG_REPO_READ")
		require.False(t, err != nil || got != "first")

	}
	assert.Equal(t, 1, calls)

	// A rotated credential must heal without a redeploy.
	value = "rotated"
	now = now.Add(2 * time.Minute)
	got, err := p.Secret(context.Background(), "PRIVATE_ORG_REPO_READ")
	require.Nil(t, err)

	assert.Equal(t, "rotated", got)

	assert.Equal(t, 2, calls)

}

// A failed fetch must not be sticky: the deployment that could not reach
// secret-server at startup is exactly the one that must recover by itself.
func TestFetchFailureIsNotCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"PRIVATE_ORG_REPO_READ":"ghp_example"}`))
	}))
	defer srv.Close()

	p := NewProvider(newTestClient(t, srv.URL, "sst_abc"), time.Hour)
	_, err := p.Secret(context.Background(), "PRIVATE_ORG_REPO_READ")
	require.NotNil(t, err)

	got, err := p.Secret(context.Background(), "PRIVATE_ORG_REPO_READ")
	require.Nil(t, err)

	assert.Equal(t, "ghp_example", got)

}

// Secret VALUES must never reach an error string: errors travel to logs and to
// the attention surface, which is required to be value-free.
func TestErrorsNeverCarrySecretValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A 200 whose body is not the expected shape: the parse error must
		// not quote it, because on 200 the body is secret material.
		_, _ = w.Write([]byte(`["ghp_SUPERSECRET_VALUE"]`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, "sst_abc").Fetch(context.Background())
	require.NotNil(t, err)

	require.NotContains(t, err.Error(), "SUPERSECRET")

}

func TestNilProviderIsSafe(t *testing.T) {
	var p *Provider
	_, err := p.Secret(context.Background(), "X")
	require.NotNil(t, err)

}
