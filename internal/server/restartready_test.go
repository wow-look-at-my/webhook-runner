package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// The docker-updater pre-check. 2xx means "replace the container now"; a
// restart with runs in flight loses them AND gets their containers reaped by
// the next boot's orphan sweep, so a wrong 200 kills live CI jobs.

func restartReady(t *testing.T, s *Server) (int, restartReadyResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	admin(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/restart-ready", nil))
	var got restartReadyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return rec.Code, got
}

func TestRestartReadyIdleIsSafe(t *testing.T) {
	s, _, _, _ := newTestServer(t)

	code, got := restartReady(t, s)

	assert.Equal(t, http.StatusOK, code)
	assert.True(t, got.Ready)
	assert.Zero(t, got.ActiveRuns)
}

func TestRestartReadyBlocksOnActiveRun(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	tr.New("h") // a live run: not terminal

	code, got := restartReady(t, s)

	assert.Equal(t, http.StatusServiceUnavailable, code,
		"docker-updater treats any non-2xx as 'skip this cycle'")
	assert.False(t, got.Ready)
	assert.Equal(t, 1, got.ActiveRuns)
	assert.Len(t, got.RunIDs, 1)
}

// A finished run must not keep blocking updates forever.
func TestRestartReadyClearsWhenRunsFinish(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	r := tr.New("h")
	code, _ := restartReady(t, s)
	require.Equal(t, http.StatusServiceUnavailable, code)

	r.Finish(runs.StatusSuccess, 0, "")

	code, got := restartReady(t, s)
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, got.Ready)
}

// LIVENESS: docker-updater retries forever on a non-2xx and has no max-defer
// of its own, so a never-idle fleet would pin the binary at its current
// version — a silent freeze that looks exactly like a working gate.
func TestRestartReadyForcesAfterMaxDefer(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	s.restartMaxDefer = time.Hour
	tr.New("h")

	code, got := restartReady(t, s)
	require.Equal(t, http.StatusServiceUnavailable, code, "blocked at first")
	require.False(t, got.Forced)

	// Backdate the blocked-since clock past the max defer.
	s.restart.mu.Lock()
	s.restart.blockedAt = time.Now().Add(-2 * time.Hour)
	s.restart.mu.Unlock()

	code, got = restartReady(t, s)
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, got.Ready)
	assert.True(t, got.Forced)
	assert.Equal(t, 1, got.ActiveRuns, "honest: it reports the runs it is about to cost")
}

// Only an UNBROKEN busy stretch may reach the force — an idle moment resets
// the clock, so intermittent load can never accumulate its way to a forced
// restart.
func TestRestartReadyIdleResetsTheDeferClock(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	s.restartMaxDefer = time.Hour
	r := tr.New("h")
	require.Equal(t, http.StatusServiceUnavailable, mustCode(t, s))

	s.restart.mu.Lock()
	s.restart.blockedAt = time.Now().Add(-59 * time.Minute)
	s.restart.mu.Unlock()

	r.Finish(runs.StatusSuccess, 0, "") // an idle moment
	require.Equal(t, http.StatusOK, mustCode(t, s))

	tr.New("h2") // busy again — the clock restarts from now
	code, got := restartReady(t, s)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.False(t, got.Forced, "the earlier 59m must not carry over")
}

// A negative max defer means "never force": correctness over liveness, for an
// operator who would rather ship late than kill a job.
func TestRestartReadyNegativeMaxDeferNeverForces(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	s.restartMaxDefer = -1
	tr.New("h")

	s.restart.mu.Lock()
	s.restart.blockedAt = time.Now().Add(-1000 * time.Hour)
	s.restart.mu.Unlock()

	code, got := restartReady(t, s)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.False(t, got.Forced)
}

func TestRestartReadyDefaultsMaxDefer(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	assert.Equal(t, DefaultRestartMaxDefer, s.restartMaxDefer,
		"an unset Options.RestartMaxDefer must not mean 'force immediately'")
}

func TestRestartReadyCapsReportedRunIDs(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	for i := 0; i < restartReadyRunIDCap+5; i++ {
		tr.New("h")
	}

	_, got := restartReady(t, s)

	assert.Equal(t, restartReadyRunIDCap+5, got.ActiveRuns, "the COUNT is the decision")
	assert.Len(t, got.RunIDs, restartReadyRunIDCap, "the id list is a bounded courtesy")
}

func mustCode(t *testing.T, s *Server) int {
	t.Helper()
	code, _ := restartReady(t, s)
	return code
}

// The standard paths are aliases, so they must answer identically to the two
// they alias — including the 503 that holds an update back. A pre-update that
// answered 200 with runs in flight would reap live CI jobs, which is the whole
// reason the gate exists.
func TestWellKnownPathsMirrorTheAdminPair(t *testing.T) {
	s, _, tr, _ := newTestServer(t)

	idle := httptest.NewRecorder()
	admin(s).ServeHTTP(idle, httptest.NewRequest(http.MethodGet, wellKnownPreUpdate, nil))
	assert.Equal(t, http.StatusOK, idle.Code)

	health := httptest.NewRecorder()
	admin(s).ServeHTTP(health, httptest.NewRequest(http.MethodGet, wellKnownHealth, nil))
	assert.Equal(t, http.StatusOK, health.Code)

	tr.New("h") // a live run: not terminal

	busy := httptest.NewRecorder()
	admin(s).ServeHTTP(busy, httptest.NewRequest(http.MethodGet, wellKnownPreUpdate, nil))
	assert.Equal(t, http.StatusServiceUnavailable, busy.Code,
		"pre-update must hold the update back while a run is in flight")

	stillUp := httptest.NewRecorder()
	admin(s).ServeHTTP(stillUp, httptest.NewRequest(http.MethodGet, wellKnownHealth, nil))
	assert.Equal(t, http.StatusOK, stillUp.Code,
		"health answers whether the process is up, which a busy run does not change")
}
