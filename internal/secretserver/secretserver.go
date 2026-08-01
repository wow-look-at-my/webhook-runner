// Package secretserver reads secrets from secret-server
// (https://github.com/wow-look-at-my/secret-server) with a machine token.
//
// The server vends every secret the credential is entitled to through ONE
// public route, GET /github/v1/secrets, authenticated by a bearer credential:
// a GitHub Actions OIDC JWT, or an admin-issued machine token whose "sst_"
// prefix selects the validation path. The response is a flat JSON object of
// name -> plaintext value; a valid credential entitled to nothing gets {} with
// HTTP 200, and a missing or invalid one gets 401. That is the whole contract
// this package implements.
//
// Why webhook-runner needs it: the reload gate reads the hooks repo's gating
// commit status from the GitHub API, which needs a credential with read access
// to a PRIVATE repo's statuses. That credential is PRIVATE_ORG_REPO_READ, and
// it lives in secret-server -- so before this package the only way to have it
// was to paste it into the deployment's environment by hand, and a deployment
// that had not been given one held every reload with "no GitHub token
// configured".
//
// Values are secrets: they are never logged, never put in an error message,
// and never written to the attention surface (whose entries are required to be
// value-free). Errors here name the SECRET, the URL and the HTTP status --
// never a response body from a successful fetch, because that body is the
// secrets themselves.
package secretserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is this org's secret-server.
const DefaultBaseURL = "https://secrets.pazer.io"

// MachineTokenPrefix marks an admin-issued machine token. The server picks its
// validation path from the prefix, so a credential without it is treated as an
// OIDC JWT and rejected -- worth catching here, where the message can say so.
const MachineTokenPrefix = "sst_"

// DefaultTTL is how long a fetched secret set is reused. Secret-server is a
// hard dependency of a reload only once an hour (the reconciliation poll), so
// this is about surviving a restart-time outage rather than saving requests:
// short enough that a rotated credential heals without a redeploy, long enough
// that nothing hammers the server.
const DefaultTTL = 15 * time.Minute

// ErrNotEntitled reports that the fetch succeeded but the credential is not
// entitled to the requested secret. It is a CONFIGURATION answer, not a
// transport failure: retrying changes nothing until an admin attaches the
// secret to the token.
var ErrNotEntitled = errors.New("secret-server: credential is not entitled to that secret")

// Client fetches the secret set one credential can read.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for the given secret-server base URL and machine
// token. An empty baseURL means DefaultBaseURL.
func New(baseURL, token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("secret-server: no machine token given")
	}
	if !strings.HasPrefix(token, MachineTokenPrefix) {
		return nil, fmt.Errorf("secret-server: machine token must start with %q (the prefix selects the server's validation path; an OIDC JWT is not usable here)", MachineTokenPrefix)
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// SetHTTPClient overrides the HTTP client. Tests use it to point at an
// httptest server with a short timeout.
func (c *Client) SetHTTPClient(h *http.Client) {
	if h != nil {
		c.http = h
	}
}

// Fetch returns every secret the credential is entitled to, as name -> value.
// An entitled-to-nothing credential yields an empty map and a nil error, which
// is what the server means by 200 {}.
func (c *Client) Fetch(ctx context.Context) (map[string]string, error) {
	url := c.baseURL + "/github/v1/secrets"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("secret-server: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secret-server: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Only an ERROR body is quoted. A 200 body is the secrets.
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		msg := strings.TrimSpace(string(excerpt))
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("secret-server: GET %s: HTTP 401 (machine token missing, revoked or malformed): %s", url, msg)
		}
		return nil, fmt.Errorf("secret-server: GET %s: HTTP %d: %s", url, resp.StatusCode, msg)
	}

	var secrets map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&secrets); err != nil {
		// Deliberately not quoting the body: on 200 it is secret material.
		return nil, fmt.Errorf("secret-server: GET %s: response is not a JSON object of name -> value: %w", url, err)
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	return secrets, nil
}

// Provider serves individual secrets from a cached fetch.
//
// The cache is what makes a startup-time outage survivable: a failed fetch is
// not sticky, so the next caller retries, and the reconciliation poll heals on
// its own instead of leaving the deployment blind until someone restarts it.
type Provider struct {
	client *Client
	ttl    time.Duration
	now    func() time.Time

	mu        sync.Mutex
	cached    map[string]string
	fetchedAt time.Time
}

// NewProvider wraps a client with a TTL cache. A ttl <= 0 means DefaultTTL.
func NewProvider(c *Client, ttl time.Duration) *Provider {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Provider{client: c, ttl: ttl, now: time.Now}
}

// Secret returns one secret by name, fetching (or re-fetching) as needed.
// A name the credential cannot read returns ErrNotEntitled -- distinct from a
// transport failure, because the two need different fixes.
func (p *Provider) Secret(ctx context.Context, name string) (string, error) {
	if p == nil || p.client == nil {
		return "", errors.New("secret-server: not configured")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached == nil || p.now().Sub(p.fetchedAt) >= p.ttl {
		secrets, err := p.client.Fetch(ctx)
		if err != nil {
			return "", err
		}
		p.cached = secrets
		p.fetchedAt = p.now()
	}
	v, ok := p.cached[name]
	if !ok || v == "" {
		return "", fmt.Errorf("%w: %s", ErrNotEntitled, name)
	}
	return v, nil
}
