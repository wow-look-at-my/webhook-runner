package server

// /runs/stream: the SSE live tail. These tests exercise the real HTTP
// surface (httptest.NewServer — a ResponseRecorder can't hang up, and the
// handler intentionally never returns on its own), the hub's
// never-block/drop-slow-clients contract, and shape parity with /runs.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	name string
	data string
}

// readSSE parses events off the stream, sending each on the returned
// channel until the body ends. Comment lines are ignored (they are not
// events), which is exactly how EventSource treats them.
func readSSE(t *testing.T, body io.Reader) <-chan sseEvent {
	t.Helper()
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		cur := sseEvent{}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.name != "" || cur.data != "" {
					out <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, ":"):
				// comment — dispatches nothing
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "retry: "):
				out <- sseEvent{name: "retry", data: strings.TrimPrefix(line, "retry: ")}
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, ch <-chan sseEvent, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "stream ended waiting for %s", what)
		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return sseEvent{}
	}
}

// nextRunEvent skips heartbeats and returns the next `run` delta.
func nextRunEvent(t *testing.T, ch <-chan sseEvent, what string) runs.RunState {
	t.Helper()
	for {
		ev := nextEvent(t, ch, what)
		if ev.name == "hb" {
			continue
		}
		require.Equal(t, "run", ev.name, "expected a run delta for %s, got %q (%s)", what, ev.name, ev.data)
		var st runs.RunState
		require.NoError(t, json.Unmarshal([]byte(ev.data), &st))
		return st
	}
}

func openStream(t *testing.T, baseURL string) (<-chan sseEvent, *http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/runs/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	return readSSE(t, resp.Body), resp, cancel
}

func TestRunsStreamEndToEnd(t *testing.T) {
	old := streamHeartbeat
	streamHeartbeat = 150 * time.Millisecond
	defer func() { streamHeartbeat = old }()

	s, _, tr, _ := newTestServer(t)
	// One pre-existing run so the connect snapshot has content.
	prior := tr.New("seed-hook")
	prior.Finish(runs.StatusSuccess, 0, "")

	srv := httptest.NewServer(admin(s))
	defer srv.Close()

	events, resp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer resp.Body.Close()

	// 1. Fixed retry directive first.
	ev := nextEvent(t, events, "retry directive")
	require.Equal(t, "retry", ev.name)
	assert.Equal(t, "2000", ev.data)

	// 2. Connect snapshot: same shape as /runs, includes the prior run.
	ev = nextEvent(t, events, "snapshot")
	require.Equal(t, "snapshot", ev.name)
	var snap []runs.RunState
	require.NoError(t, json.Unmarshal([]byte(ev.data), &snap))
	require.Len(t, snap, 1)
	assert.Equal(t, prior.ID(), snap[0].ID)
	assert.Equal(t, runs.StatusSuccess, snap[0].Status)
	assert.Empty(t, snap[0].Output, "list-shaped snapshots must not carry output")

	// 3. Deltas, in lifecycle order, driven through the tracker.
	run := tr.New("live-hook")
	st := nextRunEvent(t, events, "creation delta")
	assert.Equal(t, run.ID(), st.ID)
	assert.Equal(t, runs.StatusPending, st.Status)

	seq := run.SetWaitingOn(runs.WaitingOn{Kind: runs.WaitingOnWait, Reason: "declared sleep"})
	st = nextRunEvent(t, events, "waiting delta")
	require.NotNil(t, st.WaitingOn)
	assert.Equal(t, "declared sleep", st.WaitingOn.Reason)

	run.ClearWaitingOn(seq)
	st = nextRunEvent(t, events, "wait-cleared delta")
	assert.Nil(t, st.WaitingOn)

	run.SetRunning()
	st = nextRunEvent(t, events, "running delta")
	assert.Equal(t, runs.StatusRunning, st.Status)
	assert.False(t, st.StartedAt.IsZero())

	run.SetTitle("owner/repo#7")
	st = nextRunEvent(t, events, "title delta")
	assert.Equal(t, "owner/repo#7", st.Title)

	run.RequestCancel()
	st = nextRunEvent(t, events, "cancel-requested delta")
	assert.True(t, st.CancelRequested)

	run.Finish(runs.StatusCancelled, -1, "cancelled")
	st = nextRunEvent(t, events, "terminal delta")
	assert.Equal(t, runs.StatusCancelled, st.Status)
	assert.Equal(t, -1, st.ExitCode)
	assert.Equal(t, "cancelled", st.Error)
	assert.False(t, st.Finished.IsZero())

	// 4. Heartbeats keep arriving on an idle stream.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "stream ended before a heartbeat")
			if ev.name == "hb" {
				return // saw one — done
			}
		case <-deadline:
			t.Fatal("no heartbeat within 3s at a 150ms cadence")
		}
	}
}

// The snapshot and /runs must be the same marshaling of the same read path.
func TestRunsStreamSnapshotMatchesRunsEndpoint(t *testing.T) {
	s, _, tr, _ := newTestServer(t)
	a := tr.New("h")
	a.SetRunning()
	b := tr.New("h")
	b.Finish(runs.StatusFailure, 2, "boom")

	srv := httptest.NewServer(admin(s))
	defer srv.Close()

	var fromRuns []runs.RunState
	resp, err := http.Get(srv.URL + "/runs")
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&fromRuns))
	resp.Body.Close()

	events, sresp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer sresp.Body.Close()
	nextEvent(t, events, "retry")
	ev := nextEvent(t, events, "snapshot")
	var fromStream []runs.RunState
	require.NoError(t, json.Unmarshal([]byte(ev.data), &fromStream))

	rb, err := json.Marshal(fromRuns)
	require.NoError(t, err)
	sb, err := json.Marshal(fromStream)
	require.NoError(t, err)
	assert.JSONEq(t, string(rb), string(sb))
}

// A slow client (full buffer) is dropped without ever blocking the
// publishing (runner-side) goroutine.
func TestRunsStreamSlowClientDropped(t *testing.T) {
	hub := newStreamHub()
	sub := hub.subscribe()
	require.Equal(t, 1, hub.clients())

	done := make(chan struct{})
	go func() {
		defer close(done)
		// One more publish than the buffer holds: the last one must drop
		// the subscriber instead of blocking.
		for i := 0; i < streamClientBuffer+1; i++ {
			hub.publish(runs.RunState{ID: "r", HookID: "h"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow client")
	}
	assert.Equal(t, 0, hub.clients(), "slow client must be dropped")
	// Its channel is closed: a reader sees termination, not a hang.
	drained := 0
	for range sub.ch {
		drained++
	}
	assert.Equal(t, streamClientBuffer, drained)

	// And the tracker path stays non-blocking end to end: a Finish with a
	// wedged subscriber must return promptly.
	tr := runs.NewTracker()
	tr.SetOnChange(hub.publish)
	stuck := hub.subscribe()
	_ = stuck // never read — fills and gets dropped
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < streamClientBuffer+8; i++ {
			r := tr.New("h")
			r.Finish(runs.StatusSuccess, 0, "")
		}
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("run lifecycle blocked on a dead stream subscriber")
	}
}

// CloseStreams ends open handlers (the graceful-shutdown path).
func TestRunsStreamCloseStreamsEndsHandlers(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	srv := httptest.NewServer(admin(s))
	defer srv.Close()

	events, resp, cancel := openStream(t, srv.URL)
	defer cancel()
	defer resp.Body.Close()
	nextEvent(t, events, "retry")
	nextEvent(t, events, "snapshot")

	s.CloseStreams()
	select {
	case _, ok := <-events:
		if ok {
			// A buffered event may still arrive; the channel must close
			// right behind it.
			for range events {
				// drain
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end after CloseStreams")
	}
	// New subscriptions after close die immediately (no handler leak at
	// shutdown).
	sub := s.stream.subscribe()
	_, ok := <-sub.ch
	assert.False(t, ok)
}
