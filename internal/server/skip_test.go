package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/runstore"
)

// newSkipTestServer wires the full production shape a skip flows through —
// events feed, run store behind the tracker's OnFinish seam, and a mock
// docker that LOGS every invocation so "no container was booted" is a
// positive assertion, not an absence of crashes.
func newSkipTestServer(t *testing.T) (*Server, *hooks.Registry, *runs.Tracker, *runstore.Store, *events.Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-invocations.log")
	docker := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(docker,
		[]byte("#!/bin/sh\necho \"$@\" >> \""+dockerLog+"\"\nexit 0\n"), 0o755))

	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := runstore.Open(runstore.Config{Path: filepath.Join(dir, "runs.db")}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	tr.SetOnFinish(func(rs runs.RunState) { require.NoError(t, st.Record(rs)) })

	rec := events.NewRecorder(50)
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker, Events: rec})
	// LIFO with the store's Close above: every in-flight async run finishes
	// (and records) before the store closes — same ordering serve.go
	// guarantees with its deferred runStore.Close after rn.Wait().
	t.Cleanup(rn.Wait)
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, RunStore: st, Events: rec, Version: testVersion})
	return s, reg, tr, st, rec, dockerLog
}

func strPtr(s string) *string { return &s }

// skipHook is an HMAC-authenticated hook that skips workflow_run deliveries
// — the motivating "GitHub event you can't unsubscribe from" shape.
func skipHook(sync bool) *hooks.Hook {
	return &hooks.Hook{
		ID:          "gh",
		Command:     []string{"echo"},
		Secret:      "abc",
		Synchronous: sync,
		SkipIf: hooks.SkipConditions{
			{"header:x-github-event": &hooks.SkipMatcher{Eq: strPtr("workflow_run")}},
		},
	}
}

func signedRequest(t *testing.T, url, body, secret string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func countEventKind(rec *events.Recorder, kind string) int {
	n := 0
	for _, ev := range rec.List(0) {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func TestTriggerSkipMatched(t *testing.T) {
	s, reg, tr, st, rec, dockerLog := newSkipTestServer(t)
	reg.Set(skipHook(false))

	req := signedRequest(t, "/hook/gh", `{"action":"completed"}`, "abc")
	req.Header.Set("X-GitHub-Event", "workflow_run")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	// Answered immediately, 2xx, naming the skip.
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "skipped", resp["status"])
	assert.Equal(t, `skip_if[0]: header x-github-event == "workflow_run"`, resp["reason"])
	id := resp["run_id"]
	require.NotEmpty(t, id)

	// A real, terminal run in the tracker with the reason as its output.
	run := tr.Get(id)
	require.NotNil(t, run)
	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSkipped, snap.Status)
	assert.Equal(t, []string{`skipped: skip_if[0]: header x-github-event == "workflow_run"`}, snap.Output)
	assert.True(t, snap.StartedAt.IsZero())

	// Persisted through the OnFinish seam (Finish runs it synchronously, so
	// it's durable before the HTTP response is even written).
	got, ok := st.Get(id)
	require.True(t, ok)
	assert.Equal(t, runs.StatusSkipped, got.Status)

	// Loud on the activity feed; and NO container was ever booted.
	assert.Equal(t, 1, countEventKind(rec, "run.skipped"))
	assert.NoFileExists(t, dockerLog)
}

// Authentication strictly precedes skip evaluation: an unauthenticated
// delivery that WOULD match gets the plain 401 — no skip record, no
// run.skipped event, nothing for a probing caller to observe.
func TestTriggerSkipRequiresAuthFirst(t *testing.T) {
	s, reg, tr, st, rec, dockerLog := newSkipTestServer(t)
	reg.Set(skipHook(false))

	req := httptest.NewRequest(http.MethodPost, "/hook/gh", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "workflow_run") // matches, but unauthenticated
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, tr.ListAll(0), "no run may exist for an unauthenticated delivery")
	assert.Empty(t, st.ListAll(0))
	assert.Equal(t, 0, countEventKind(rec, "run.skipped"))
	assert.Equal(t, 1, countEventKind(rec, "hook.denied"))
	assert.NoFileExists(t, dockerLog)
}

func TestTriggerSkipNoMatchRunsNormally(t *testing.T) {
	s, reg, _, _, _, dockerLog := newSkipTestServer(t)
	h := skipHook(false)
	reg.Set(h)

	req := signedRequest(t, "/hook/gh", `{"action":"opened"}`, "abc")
	req.Header.Set("X-GitHub-Event", "pull_request") // does not match skip_if
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	// The normal async path: accepted, run dispatched. (The run itself
	// errors later — the registry hook has no on-disk directory to build an
	// image from — but that's the run pipeline's business; the point here is
	// the delivery was NOT skipped.)
	require.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp["status"])
	assert.NotEmpty(t, resp["run_id"])
	_ = dockerLog
}

// A skipped delivery on a synchronous hook answers immediately with the
// 200/skipped shape — never the sync snapshot path (whose non-success rule
// would have turned "no work was done" into a 500).
func TestTriggerSkipSynchronousHookAnswersImmediately(t *testing.T) {
	s, reg, _, _, _, _ := newSkipTestServer(t)
	reg.Set(skipHook(true))

	req := signedRequest(t, "/hook/gh", `{}`, "abc")
	req.Header.Set("X-GitHub-Event", "workflow_run")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "skipped", resp["status"])
}

// Payload-path conditions work end-to-end over the raw request body.
func TestTriggerSkipPayloadCondition(t *testing.T) {
	s, reg, _, _, _, _ := newSkipTestServer(t)
	reg.Set(&hooks.Hook{
		ID:      "gh",
		Command: []string{"echo"},
		Secret:  "abc",
		SkipIf: hooks.SkipConditions{
			{
				"action":      &hooks.SkipMatcher{In: []string{"labeled", "unlabeled"}},
				"sender.type": &hooks.SkipMatcher{Eq: strPtr("Bot")},
			},
		},
	})

	req := signedRequest(t, "/hook/gh", `{"action":"labeled","sender":{"type":"Bot"}}`, "abc")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"skipped"`)

	// AND semantics: same action from a human runs.
	req = signedRequest(t, "/hook/gh", `{"action":"labeled","sender":{"type":"User"}}`, "abc")
	w = httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
}

// Skips are their own stats bucket: visible per hook, never diluting the
// completion/success/duration figures.
func TestHookDetailStatsSkippedBucket(t *testing.T) {
	s, reg, tr, _, _, _ := newSkipTestServer(t)
	h := skipHook(false)
	reg.Set(h)

	ok := tr.New("gh")
	ok.SetRunning()
	ok.Finish(runs.StatusSuccess, 0, "")
	s.runner.Skip(h, `skip_if[0]: header x-github-event == "workflow_run"`, "")

	req := httptest.NewRequest(http.MethodGet, "/hooks/gh", nil)
	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var detail HookDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &detail))
	assert.Equal(t, 1, detail.Info.SkipConditions)
	assert.Equal(t, 2, detail.Stats.Tracked)
	assert.Equal(t, 1, detail.Stats.Completed, "skips are not completions")
	assert.Equal(t, 1, detail.Stats.Skipped)
	assert.Equal(t, 1.0, detail.Stats.SuccessRate, "a skip must not dilute the success rate")
	assert.Equal(t, 1, detail.Stats.ByStatus[runs.StatusSkipped])
	assert.Equal(t, 1, detail.Stats.ByStatus[runs.StatusSuccess])
}
