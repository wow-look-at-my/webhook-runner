package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// newEventsServer wires the minimum GET /events touches: a registry, a
// tracker, and a real recorder (newTestServer leaves Events nil, which the
// nil-safe recorder answers with an empty feed).
func newEventsServer(t *testing.T) (*Server, *events.Recorder) {
	t.Helper()
	rec := events.NewRecorder(500)
	s := New(Options{
		Registry: hooks.NewRegistry(),
		Tracker:  runs.NewTracker(),
		Events:   rec,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:  testVersion,
	})
	return s, rec
}

func getEvents(t *testing.T, s *Server, query string) []events.Event {
	t.Helper()
	rr := httptest.NewRecorder()
	admin(s).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, query, nil))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var got []events.Event
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	return got
}

// The dashboard drops run lifecycle from both feeds (it belongs to the runs
// table). The exclusion has to happen BEFORE max or it defeats itself on
// exactly the hooks it matters for: a skip flood fills the page, the filter
// empties it, and the panel reads "nothing here" while the events the
// operator came for sit just behind the burst.
func TestEventsExcludeFiltersBeforeTheCap(t *testing.T) {
	s, rec := newEventsServer(t)

	rec.Record("hook.denied", "bad signature", map[string]string{"hook": "h"})
	rec.Record("image.built", "built whr-hook/h:abc", map[string]string{"hook": "h"})
	// The shape that breaks a page-then-filter implementation.
	for i := range 55 {
		rec.Record("run.skipped", fmt.Sprintf("skip %d", i), map[string]string{"hook": "h"})
	}

	for _, q := range []string{
		"/events?max=10&exclude=run",
		"/events?hook=h&max=10&exclude=run",
	} {
		got := getEvents(t, s, q)
		require.Lenf(t, got, 2, "query %s returned %d events", q, len(got))
		assert.Equal(t, "image.built", got[0].Kind) // newest first
		assert.Equal(t, "hook.denied", got[1].Kind)
	}

	// Without the exclusion the same window is nothing but the burst — which is the duplication the dashboard now drops.
	unfiltered := getEvents(t, s, "/events?max=10")
	require.Len(t, unfiltered, 10)
	assert.Equal(t, "run.skipped", unfiltered[0].Kind)
}

func TestEventsExcludeRejectsAKindInPlaceOfAFamily(t *testing.T) {
	s, rec := newEventsServer(t)
	rec.Record("run.started", "x", map[string]string{"hook": "h"})

	rr := httptest.NewRecorder()
	admin(s).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/events?exclude=run.started", nil))
	// Silently matching nothing would read as "the filter did not work".
	require.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "families")
}

func TestEventsExcludeAcceptsSeveralFamilies(t *testing.T) {
	s, rec := newEventsServer(t)
	rec.Record("run.started", "run", map[string]string{"hook": "h"})
	rec.Record("image.built", "image", map[string]string{"hook": "h"})
	rec.Record("hook.denied", "denied", map[string]string{"hook": "h"})

	got := getEvents(t, s, "/events?exclude=run,%20image")
	require.Len(t, got, 1)
	assert.Equal(t, "hook.denied", got[0].Kind)

	// An empty entry is ignored rather than treated as a family that matches everything.
	assert.Len(t, getEvents(t, s, "/events?exclude=run,,"), 2)
}
