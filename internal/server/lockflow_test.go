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

func eventKinds(evs []events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// A contended acquire is never anonymous: the names exactly who holds
// the lock — run, hook, and since when.
func TestStateLockContendedNamesHolder(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "pr-7", holder.ID(), 0)
	require.NoError(t, err)

	contender := tr.New("h")
	contender.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/pr-7/acquire", store.Token("h", contender.ID()), nil)
	require.Equal(t, 409, rr.Code)

	var res struct {
		Error  string `json:"error"`
		HeldBy struct {
			RunID      string    `json:"run_id"`
			HookID     string    `json:"hook_id"`
			AcquiredAt time.Time `json:"acquired_at"`
		} `json:"held_by"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	assert.NotEmpty(t, res.Error)
	assert.Equal(t, holder.ID(), res.HeldBy.RunID)
	assert.Equal(t, "h", res.HeldBy.HookID)
	assert.False(t, res.HeldBy.AcquiredAt.IsZero())
}

// {"block": true}: a contended acquire is held open and completes the
// moment the holder releases — and while blocked, the waiter's run is
// visibly waiting on the named holder.
func TestStateLockBlockingAcquireWaitsForRelease(t *testing.T) {
	s, store, tr, rec := newWaitServer(t)
	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "gate", holder.ID(), 0)
	require.NoError(t, err)

	blocked := tr.New("h")
	blocked.SetRunning()
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", blocked.ID()),
			strings.NewReader(`{"block":true,"block_timeout_seconds":10}`))
	}()

	require.Eventually(t, func() bool {
		w := blocked.Snapshot(0).WaitingOn
		return w != nil && w.Kind == runs.WaitingOnLock && w.HolderRunID == holder.ID()
	}, 2*time.Second, 5*time.Millisecond)
	w := blocked.Snapshot(0).WaitingOn
	assert.Equal(t, "gate", w.Key)
	assert.Equal(t, "h", w.HolderHookID)
	assert.Contains(t, eventKinds(rec.ListByHook("h", 20)), "lock.waiting")

	require.NoError(t, store.ReleaseLock("h", "gate", holder.ID()))
	select {
	case rr := <-respCh:
		require.Equal(t, 200, rr.Code)
		assert.Contains(t, rr.Body.String(), blocked.ID(), "the waiter owns the lock now")
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not complete after release")
	}
	assert.Nil(t, blocked.Snapshot(0).WaitingOn, "waiting state must clear on acquire")
}

// A blocking acquire that never gets the lock gives up at its block timeout
// with the same holder-identified as an immediate contended acquire.
func TestStateLockBlockingTimeoutReportsHolder(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "gate", holder.ID(), 0)
	require.NoError(t, err)

	blocked := tr.New("h")
	blocked.SetRunning()
	start := time.Now()
	rr := stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", blocked.ID()),
		strings.NewReader(`{"block":true,"block_timeout_seconds":1}`))
	elapsed := time.Since(start)

	require.Equal(t, 409, rr.Code)
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond, "must block for the timeout")
	assert.Contains(t, rr.Body.String(), "still held")
	assert.Contains(t, rr.Body.String(), holder.ID())
	assert.Nil(t, blocked.Snapshot(0).WaitingOn)
}

func TestStateLockBlockingValidation(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "gate", holder.ID(), 0)
	require.NoError(t, err)

	live := tr.New("h")
	live.SetRunning()
	tok := store.Token("h", live.ID())
	for _, body := range []string{
		`{"block":true,"block_timeout_seconds":0}`,
		`{"block":true,"block_timeout_seconds":601}`,
	} {
		rr := stateReq(t, s, "POST", "/kv/gate/acquire", tok, strings.NewReader(body))
		require.Equalf(t, 400, rr.Code, "body=%q -> %s", body, rr.Body.String())
	}

	// A contended blocking acquire from a run the tracker doesn't know (or
	// that finished) cannot be attributed: refuse rather than hold.
	rr := stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", "ghost"),
		strings.NewReader(`{"block":true}`))
	require.Equal(t, 409, rr.Code)
	assert.Contains(t, rr.Body.String(), "run is not active")

	// Steal never blocks — it takes unconditionally, so the flag is a caller bug.
	rr = stateReq(t, s, "POST", "/kv/gate/steal", tok, strings.NewReader(`{"block":true}`))
	require.Equal(t, 400, rr.Code)
}

// THE load-bearing watchdog test: a blocked acquire counts as
// activity exactly like a declared wait — a silent container blocked on a
// contended lock past its idle timeout is NOT reaped.
func TestStateLockBlockingFeedsWatchdog(t *testing.T) {
	dir := t.TempDir()
	docker := writeSilentSleepDocker(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	reg := hooks.NewRegistry()
	tr := runs.NewTracker()
	rn := runner.New(runner.Options{Tracker: tr, Logger: logger, TmpDir: dir, Docker: docker})
	s := New(Options{Registry: reg, Runner: rn, Tracker: tr, Logger: logger, KV: store})

	hookDir := filepath.Join(dir, "locker")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	h := &hooks.Hook{
		ID:             "locker",
		SourcePath:     filepath.Join(hookDir, "hook.json"),
		Command:        []string{"x"},
		State:          true,
		IdleTimeoutRaw: "500ms", // idle limit far below the .s of silence
	}
	reg.Set(h)

	// Another (tracker-only) run holds the lock for the whole test.
	holder := tr.New("locker")
	holder.SetRunning()
	_, err = store.AcquireLock("locker", "gate", holder.ID(), 0)
	require.NoError(t, err)

	run, err := rn.Start(context.Background(), h, []byte("p"), http.Header{}, "")
	require.NoError(t, err)
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("locker", run.ID()),
			strings.NewReader(`{"block":true,"block_timeout_seconds":10}`))
	}()

	select {
	case <-run.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("run did not finish")
	}
	rn.Wait()
	require.Equal(t, runs.StatusSuccess, run.Status(),
		"a blocked lock acquire must count as activity for the idle timeout")

	select {
	case rr := <-respCh:
		// The lock never freed; the run ended , interrupting the hold.
		require.Equal(t, 409, rr.Code)
		assert.Contains(t, rr.Body.String(), "interrupted: run finished")
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not return after the run finished")
	}
}

// The full steal story: the thief takes the lock atomically, the displaced
// holder is cancelled with a reason that names the steal, and when the
// victim terminates the finish seam frees its OTHER locks while the stolen
// stays the thief's (transfer, never release).
func TestStateLockStealCancelsHolderAndTransfers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	tr := runs.NewTracker()
	rec := events.NewRecorder(50)
	// The real finish seam, so the victim's terminal state exercises the transfer-vs-release invariant end to end.
	tr.SetOnFinish(RunFinishCallback(store, func(runs.RunState) error { return nil }, rec, logger))
	s := New(Options{Logger: logger, KV: store, Tracker: tr, Events: rec})

	victim := tr.New("h")
	victim.SetRunning()
	_, err = store.AcquireLock("h", "pr-7", victim.ID(), 0)
	require.NoError(t, err)
	_, err = store.AcquireLock("h", "other", victim.ID(), 0)
	require.NoError(t, err)

	thief := tr.New("h")
	thief.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/pr-7/steal", store.Token("h", thief.ID()), nil)
	require.Equal(t, 200, rr.Code)

	var res struct {
		RunID      string `json:"run_id"`
		StolenFrom *struct {
			RunID  string `json:"run_id"`
			HookID string `json:"hook_id"`
		} `json:"stolen_from"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	assert.Equal(t, thief.ID(), res.RunID)
	require.NotNil(t, res.StolenFrom)
	assert.Equal(t, victim.ID(), res.StolenFrom.RunID)
	assert.Equal(t, "h", res.StolenFrom.HookID)

	// The victim was cancelled, and the reason names the steal — it will land in run history as the terminal error.
	assert.True(t, victim.Snapshot(0).CancelRequested)
	assert.Contains(t, victim.CancelReason(), `lock "pr-7" stolen by run `+thief.ID())
	assert.Contains(t, eventKinds(rec.ListByHook("h", 20)), "lock.stolen")

	// Victim terminates (the runner does this after the kill); its OTHER lock frees, the stolen stays the thief's.
	victim.Finish(runs.StatusCancelled, -1, victim.CancelReason())
	_, err = store.AcquireLock("h", "other", "run-z", 0)
	require.NoError(t, err, "the victim's other lock must release on finish")
	holderInfo, err := store.AcquireLock("h", "pr-7", "run-z", 0)
	require.ErrorIs(t, err, kv.ErrLockHeld, "the stolen lock must stay held")
	assert.Equal(t, thief.ID(), holderInfo.RunID)
}

// Steal of a free lock is exactly an acquire: , no stolen_from, nobody
// cancelled.
func TestStateLockStealFreeLockIsAcquire(t *testing.T) {
	s, store, tr, rec := newWaitServer(t)
	thief := tr.New("h")
	thief.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/free/steal", store.Token("h", thief.ID()), nil)
	require.Equal(t, 200, rr.Code)
	assert.NotContains(t, rr.Body.String(), "stolen_from")
	assert.NotContains(t, eventKinds(rec.ListByHook("h", 20)), "lock.stolen")
}

// The steal-vs-normal-finish race: the holder finished (its locks already
// released by the finish seam) before the steal landed — the steal degrades
// to a plain acquire and no cancel is issued.
func TestStateLockStealAfterHolderFinished(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "gate", holder.ID(), 0)
	require.NoError(t, err)

	// Holder finishes normally; the finish seam frees its locks.
	holder.Finish(runs.StatusSuccess, 0, "")
	store.ReleaseRunLocks(holder.ID())

	thief := tr.New("h")
	thief.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/gate/steal", store.Token("h", thief.ID()), nil)
	require.Equal(t, 200, rr.Code)
	assert.NotContains(t, rr.Body.String(), "stolen_from")
	assert.Empty(t, holder.CancelReason())
	assert.False(t, holder.Snapshot(0).CancelRequested)
}

// The dashboard contract for lock waits: the blocked run's waiting_on names
// the lock and its holder, and the HOLDER's runs carry the derived waiters
// list — on /runs, /runs/{id}, and the per-hook slice.
func TestStateLockWaitingOnAndWaitersJSON(t *testing.T) {
	s, store, tr, _ := newWaitServer(t)
	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "pr-7", holder.ID(), 0)
	require.NoError(t, err)

	blocked := tr.New("h")
	blocked.SetRunning()
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- stateReq(t, s, "POST", "/kv/pr-7/acquire", store.Token("h", blocked.ID()),
			strings.NewReader(`{"block":true,"block_timeout_seconds":10}`))
	}()
	require.Eventually(t, func() bool { return blocked.Snapshot(0).WaitingOn != nil }, 2*time.Second, 5*time.Millisecond)

	// The waiter side: /runs/{id} names the lock and its holder.
	detail := httptest.NewRecorder()
	admin(s).ServeHTTP(detail, httptest.NewRequest("GET", "/runs/"+blocked.ID(), nil))
	require.Equal(t, 200, detail.Code)
	var detailState runs.RunState
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &detailState))
	require.NotNil(t, detailState.WaitingOn)
	assert.Equal(t, "lock", detailState.WaitingOn.Kind)
	assert.Equal(t, "pr-7", detailState.WaitingOn.Key)
	assert.Equal(t, holder.ID(), detailState.WaitingOn.HolderRunID)
	assert.Equal(t, "h", detailState.WaitingOn.HolderHookID)

	// The holder side: /runs/{id} carries the derived waiters list.
	hd := httptest.NewRecorder()
	admin(s).ServeHTTP(hd, httptest.NewRequest("GET", "/runs/"+holder.ID(), nil))
	require.Equal(t, 200, hd.Code)
	var holderState runs.RunState
	require.NoError(t, json.Unmarshal(hd.Body.Bytes(), &holderState))
	require.NotEmpty(t, holderState.Waiters)
	assert.Equal(t, blocked.ID(), holderState.Waiters[0].RunID)
	assert.Equal(t, "pr-7", holderState.Waiters[0].Key)

	// And the list view is decorated the same way.
	list := httptest.NewRecorder()
	admin(s).ServeHTTP(list, httptest.NewRequest("GET", "/runs?hook=h", nil))
	require.Equal(t, 200, list.Code)
	assert.Contains(t, list.Body.String(), `"waiters"`)

	blocked.RequestCancel() // release the held request
	select {
	case rr := <-respCh:
		require.Equal(t, 409, rr.Code)
		assert.Contains(t, rr.Body.String(), "interrupted: run cancelled")
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not release the blocking acquire")
	}
}

// ---- TTL enforcement: kill the holder, confirm it is dead, THEN hand over ----
//
// The operator's mutex ruling, verbatim: "never have a TTL on a mutex, that
// doesn't make sense. Or, if you want to have a mutex TTL, you need to force
// kill the thing that's holding it when the time is up. THAT FORCE KILL
// COMPLETES AND THAT JOB IS CERTAIN TO BE DEAD, then the mutex would be freed
// automatically due to the ending job, and the TTL has been enforced." These
// tests pin exactly that sequence — and its refusals.

// ttlServer is the steal test's wiring: a real tracker plus the real finish
// seam, so a killed holder's locks free the way they do in production.
func ttlServer(t *testing.T) (*Server, *kv.Store, *runs.Tracker, *events.Recorder) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := kv.New(kv.Config{Dir: filepath.Join(t.TempDir(), "kv")}, []byte("server-test-secret"), logger)
	require.NoError(t, err)
	tr := runs.NewTracker()
	rec := events.NewRecorder(50)
	tr.SetOnFinish(RunFinishCallback(store, func(runs.RunState) error { return nil }, rec, logger))
	return New(Options{Logger: logger, KV: store, Tracker: tr, Events: rec}), store, tr, rec
}

// The whole sequence: a LIVE holder past its TTL is cancelled by the
// contender's acquire, the acquire waits for it to actually die, and only
// the finish-seam release hands the lock over.
func TestStateLockTTLKillsHolderThenHandsOver(t *testing.T) {
	s, store, tr, rec := ttlServer(t)

	victim := tr.New("h")
	victim.SetRunning()
	_, err := store.AcquireLock("h", "gate", victim.ID(), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond) // over budget

	// Stand in for the runner: the container dies on the cancel signal, and the run reaches a terminal state — which is what frees the lock.
	died := make(chan struct{})
	go func() {
		<-victim.Cancelled()
		victim.Finish(runs.StatusCancelled, -1, victim.CancelReason())
		close(died)
	}()

	contender := tr.New("h")
	contender.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", contender.ID()), nil)
	require.Equal(t, 200, rr.Code, "the lock is handed over once the holder is certainly dead")
	require.Contains(t, rr.Body.String(), contender.ID())

	select {
	case <-died:
	case <-time.After(2 * time.Second):
		t.Fatal("the over-budget holder was never killed")
	}
	assert.True(t, victim.Snapshot(0).CancelRequested, "the TTL is ENFORCED against the holder, not assumed")
	assert.Contains(t, victim.CancelReason(), `lock "gate" held past its TTL`)
	assert.Contains(t, eventKinds(rec.ListByHook("h", 20)), "lock.ttl_enforced")
}

// A holder that will not die does NOT lose its lock: the contender is
// refused, because handing a mutex to a live run is the failure the
// kill exists to prevent.
func TestStateLockTTLRefusesWhenTheKillDoesNotComplete(t *testing.T) {
	s, store, tr, _ := ttlServer(t)
	old := lockKillTimeout
	lockKillTimeout = 50 * time.Millisecond
	t.Cleanup(func() { lockKillTimeout = old })

	victim := tr.New("h")
	victim.SetRunning()
	_, err := store.AcquireLock("h", "gate", victim.ID(), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	contender := tr.New("h")
	contender.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", contender.ID()), nil)
	require.Equal(t, 409, rr.Code)
	assert.Contains(t, rr.Body.String(), "did not terminate")
	assert.Contains(t, rr.Body.String(), victim.ID(), "the refusal still names the holder")

	// And the lock really is still the victim's.
	info, err := store.AcquireLock("h", "gate", "run-z", 0)
	require.Error(t, err)
	assert.Equal(t, victim.ID(), info.RunID)
}

// The release-path bug the backstop exists for: the holder is already gone
// but its lock was never freed. Nothing to kill — the shell is reaped and
// the lock handed over immediately.
func TestStateLockTTLReapsAlreadyDeadHolder(t *testing.T) {
	s, store, tr, rec := ttlServer(t)

	ghost := tr.New("h")
	ghost.SetRunning()
	_, err := store.AcquireLock("h", "gate", ghost.ID(), time.Millisecond)
	require.NoError(t, err)
	// Terminal WITHOUT the finish seam having freed the lock (the bug).
	ghost.Finish(runs.StatusError, 1, "boom")
	_, err = store.AcquireLock("h", "gate", ghost.ID(), time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	contender := tr.New("h")
	contender.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", contender.ID()), nil)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, eventKinds(rec.ListByHook("h", 20)), "lock.ttl_reaped")
}

// An UNEXPIRED hold is untouched by any of this: plain , holder alive and
// uncancelled. TTL enforcement must never fire early.
func TestStateLockWithinTTLIsPlainContention(t *testing.T) {
	s, store, tr, _ := ttlServer(t)

	holder := tr.New("h")
	holder.SetRunning()
	_, err := store.AcquireLock("h", "gate", holder.ID(), time.Minute)
	require.NoError(t, err)

	contender := tr.New("h")
	contender.SetRunning()
	rr := stateReq(t, s, "POST", "/kv/gate/acquire", store.Token("h", contender.ID()), nil)
	require.Equal(t, 409, rr.Code)
	assert.False(t, holder.Snapshot(0).CancelRequested, "a holder within its budget is never killed")
}

// Manager-instance lock blocking is covered in lockflow_manager_test.go.
