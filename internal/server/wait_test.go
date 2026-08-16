package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// newWaitServer wires the pieces POST /wait touches: the kv store (token
// auth), the run tracker (wait bookkeeping), and an events recorder.
func newWaitServer(t *testing.T) (*Server, *kv.Store, *runs.Tracker, *events.Recorder) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	tr := runs.NewTracker()
	rec := events.NewRecorder(50)
	s := New(Options{Logger: logger, KV: store, Tracker: tr, Events: rec})
	return s, store, tr, rec
}

func TestStateWaitValidation(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	// Auth mirrors every other state route.
	require.Equal(t, 401, stateReq(t, s, "POST", "/wait", "", strings.NewReader(`{"seconds":1,"reason":"x"}`)).Code)
	require.Equal(t, 401, stateReq(t, s, "POST", "/wait", "h.bogus", strings.NewReader(`{"seconds":1,"reason":"x"}`)).Code)

	// Bad bodies are rejected before any blocking happens.
	for _, body := range []string{
		`not json`,
		`{"seconds":0,"reason":"x"}`,
		`{"seconds":-3,"reason":"x"}`,
		`{"seconds":601,"reason":"x"}`,
		`{"seconds":1.5,"reason":"x"}`,
		`{"seconds":5}`,
		`{"seconds":5,"reason":""}`,
		`{"seconds":5,"reason":"   "}`,
		`{"seconds":5,"reason":"` + strings.Repeat("r", maxWaitReasonLen+1) + `"}`,
	} {
		rr := stateReq(t, s, "POST", "/wait", tok, strings.NewReader(body))
		require.Equalf(t, 400, rr.Code, "body=%q -> %s", body, rr.Body.String())
	}

	// A token whose run is unknown, finished, or belongs to another hook
	// has nothing to attribute the wait to: 409, never a block.
	require.Equal(t, 409,
		stateReq(t, s, "POST", "/wait", store.Token("h", "nosuchrun"), strings.NewReader(`{"seconds":1,"reason":"x"}`)).Code)
	require.Equal(t, 409,
		stateReq(t, s, "POST", "/wait", store.Token("other", run.ID()), strings.NewReader(`{"seconds":1,"reason":"x"}`)).Code)
	done := tr.New("h")
	done.Finish(runs.StatusSuccess, 0, "")
	require.Equal(t, 409,
		stateReq(t, s, "POST", "/wait", store.Token("h", done.ID()), strings.NewReader(`{"seconds":1,"reason":"x"}`)).Code)
}

func TestStateWaitWithoutTracker(t *testing.T) {
	// A server with a kv store but no tracker (possible in odd wiring) must
	// refuse rather than dereference nil or block unattributed.
	s, store := newStateServer(t, kv.Config{})
	rr := stateReq(t, s, "POST", "/wait", store.Token("h", "r1"), strings.NewReader(`{"seconds":1,"reason":"x"}`))
	require.Equal(t, 503, rr.Code)
}

// The happy path: the call blocks for the requested duration, returns
// {"waited": N}, and leaves no wait state behind.
func TestStateWaitBlocksFullDuration(t *testing.T) {
	s, store, tr, rec := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	start := time.Now()
	rr := stateReq(t, s, "POST", "/wait", tok, strings.NewReader(`{"seconds":1,"reason":"settling"}`))
	elapsed := time.Since(start)

	require.Equal(t, 200, rr.Code)
	require.JSONEq(t, `{"waited":1}`, rr.Body.String())
	assert.GreaterOrEqual(t, elapsed, 950*time.Millisecond, "the wait must actually block")

	assert.Nil(t, run.Snapshot(-1).WaitingOn)

	// The start of the wait landed on the activity feed, hook-scoped.
	evs := rec.ListByHook("h", 10)
	require.NotEmpty(t, evs)
	assert.Equal(t, "run.wait", evs[0].Kind)
	assert.Contains(t, evs[0].Msg, "waiting 1s: settling")
}

// While a wait is in flight the run is visibly waiting — on the Run snapshot
// and through the admin run endpoints — and a cancel request interrupts it
// early with a distinguishable body.
func TestStateWaitVisibleAndCancelInterrupts(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/wait", tok, strings.NewReader(`{"seconds":30,"reason":"green-settle"}`))
	}()

	require.Eventually(t, func() bool { return run.Snapshot(0).WaitingOn != nil }, 2*time.Second, 5*time.Millisecond)
	snap := run.Snapshot(0)
	require.NotNil(t, snap.WaitingOn)
	assert.Equal(t, runs.WaitingOnWait, snap.WaitingOn.Kind)
	assert.Equal(t, "green-settle", snap.WaitingOn.Reason)
	assert.WithinDuration(t, time.Now().Add(30*time.Second), snap.WaitingOn.Until, 3*time.Second)

	// The dashboard reads these via /runs and /runs/{id}: field names are
	// part of the contract.
	detail := httptest.NewRecorder()
	admin(s).ServeHTTP(detail, httptest.NewRequest("GET", "/runs/"+run.ID(), nil))
	require.Equal(t, 200, detail.Code)
	assert.Contains(t, detail.Body.String(), `"waiting_on"`)
	assert.Contains(t, detail.Body.String(), `"kind": "wait"`)
	assert.Contains(t, detail.Body.String(), `"reason": "green-settle"`)
	assert.Contains(t, detail.Body.String(), `"until"`)

	list := httptest.NewRecorder()
	admin(s).ServeHTTP(list, httptest.NewRequest("GET", "/runs?hook=h", nil))
	require.Equal(t, 200, list.Code)
	assert.Contains(t, list.Body.String(), `"reason": "green-settle"`)

	run.RequestCancel()
	select {
	case rr := <-respCh:
		require.Equal(t, 200, rr.Code)
		var res struct {
			Waited      int    `json:"waited"`
			Interrupted bool   `json:"interrupted"`
			Cause       string `json:"cause"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
		assert.True(t, res.Interrupted)
		assert.Equal(t, "run cancelled", res.Cause)
		assert.Less(t, res.Waited, 30)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not interrupt the wait")
	}
	assert.Nil(t, run.Snapshot(0).WaitingOn, "wait state must clear when the wait ends")
}

func TestStateWaitRunFinishInterrupts(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/wait", tok, strings.NewReader(`{"seconds":30,"reason":"outliving the run"}`))
	}()
	require.Eventually(t, func() bool { return run.Snapshot(0).WaitingOn != nil }, 2*time.Second, 5*time.Millisecond)

	run.Finish(runs.StatusSuccess, 0, "")
	select {
	case rr := <-respCh:
		require.Equal(t, 200, rr.Code)
		assert.Contains(t, rr.Body.String(), `"interrupted": true`)
		assert.Contains(t, rr.Body.String(), `"cause": "run finished"`)
	case <-time.After(5 * time.Second):
		t.Fatal("run finish did not interrupt the wait")
	}
	assert.Nil(t, run.Snapshot(0).WaitingOn)
}

func TestStateWaitClientDisconnect(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	run := tr.New("h")
	run.SetRunning()
	tok := store.Token("h", run.ID())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/wait", strings.NewReader(`{"seconds":30,"reason":"caller vanishes"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		state(s).ServeHTTP(rr, req)
		close(done)
	}()
	require.Eventually(t, func() bool { return run.Snapshot(0).WaitingOn != nil }, 2*time.Second, 5*time.Millisecond)

	cancel() // the hook's container went away mid-wait
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client disconnect did not release the wait")
	}
	assert.Nil(t, run.Snapshot(0).WaitingOn)
}

// writeSilentSleepDocker fakes a container that produces NO output and
// sleeps ~1.5s — long past the test hook's idle timeout. `docker kill` is
// accepted but doesn't kill anything; the runner's 2s process-kill backstop
// ends a timed-out run.
func writeSilentSleepDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "docker")
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then exit 0; fi
if [ "$1" = "image" ] || [ "$1" = "build" ]; then exit 0; fi
sleep 1.5
exit 0
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// THE load-bearing interaction test: a run whose idle `timeout` is far
// shorter than its silence is killed when it just goes quiet — but survives
// when it has DECLARED that silence via POST /wait, because the in-flight
// wait keeps counting as activity. End-to-end through the real runner
// watchdog and the real state handler.
func TestStateWaitCountsAsWatchdogActivity(t *testing.T) {
	dir := t.TempDir()
	docker := writeSilentSleepDocker(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, KV: store})

	hookDir := filepath.Join(dir, "napper")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	h := &hooks.Hook{
		ID:             "napper",
		SourcePath:     filepath.Join(hookDir, "hook.json"),
		Command:        []string{"x"},
		State:          true,
		IdleTimeoutRaw: "500ms", // idle limit far below the 1.5s of silence
	}
	reg.Set(h)

	// Control: the same silent container with no declared wait is killed
	// for silence.
	ctrl, err := rn.Start(context.Background(), h, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	select {
	case <-ctrl.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("control run did not finish")
	}
	require.Equal(t, runs.StatusTimeout, ctrl.Status())
	require.Contains(t, ctrl.Error(), "no output")

	// Protected: identical silence, but announced via /wait — the wait
	// touches the watchdog, the container finishes on its own, success.
	run, err := rn.Start(context.Background(), h, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/wait", store.Token("napper", run.ID()),
			strings.NewReader(`{"seconds":10,"reason":"napping on purpose"}`))
	}()

	select {
	case <-run.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("protected run did not finish")
	}
	rn.Wait()
	require.Equal(t, runs.StatusSuccess, run.Status(),
		"a declared wait must count as activity for the idle timeout")

	select {
	case rr := <-respCh:
		require.Equal(t, 200, rr.Code)
		// The run ended before the 10s wait elapsed, so the wait reports
		// the interruption rather than a full wait.
		assert.Contains(t, rr.Body.String(), `"interrupted": true`)
	case <-time.After(5 * time.Second):
		t.Fatal("wait call did not return after the run finished")
	}
}
