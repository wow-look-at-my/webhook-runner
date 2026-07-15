package server

// GET /attention + the "attention" changed-section signal: the endpoint
// serves the aggregator's current problem set, and every real mutation —
// direct reports, reload re-derivations, event-derived entries fed through
// the recorder — pushes an "attention" signal on /runs/stream so the
// dashboard's banner/panel refetch without polling.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

type attentionResp struct {
	Count   int `json:"count"`
	Entries []struct {
		Source  string `json:"source"`
		Hook    string `json:"hook"`
		Key     string `json:"key"`
		Message string `json:"message"`
		Since   string `json:"since"`
	} `json:"entries"`
}

func getAttention(t *testing.T, h http.Handler) attentionResp {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/attention", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var out attentionResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

// Without an aggregator the endpoint still answers — zero problems, an
// empty (never null) entries array.
func TestAttentionEndpointNilAggregator(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	got := getAttention(t, admin(s))
	assert.Equal(t, 0, got.Count)
	require.NotNil(t, got.Entries)
	assert.Empty(t, got.Entries)
}

// The populated shape: count + entries with source/hook/key/message/since,
// oldest first.
func TestAttentionEndpointServesEntries(t *testing.T) {
	agg := attention.New()
	s := New(Options{
		Registry:  hooks.NewRegistry(),
		Tracker:   runs.NewTracker(),
		Attention: agg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:   testVersion,
	})
	agg.Report(attention.Entry{
		Source:  attention.SourceServer,
		Key:     attention.KeyTmpDir,
		Message: "tmpdir hazard",
	})
	agg.ReplaceSource(attention.SourceLoad, []attention.Entry{
		{Hook: "broken", Key: attention.KeyLoad, Message: "failed to load"},
	})

	got := getAttention(t, admin(s))
	require.Equal(t, 2, got.Count)
	require.Len(t, got.Entries, 2)
	assert.Equal(t, attention.SourceServer, got.Entries[0].Source)
	assert.Equal(t, attention.KeyTmpDir, got.Entries[0].Key)
	assert.NotEmpty(t, got.Entries[0].Since)
	assert.Equal(t, "broken", got.Entries[1].Hook)

	// The clear rule over HTTP: a clean re-derivation empties the panel.
	agg.ReplaceSource(attention.SourceLoad, nil)
	agg.Resolve(attention.SourceServer, "", attention.KeyTmpDir)
	got = getAttention(t, admin(s))
	assert.Equal(t, 0, got.Count)
	assert.Empty(t, got.Entries)
}

// The push path, end to end over real HTTP: an aggregator mutation —
// including one derived from a recorded activity event through the
// server's recorder wiring — dirties the "attention" section on
// /runs/stream.
func TestStreamSectionSignalsOnAttention(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := events.NewRecorder(64)
	agg := attention.New()
	attention.RegisterStandardEventRules(agg)
	s := New(Options{
		Registry:  hooks.NewRegistry(),
		Tracker:   runs.NewTracker(),
		Events:    rec,
		Attention: agg,
		Logger:    logger,
		Version:   testVersion,
	})

	srv := httptest.NewServer(admin(s))
	defer srv.Close()
	evs, resp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer resp.Body.Close()
	nextEvent(t, evs, "retry")
	nextEvent(t, evs, "snapshot")

	// A direct state mutation (what the reload path and the boot verdict do).
	agg.Report(attention.Entry{Source: attention.SourceServer, Key: attention.KeyTmpDir, Message: "hazard"})
	waitChangedCovering(t, evs, "attention")

	// The event seam through the RECORDER: auth.go records
	// hook.misconfigured; the server's OnRecord wiring must feed the
	// aggregator, whose change signals attention (and the event itself
	// dirties events as always).
	rec.Record("hook.misconfigured",
		"h: api_key reference ${X} did not resolve (secrets.sops.env / host env); all callers are denied",
		map[string]string{"hook": "h"})
	waitChangedCovering(t, evs, "attention", "events")
	assert.Equal(t, 2, agg.Count(), "the recorded event must have derived an entry")
}
