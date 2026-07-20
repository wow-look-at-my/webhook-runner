package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/runstore"
)

// newTestServerWithStore mirrors newTestServer plus the persistent run
// store, wired the way serve.go wires it: terminal runs flow into the store
// through the tracker's OnFinish seam.
func newTestServerWithStore(t *testing.T) (*Server, *hooks.Registry, *runs.Tracker, *runstore.Store) {
	t.Helper()
	dir := t.TempDir()
	docker := writeMockDocker(t, dir)

	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := runstore.Open(runstore.Config{Path: filepath.Join(dir, "runs.db")}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	tr.SetOnFinish(func(rs runs.RunState) { require.NoError(t, st.Record(rs)) })

	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, RunStore: st, Version: testVersion})
	return s, reg, tr, st
}

// persistOld drops a completed run directly into the store with no tracker
// counterpart — the shape of history from before a restart (or evicted from
// the tracker's bounded window).
func persistOld(t *testing.T, st *runstore.Store, id, hook string, status runs.Status, age time.Duration, output ...string) runs.RunState {
	t.Helper()
	started := time.Now().UTC().Add(-age)
	rs := runs.RunState{
		ID:       id,
		HookID:   hook,
		Status:   status,
		Started:  started,
		Finished: started.Add(2 * time.Second),
		Output:   output,
	}
	for range output {
		rs.OutputTimes = append(rs.OutputTimes, started)
	}
	require.NoError(t, st.Record(rs))
	return rs
}

func TestListRunsMergesPersistedHistory(t *testing.T) {
	s, _, tr, st := newTestServerWithStore(t)

	// Only in the store: finished before a "restart".
	old := persistOld(t, st, "oldoldoldoldoldoldoldoldol", "h", runs.StatusSuccess, time.Hour, "bye")

	// Finished live run: in the tracker AND (via OnFinish) in the store —
	// the merged view must show it exactly once.
	fin := tr.New("h")
	fin.AppendOutput("data")
	fin.Finish(runs.StatusFailure, 1, "")

	// Active run: tracker only.
	act := tr.New("h")
	act.SetRunning()

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	require.Len(t, got, 3, "duplicated run must be deduped")
	assert.Equal(t, act.ID(), got[0].ID)
	assert.Equal(t, fin.ID(), got[1].ID)
	assert.Equal(t, old.ID, got[2].ID)
	assert.Equal(t, runs.StatusFailure, got[1].Status)
	for _, r := range got {
		assert.Empty(t, r.Output, "list view must not ship output")
	}

	// max caps the merged result, newest-first.
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs?max=2", nil))
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, act.ID(), got[0].ID)
}

func TestListRunsHookFilterSpansBothSources(t *testing.T) {
	s, _, tr, st := newTestServerWithStore(t)
	oldA := persistOld(t, st, "aaaaaaaaaaaaaaaaaaaaaaaaaa", "a", runs.StatusSuccess, time.Hour)
	persistOld(t, st, "bbbbbbbbbbbbbbbbbbbbbbbbbb", "b", runs.StatusSuccess, time.Hour)
	liveA := tr.New("a")

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs?hook=a", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got []runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, liveA.ID(), got[0].ID)
	assert.Equal(t, oldA.ID, got[1].ID)
}

// A ?before= cursor pages the MERGED view: live runs at-or-after it drop
// out, persisted history seeks from strictly before it, a run present in
// both sources still appears exactly once (live wins), and the cursor
// composes with ?hook= and ?max=.
func TestListRunsBeforePagesMergedSources(t *testing.T) {
	s, _, tr, st := newTestServerWithStore(t)

	old := persistOld(t, st, "oldoldoldoldoldoldoldoldol", "h", runs.StatusSuccess, 2*time.Hour)
	other := persistOld(t, st, "otherotherotherotherothero", "x", runs.StatusSuccess, 90*time.Minute)
	mid := persistOld(t, st, "midmidmidmidmidmidmidmidmi", "h", runs.StatusFailure, time.Hour)

	// Finished live run: in the tracker AND (via OnFinish) in the store.
	fin := tr.New("h")
	fin.Finish(runs.StatusFailure, 1, "")
	// Active run: tracker only, newest.
	act := tr.New("h")

	get := func(query string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs?"+query, nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var got []runs.RunState
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		ids := make([]string, 0, len(got))
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		return ids
	}
	cursor := func(at time.Time) string {
		return "before=" + url.QueryEscape(at.Format(time.RFC3339Nano))
	}

	// Cursor at the active run's queued instant: it drops out (strictly
	// before); the finished run — in BOTH sources — appears exactly once,
	// ahead of the store-only history, newest-first.
	atAct := cursor(act.Snapshot(0).Started)
	assert.Equal(t, []string{fin.ID(), mid.ID, other.ID, old.ID}, get(atAct))

	// Paging deeper from the finished run leaves only persisted history.
	assert.Equal(t, []string{mid.ID, other.ID, old.ID}, get(cursor(fin.Snapshot(0).Started)))

	// The hook filter composes with the cursor across both sources...
	assert.Equal(t, []string{fin.ID(), mid.ID, old.ID}, get("hook=h&"+atAct))
	// ...and max still caps the composed page.
	assert.Equal(t, []string{fin.ID(), mid.ID}, get("hook=h&max=2&"+atAct))
}

func TestGetRunFallsBackToStore(t *testing.T) {
	s, _, _, st := newTestServerWithStore(t)
	old := persistOld(t, st, "cccccccccccccccccccccccccc", "h", runs.StatusFailure, time.Hour, "a", "b", "c")

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+old.ID, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got runs.RunState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, old.ID, got.ID)
	assert.Equal(t, runs.StatusFailure, got.Status)
	assert.Equal(t, []string{"a", "b", "c"}, got.Output)
	assert.Len(t, got.OutputTimes, 3)

	// tail trims persisted output exactly like a live snapshot.
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+old.ID+"?tail=2", nil))
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, []string{"b", "c"}, got.Output)
	assert.Len(t, got.OutputTimes, 2)

	// Absent from both sources is still a 404.
	rec = httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHookDetailStatsMergePersisted(t *testing.T) {
	s, reg, tr, st := newTestServerWithStore(t)
	reg.Set(&hooks.Hook{ID: "h", Command: []string{"x"},
		SourcePath: filepath.Join(t.TempDir(), "hook.json")})

	// Persisted-only failure, live success (in both sources via OnFinish),
	// and a live in-flight run.
	persistOld(t, st, "dddddddddddddddddddddddddd", "h", runs.StatusFailure, 30*time.Minute)
	tr.New("h").Finish(runs.StatusSuccess, 0, "")
	running := tr.New("h")
	running.SetRunning()

	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/h", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got HookDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	assert.Equal(t, 3, got.Stats.Tracked, "merged window must dedupe the shared run")
	assert.Equal(t, runs.MaxRunsPerHook, got.Stats.MaxTracked)
	assert.Equal(t, "48h", got.Stats.Retention, "default retention labels the window")
	assert.Equal(t, 2, got.Stats.Completed)
	assert.InDelta(t, 0.5, got.Stats.SuccessRate, 1e-9)
	assert.Equal(t, map[runs.Status]int{
		runs.StatusSuccess: 1, runs.StatusFailure: 1, runs.StatusRunning: 1,
	}, got.Stats.ByStatus)
	require.NotNil(t, got.Stats.LastRun)
	assert.Equal(t, running.ID(), got.Stats.LastRun.ID)
}

func TestHookDetailUnknownStill404WithStore(t *testing.T) {
	s, _, _, st := newTestServerWithStore(t)
	// Even persisted history for the ID doesn't resurrect an unloaded hook.
	persistOld(t, st, "eeeeeeeeeeeeeeeeeeeeeeeeee", "ghost", runs.StatusSuccess, time.Minute)
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/ghost", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no such hook")
}

func TestCompactDuration(t *testing.T) {
	assert.Equal(t, "48h", compactDuration(48*time.Hour))
	assert.Equal(t, "1h30m", compactDuration(90*time.Minute))
	assert.Equal(t, "30s", compactDuration(30*time.Second))
	assert.Equal(t, "1h0m30s", compactDuration(time.Hour+30*time.Second))
	assert.Equal(t, "72h", compactDuration(72*time.Hour))
}
