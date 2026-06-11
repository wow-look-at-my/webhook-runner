package githubstatus

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

type capturedRequest struct {
	method, path, auth string
	body               map[string]string
}

type fakeGitHub struct {
	mu       sync.Mutex
	received []capturedRequest
	respCode int
}

func (f *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.received = append(f.received, capturedRequest{
			method: r.Method,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			body:   body,
		})
		code := f.respCode
		f.mu.Unlock()
		if code == 0 {
			code = http.StatusCreated
		}
		w.WriteHeader(code)
	})
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newRun(t *testing.T, hookID string, status runs.Status, exit int) *runs.Run {
	t.Helper()
	tr := runs.NewTracker()
	r := tr.New(hookID)
	r.AppendOutput("hello")
	r.AppendOutput("world")
	if status != runs.StatusPending {
		r.Finish(status, exit, "")
	}
	return r
}

func TestPostStartAndFinish(t *testing.T) {
	fake := &fakeGitHub{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New("ghp_token", newSilentLogger())
	c.SetAPIURL(srv.URL)

	hook := &hooks.Hook{
		ID: "deploy",
		GitHubStatus: &hooks.GitHubStatusConfig{
			Enabled:   true,
			Context:   "ci/deploy",
			TargetURL: "https://logs.example.com/{{.HookID}}/{{.RunID}}",
		},
	}
	payload := []byte(`{"after":"abc","repository":{"full_name":"o/r"}}`)

	run := newRun(t, "deploy", runs.StatusPending, 0)
	c.PostStart(context.Background(), hook, run, payload)

	run.Finish(runs.StatusSuccess, 0, "")
	c.PostFinish(context.Background(), hook, run, payload)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.received, 2)
	assert.Equal(t, "/repos/o/r/statuses/abc", fake.received[0].path)
	assert.Equal(t, "Bearer ghp_token", fake.received[0].auth)
	assert.Equal(t, "pending", fake.received[0].body["state"])
	assert.Equal(t, "ci/deploy", fake.received[0].body["context"])
	assert.Contains(t, fake.received[0].body["target_url"], run.ID())

	assert.Equal(t, "success", fake.received[1].body["state"])
	assert.Contains(t, fake.received[1].body["description"], "succeeded")
}

func TestPostFinishStateMapping(t *testing.T) {
	cases := []struct {
		status runs.Status
		want   string
	}{
		{runs.StatusSuccess, "success"},
		{runs.StatusFailure, "failure"},
		{runs.StatusTimeout, "error"},
		{runs.StatusError, "error"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			fake := &fakeGitHub{}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			c := New("token", newSilentLogger())
			c.SetAPIURL(srv.URL)

			hook := &hooks.Hook{
				ID:           "h",
				GitHubStatus: &hooks.GitHubStatusConfig{Enabled: true, Context: "x"},
			}
			run := newRun(t, "h", tc.status, 0)
			payload := []byte(`{"after":"abc","repository":{"full_name":"o/r"}}`)
			c.PostFinish(context.Background(), hook, run, payload)

			fake.mu.Lock()
			defer fake.mu.Unlock()
			require.Len(t, fake.received, 1)
			assert.Equal(t, tc.want, fake.received[0].body["state"])
		})
	}
}

func TestNoTokenSilentlySkips(t *testing.T) {
	fake := &fakeGitHub{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New("", newSilentLogger())
	c.SetAPIURL(srv.URL)
	assert.False(t, c.Enabled())

	hook := &hooks.Hook{ID: "h", GitHubStatus: &hooks.GitHubStatusConfig{Enabled: true, Context: "x"}}
	run := newRun(t, "h", runs.StatusSuccess, 0)
	payload := []byte(`{"after":"abc","repository":{"full_name":"o/r"}}`)
	c.PostStart(context.Background(), hook, run, payload)
	c.PostFinish(context.Background(), hook, run, payload)
	assert.Empty(t, fake.received)
}

func TestDisabledHookSkips(t *testing.T) {
	fake := &fakeGitHub{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New("token", newSilentLogger())
	c.SetAPIURL(srv.URL)

	cases := []*hooks.Hook{
		{ID: "h"}, // no github_status at all
		{ID: "h", GitHubStatus: &hooks.GitHubStatusConfig{Enabled: false, Context: "x"}},
	}
	run := newRun(t, "h", runs.StatusSuccess, 0)
	payload := []byte(`{"after":"abc","repository":{"full_name":"o/r"}}`)
	for _, hook := range cases {
		c.PostStart(context.Background(), hook, run, payload)
		c.PostFinish(context.Background(), hook, run, payload)
	}
	assert.Empty(t, fake.received)
}

func TestMissingRepoOrSHASkips(t *testing.T) {
	fake := &fakeGitHub{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New("token", newSilentLogger())
	c.SetAPIURL(srv.URL)

	hook := &hooks.Hook{ID: "h", GitHubStatus: &hooks.GitHubStatusConfig{Enabled: true, Context: "x"}}
	run := newRun(t, "h", runs.StatusSuccess, 0)
	c.PostStart(context.Background(), hook, run, []byte(`{}`))
	c.PostFinish(context.Background(), hook, run, []byte(`{}`))
	assert.Empty(t, fake.received)
}

func TestDescriptionTruncation(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := truncate(long, 140)
	assert.Equal(t, 140, len(got))
	assert.True(t, strings.HasSuffix(got, "..."))

	assert.Equal(t, "abc", truncate("abc", 140))
	assert.Equal(t, "ab", truncate("abcdef", 2))
}

func TestServerError(t *testing.T) {
	fake := &fakeGitHub{respCode: http.StatusInternalServerError}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c := New("token", newSilentLogger())
	c.SetAPIURL(srv.URL)
	hook := &hooks.Hook{ID: "h", GitHubStatus: &hooks.GitHubStatusConfig{Enabled: true, Context: "x"}}
	run := newRun(t, "h", runs.StatusSuccess, 0)
	payload := []byte(`{"after":"abc","repository":{"full_name":"o/r"}}`)
	// should not panic, just log
	c.PostFinish(context.Background(), hook, run, payload)
}
