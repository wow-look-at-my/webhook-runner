package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/spool"
)

// A delivery arriving during a deploy must be answered without an error AND
// must not be lost. Never 503 it on the premise that "the sender redelivers"
// — GitHub does not, which is why the hooks repo needs a delivery-gap replay
// SDK at all.

func drainingServerWithSpool(t *testing.T) (*Server, *spool.Store) {
	t.Helper()
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Description: "d", Command: []string{"x"}})
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool"), nil)
	require.NoError(t, err)
	s.spool = sp
	rn.BeginShutdown()
	return s, sp
}

func postHook(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/hook/h", strings.NewReader(body))
	req.Header.Set("X-Github-Event", "push")
	rec := httptest.NewRecorder()
	s.HookHandler().ServeHTTP(rec, req)
	return rec
}

func TestDrainingDeliveryIsSpooledNotErrored(t *testing.T) {
	s, sp := drainingServerWithSpool(t)

	rec := postHook(s, `{"ref":"refs/heads/master"}`)

	assert.Equal(t, http.StatusAccepted, rec.Code,
		"a parked delivery is accepted, not failed — GitHub records the response and never retries a failure")
	var got map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "spooled", got["status"])
	assert.NotEmpty(t, got["spooled"])
	assert.Equal(t, 1, sp.Len(), "and it is on disk for the next process")
}

func TestSpooledDeliveryPreservesBodyAndHeaders(t *testing.T) {
	s, sp := drainingServerWithSpool(t)
	postHook(s, `{"ref":"refs/heads/topic"}`)

	var got spool.Entry
	require.Equal(t, 1, sp.Replay(func(e spool.Entry) error { got = e; return nil }))

	assert.Equal(t, "h", got.HookID)
	assert.JSONEq(t, `{"ref":"refs/heads/topic"}`, string(got.Body))
	assert.Equal(t, []string{"push"}, got.Headers["X-Github-Event"],
		"the replayed run must be indistinguishable from the live delivery")
}

// A full spool must not silently swallow the delivery: the honest 503 is
// better than a 202 that lies.
func TestFullSpoolFallsBackToTheHonest503(t *testing.T) {
	s, _ := drainingServerWithSpool(t)
	sp, err := spool.Open(filepath.Join(t.TempDir(), "full"), nil)
	require.NoError(t, err)
	s.spool = sp
	_, err = sp.Put(spool.Entry{HookID: "h", Body: []byte("x")})
	require.NoError(t, err)
	sp.SetBounds(1, 0) // already at the bound

	rec := postHook(s, `{}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// With no spool configured there is nowhere to park a delivery, so a
// draining server answers 503 rather than accepting one it will drop.
func TestNoSpoolKeepsThe503Path(t *testing.T) {
	s, reg, _, rn := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Description: "d", Command: []string{"x"}})
	rn.BeginShutdown()

	rec := postHook(s, `{}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// A healthy server must not spool anything — this path is shutdown-only.
func TestHealthyServerDoesNotSpool(t *testing.T) {
	// This delivery is ACCEPTED, unlike its siblings above, so it starts a
	// real async run; newTestServer drains the runner before its TempDir is
	// removed, which is what keeps that from racing the cleanup.
	s, reg, _, _ := newTestServer(t)
	reg.Set(&hooks.Hook{ID: "h", Description: "d", Command: []string{"x"}})
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool"), nil)
	require.NoError(t, err)
	s.spool = sp

	rec := postHook(s, `{}`)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Zero(t, sp.Len(), "a normal delivery runs; it is never parked")
}
