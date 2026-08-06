package server

// Admin reload-panel endpoint tests: the status/commits read views, the
// on-demand check, and — the load-bearing contract — the manual switch's
// server-enforced informed override (409 + every reason without the flag;
// switch only with override:true).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// The production wiring hands the server a *hooks.Repo.
var _ ReloadRepo = (*hooks.Repo)(nil)

// fakePanelRepo scripts the ReloadRepo read surface.
type fakePanelRepo struct {
	head     string
	tip      string
	commits  []string
	subjects map[string]string
	srcAt    map[string]bool
	fetches  int
}

func (f *fakePanelRepo) Head() (string, error)                 { return f.head, nil }
func (f *fakePanelRepo) FetchBranch(depth int) (string, error) { f.fetches++; return f.tip, nil }
func (f *fakePanelRepo) FetchBranchContext(_ context.Context, depth int) (string, error) {
	return f.FetchBranch(depth)
}
func (f *fakePanelRepo) RecentCommits(max int) ([]string, error) {
	if max < len(f.commits) {
		return f.commits[:max], nil
	}
	return f.commits, nil
}
func (f *fakePanelRepo) CommitInfo(sha string) (string, time.Time, error) {
	return f.subjects[sha], time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC), nil
}
func (f *fakePanelRepo) TreeHasDir(sha, path string) bool { return f.srcAt[sha] }

// fakeReloadControl scripts the gate's manual-control surface.
type fakeReloadControl struct {
	status    reloadgate.GateStatus
	ci        map[string]string
	reconcile string

	switchOut reloadgate.SwitchOutcome
	switchErr error
	// recorded ManualSwitch inputs
	gotRef      string
	gotOverride bool
	switches    int
	reconciles  int
}

func (f *fakeReloadControl) Status() reloadgate.GateStatus { return f.status }
func (f *fakeReloadControl) Reconcile(ctx context.Context) string {
	f.reconciles++
	return f.reconcile
}
func (f *fakeReloadControl) ManualSwitch(ctx context.Context, ref string, override bool) (reloadgate.SwitchOutcome, error) {
	f.switches++
	f.gotRef, f.gotOverride = ref, override
	return f.switchOut, f.switchErr
}
func (f *fakeReloadControl) CIState(ctx context.Context, sha string) string {
	if st, ok := f.ci[sha]; ok {
		return st
	}
	return "unknown"
}

func newReloadPanelServer(t *testing.T, repo ReloadRepo, control ReloadControl, onReload func() error, rec *events.Recorder) *Server {
	t.Helper()
	opts := Options{
		Registry:    hooks.NewRegistry(),
		Tracker:     runs.NewTracker(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events:      rec,
		OnReload:    onReload,
		HooksBranch: "master",
	}
	if repo != nil {
		opts.ReloadRepo = repo
	}
	if control != nil {
		opts.ReloadControl = control
	}
	return New(opts)
}

func adminJSON(t *testing.T, s *Server, method, path, body string, want int) map[string]any {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	w := httptest.NewRecorder()
	admin(s).ServeHTTP(w, req)
	require.Equal(t, want, w.Code, "body: %s", w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func TestReloadStatusGated(t *testing.T) {
	repo := &fakePanelRepo{
		head:     "aaaa",
		subjects: map[string]string{"aaaa": "the live one", "cccc": "the held one"},
		srcAt:    map[string]bool{"aaaa": true},
	}
	control := &fakeReloadControl{
		status: reloadgate.GateStatus{
			ServingSHA: "aaaa", Verified: true,
			PendingSHA: "cccc", PendingState: "failure",
			Branch: "master", Context: "all-builds",
		},
		ci: map[string]string{"aaaa": "success", "cccc": "failure"},
	}
	s := newReloadPanelServer(t, repo, control, nil, events.NewRecorder(16))

	out := adminJSON(t, s, http.MethodGet, "/reload/status", "", http.StatusOK)
	assert.Equal(t, "gated", out["mode"])
	assert.Equal(t, "master", out["hooks_branch"])
	assert.Equal(t, "all-builds", out["gate_context"])
	assert.Equal(t, true, out["verified"])

	live := out["live"].(map[string]any)
	assert.Equal(t, "aaaa", live["sha"])
	assert.Equal(t, "aaaa", live["short"])
	assert.Equal(t, "the live one", live["subject"])
	assert.Equal(t, "success", live["ci_state"])
	assert.Equal(t, true, live["has_src"])
	assert.Equal(t, true, live["is_live"])
	assert.NotEmpty(t, live["date"])

	pending := out["pending"].(map[string]any)
	assert.Equal(t, "cccc", pending["sha"])
	assert.Equal(t, "failure", pending["state"])
	assert.Equal(t, "failure", pending["ci_state"])
	assert.Equal(t, false, pending["has_src"])
	assert.Contains(t, pending["why"], "awaiting all-builds")
	assert.Contains(t, pending["why"], "failure")
}

func TestReloadStatusLegacy(t *testing.T) {
	repo := &fakePanelRepo{head: "bbbb", subjects: map[string]string{"bbbb": "legacy head"}, srcAt: map[string]bool{"bbbb": true}}
	s := newReloadPanelServer(t, repo, nil, nil, events.NewRecorder(16))

	out := adminJSON(t, s, http.MethodGet, "/reload/status", "", http.StatusOK)
	assert.Equal(t, "legacy", out["mode"])
	assert.Equal(t, "master", out["hooks_branch"])
	live := out["live"].(map[string]any)
	assert.Equal(t, "bbbb", live["sha"])
	assert.Equal(t, "legacy head", live["subject"])
	assert.Equal(t, "unknown", live["ci_state"], "no gate = no CI reader; unknown, never a guess")
	assert.Nil(t, out["pending"])
	assert.Nil(t, out["verified"])
}

func TestReloadStatusNoRepo(t *testing.T) {
	s := newReloadPanelServer(t, nil, nil, nil, events.NewRecorder(16))
	out := adminJSON(t, s, http.MethodGet, "/reload/status", "", http.StatusOK)
	assert.Equal(t, "none", out["mode"])
	assert.Nil(t, out["live"])
}

func TestReloadCommits(t *testing.T) {
	repo := &fakePanelRepo{
		head:     "bbbb",
		tip:      "cccc",
		commits:  []string{"cccc", "bbbb", "aaaa"},
		subjects: map[string]string{"cccc": "newest", "bbbb": "serving", "aaaa": "oldest"},
		srcAt:    map[string]bool{"cccc": true, "bbbb": true},
	}
	control := &fakeReloadControl{
		status: reloadgate.GateStatus{ServingSHA: "bbbb", Branch: "master", Context: "all-builds"},
		ci:     map[string]string{"cccc": "failure", "bbbb": "success"},
	}
	s := newReloadPanelServer(t, repo, control, nil, events.NewRecorder(16))

	out := adminJSON(t, s, http.MethodGet, "/reload/commits", "", http.StatusOK)
	assert.Equal(t, "master", out["branch"])
	assert.Equal(t, 1, repo.fetches, "the listing fetches first so it is fresh")

	commits := out["commits"].([]any)
	require.Len(t, commits, 3)
	first := commits[0].(map[string]any)
	assert.Equal(t, "cccc", first["sha"])
	assert.Equal(t, "newest", first["subject"])
	assert.Equal(t, "failure", first["ci_state"])
	assert.Equal(t, true, first["has_src"])
	assert.Nil(t, first["is_live"])
	second := commits[1].(map[string]any)
	assert.Equal(t, true, second["is_live"], "the serving commit is marked")
	third := commits[2].(map[string]any)
	assert.Equal(t, "unknown", third["ci_state"], "unscripted CI reads unknown")
	assert.Equal(t, false, third["has_src"])
}

func TestReloadCommitsNoRepo(t *testing.T) {
	s := newReloadPanelServer(t, nil, nil, nil, events.NewRecorder(16))
	adminJSON(t, s, http.MethodGet, "/reload/commits", "", http.StatusNotFound)
}

func TestReloadCheckGated(t *testing.T) {
	rec := events.NewRecorder(16)
	control := &fakeReloadControl{reconcile: "held-blind"}
	s := newReloadPanelServer(t, &fakePanelRepo{}, control, nil, rec)

	out := adminJSON(t, s, http.MethodPost, "/reload/check", "", http.StatusOK)
	assert.Equal(t, "gated", out["mode"])
	assert.Equal(t, "held-blind", out["outcome"])
	assert.Equal(t, 1, control.reconciles, "check runs ONE reconcile pass — the existing logic, not a fork")
	assert.Contains(t, recordedKinds(rec), "reload.check")
}

func TestReloadCheckLegacy(t *testing.T) {
	rec := events.NewRecorder(16)
	reloads := 0
	s := newReloadPanelServer(t, &fakePanelRepo{}, nil, func() error { reloads++; return nil }, rec)

	out := adminJSON(t, s, http.MethodPost, "/reload/check", "", http.StatusOK)
	assert.Equal(t, "reloaded", out["status"])
	assert.Equal(t, 1, reloads, "legacy check runs the verbatim pull+reload path")
	assert.Contains(t, recordedKinds(rec), "reload.requested")
}

func TestReloadSwitchRefusedWithoutOverride(t *testing.T) {
	control := &fakeReloadControl{
		switchOut: reloadgate.SwitchOutcome{
			SHA: "cccc", CIState: "unknown", HasSrc: false,
			Switched: false,
			Reasons:  []string{`all-builds state for cccc is "unknown", not success`, "commit cccc has no src/hooks directory in its tree"},
		},
	}
	s := newReloadPanelServer(t, &fakePanelRepo{subjects: map[string]string{"cccc": "the pick"}}, control, nil, events.NewRecorder(16))

	out := adminJSON(t, s, http.MethodPost, "/reload/switch", `{"ref":"cccc"}`, http.StatusConflict)
	assert.Equal(t, true, out["requires_override"])
	reasons := out["reasons"].([]any)
	require.Len(t, reasons, 2)
	assert.Contains(t, reasons[0], `"unknown"`)
	assert.Contains(t, reasons[1], "src/hooks")
	commit := out["commit"].(map[string]any)
	assert.Equal(t, "cccc", commit["sha"])
	assert.Equal(t, "unknown", commit["ci_state"])
	assert.Equal(t, "the pick", commit["subject"])
	assert.Equal(t, "cccc", control.gotRef)
	assert.False(t, control.gotOverride, "the server must pass override=false through verbatim")
}

func TestReloadSwitchWithOverride(t *testing.T) {
	control := &fakeReloadControl{
		switchOut: reloadgate.SwitchOutcome{
			SHA: "cccc", CIState: "unknown", HasSrc: true,
			Switched: true,
			Reasons:  []string{`all-builds state for cccc is "unknown", not success`},
		},
	}
	s := newReloadPanelServer(t, &fakePanelRepo{}, control, nil, events.NewRecorder(16))

	out := adminJSON(t, s, http.MethodPost, "/reload/switch", `{"ref":"cccc","override":true}`, http.StatusOK)
	assert.Equal(t, "switched", out["status"])
	assert.Equal(t, true, out["overridden"])
	assert.True(t, control.gotOverride)
}

func TestReloadSwitchGreenPath(t *testing.T) {
	control := &fakeReloadControl{
		switchOut: reloadgate.SwitchOutcome{SHA: "dddd", CIState: "success", HasSrc: true, Switched: true},
	}
	s := newReloadPanelServer(t, &fakePanelRepo{}, control, nil, events.NewRecorder(16))

	out := adminJSON(t, s, http.MethodPost, "/reload/switch", `{"ref":"dddd","override":false}`, http.StatusOK)
	assert.Equal(t, "switched", out["status"])
	assert.Equal(t, false, out["overridden"])
	assert.Nil(t, out["reasons"])
}

func TestReloadSwitchLegacyRefused(t *testing.T) {
	s := newReloadPanelServer(t, &fakePanelRepo{}, nil, nil, events.NewRecorder(16))
	out := adminJSON(t, s, http.MethodPost, "/reload/switch", `{"ref":"cccc"}`, http.StatusConflict)
	assert.Contains(t, out["error"], "legacy")
}

func TestReloadSwitchBadRequests(t *testing.T) {
	control := &fakeReloadControl{switchErr: reloadgate.ErrUnknownRef}
	s := newReloadPanelServer(t, &fakePanelRepo{}, control, nil, events.NewRecorder(16))

	adminJSON(t, s, http.MethodPost, "/reload/switch", `{}`, http.StatusBadRequest)
	adminJSON(t, s, http.MethodPost, "/reload/switch", `not json`, http.StatusBadRequest)
	out := adminJSON(t, s, http.MethodPost, "/reload/switch", `{"ref":"nope"}`, http.StatusBadRequest)
	assert.Contains(t, out["error"], "cannot resolve ref")
}

func TestReloadCIStateCaching(t *testing.T) {
	control := &fakeReloadControl{ci: map[string]string{"aaaa": "success"}}
	s := newReloadPanelServer(t, &fakePanelRepo{}, control, nil, events.NewRecorder(16))
	ctx := context.Background()

	assert.Equal(t, "success", s.reloadCIState(ctx, "aaaa"))
	control.ci["aaaa"] = "failure" // the cached terminal verdict keeps serving
	assert.Equal(t, "success", s.reloadCIState(ctx, "aaaa"))
	assert.Equal(t, "unknown", s.reloadCIState(ctx, "eeee"))
}
