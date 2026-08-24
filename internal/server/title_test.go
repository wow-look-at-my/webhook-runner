package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// --- POST /title (state API): the mid-run override ------------------------

func TestStateTitleValidation(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	// Auth mirrors every other state route.
	require.Equal(t, 401, stateReq(t, s, "POST", "/title", "", strings.NewReader(`{"title":"x"}`)).Code)
	require.Equal(t, 401, stateReq(t, s, "POST", "/title", "h.bogus", strings.NewReader(`{"title":"x"}`)).Code)

	// Bad bodies: not JSON, absent/empty/blank titles, overlong titles. A
	// hook naming itself can be told no — unlike the template renderer,
	// which clamps silently.
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"title":""}`,
		`{"title":"   "}`,
		`{"title":"` + strings.Repeat("t", hooks.MaxRunTitleLen+1) + `"}`,
	} {
		rr := stateReq(t, s, "POST", "/title", tok, strings.NewReader(body))
		require.Equalf(t, 400, rr.Code, "body=%q -> %s", body, rr.Body.String())
	}

	// A token whose run is unknown, finished, or belongs to another hook has nothing to title: 409 — same rule as /wait, and a terminal run's.
	require.Equal(t, 409,
		stateReq(t, s, "POST", "/title", store.Token("h", "nosuchrun"), strings.NewReader(`{"title":"x"}`)).Code)
	require.Equal(t, 409,
		stateReq(t, s, "POST", "/title", store.Token("other", run.ID()), strings.NewReader(`{"title":"x"}`)).Code)
	done := tr.New("h")
	done.Finish(runs.StatusSuccess, 0, "")
	require.Equal(t, 409,
		stateReq(t, s, "POST", "/title", store.Token("h", done.ID()), strings.NewReader(`{"title":"x"}`)).Code)

	// Nothing above may have titled the run.
	assert.Empty(t, run.Title())
}

func TestStateTitleWithoutTracker(t *testing.T) {
	s, store := newStateServer(t, kv.Config{})
	rr := stateReq(t, s, "POST", "/title", store.Token("h", "r1"), strings.NewReader(`{"title":"x"}`))
	require.Equal(t, 503, rr.Code)
}

// The happy path: 204, and the title is immediately live — on the Run, on
// GET /runs/{id}, and replacing any earlier (template) title.
func TestStateTitleUpdatesLiveRun(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetTitle("template title") // what a run_title template would have set
	run.SetRunning()
	tok := store.Token("h", run.ID())

	rr := stateReq(t, s, "POST", "/title", tok, strings.NewReader(`{"title":"  wow-look-at-my/go-toolchain#47  "}`))
	require.Equal(t, 204, rr.Code, rr.Body.String())
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", run.Title(), "trimmed and replacing the template title")

	// Visible through the admin read path at once.
	req := httptest.NewRequest(http.MethodGet, "/runs/"+run.ID(), nil)
	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	var got runs.RunState
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", got.Title)
}

// --- Trigger wiring: template titles through POST /hook/{id} ---------------

// withHookDir gives a registry hook the on-disk directory (and Dockerfile)
// the run pipeline needs, so mock-docker runs succeed instead of erroring
// at the image step.
func withHookDir(t *testing.T, h *hooks.Hook) *hooks.Hook {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644))
	h.SourcePath = filepath.Join(dir, "hook.json")
	return h
}

// prPayload is the operator's motivating shape.
const prPayload = `{"repository":{"full_name":"wow-look-at-my/go-toolchain"},"pull_request":{"number":47}}`

// An async delivery resolves the hook's run_title template at accept time:
// the 202's run is already titled in the tracker.
func TestTriggerResolvesRunTitle(t *testing.T) {
	s, reg, tr, _, _, _ := newSkipTestServer(t)
	reg.Set(withHookDir(t, &hooks.Hook{
		ID:       "gh",
		Command:  []string{"echo"},
		Secret:   "abc",
		RunTitle: "{{repository.full_name}}#{{pull_request.number}}",
	}))

	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, signedRequest(t, "/hook/gh", prPayload, "abc"))
	require.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	run := tr.Get(resp["run_id"])
	require.NotNil(t, run)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", run.Title(),
		"the title is set at accept, before anything ran")
}

// The title resolves BEFORE skip evaluation, so a skipped run carries it —
// a skip should still say which PR it was about — all the way into the
// persisted history.
func TestTriggerSkippedRunIsTitled(t *testing.T) {
	s, reg, tr, st, _, dockerLog := newSkipTestServer(t)
	reg.Set(&hooks.Hook{
		ID:       "gh",
		Command:  []string{"echo"},
		Secret:   "abc",
		RunTitle: "{{repository.full_name}}#{{pull_request.number}}",
		SkipIf: hooks.SkipConditions{
			{"header:x-github-event": &hooks.SkipMatcher{Eq: strPtr("pull_request")}},
		},
	})

	req := signedRequest(t, "/hook/gh", prPayload, "abc")
	req.Header.Set("X-GitHub-Event", "pull_request")
	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "skipped", resp["status"])

	run := tr.Get(resp["run_id"])
	require.NotNil(t, run)
	snap := run.Snapshot(-1)
	assert.Equal(t, runs.StatusSkipped, snap.Status)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", snap.Title)

	// And the terminal snapshot persisted with it (title set before Finish).
	got, ok := st.Get(resp["run_id"])
	require.True(t, ok)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", got.Title)
	assert.NoFileExists(t, dockerLog, "still no container for a skip")
}

// A synchronous delivery's held response is the run snapshot — title
// included.
func TestTriggerSyncResponseCarriesTitle(t *testing.T) {
	s, reg, _, _, _, _ := newSkipTestServer(t)
	reg.Set(withHookDir(t, &hooks.Hook{
		ID:          "gh",
		Command:     []string{"echo"},
		Secret:      "abc",
		Synchronous: true,
		RunTitle:    "{{repository.full_name}}#{{pull_request.number}}",
	}))

	w := httptest.NewRecorder()
	hook(s).ServeHTTP(w, signedRequest(t, "/hook/gh", prPayload, "abc"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var snap runs.RunState
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", snap.Title)
}

// /runs serves the title for live runs AND for runs read back from the
// persisted history (the store round-trips it inside the metadata blob).
func TestRunsListIncludesTitle(t *testing.T) {
	s, _, tr, st, _, _ := newSkipTestServer(t)

	live := tr.New("gh")
	live.SetTitle("live: o/r#1")

	// A history-only run: recorded straight into the store, never in the
	// tracker — the restart case.
	require.NoError(t, st.Record(runs.RunState{
		ID:       "persistedpersistedpersist2",
		HookID:   "gh",
		Title:    "old: o/r#2",
		Status:   runs.StatusSuccess,
		Started:  time.Now().UTC().Add(-time.Minute),
		Finished: time.Now().UTC().Add(-time.Minute),
	}))

	req := httptest.NewRequest(http.MethodGet, "/runs?hook=gh", nil)
	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	var list []runs.RunState
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list, 2)

	titles := map[string]string{}
	for _, r := range list {
		titles[r.ID] = r.Title
	}
	assert.Equal(t, "live: o/r#1", titles[live.ID()])
	assert.Equal(t, "old: o/r#2", titles["persistedpersistedpersist2"])
}
