package server

// GET /runs/stream (admin port): the Server-Sent Events live tail of run
// lifecycle changes — what lets the dashboard stop polling /runs.
//
// Contract, in order, per connection:
//
//	retry: 2000                      (fixed client reconnect delay — never grows)
//	event: snapshot                  (current live+recent runs, same shape as
//	data: [ ...RunState... ]          /runs — output stripped, waiters attached)
//	event: run                       (one per lifecycle change: created,
//	data: { ...RunState... }          pending→running, waiting set/cleared,
//	                                  title set, cancel requested, terminal)
//	: hb                             (comment heartbeat every ~10s, PLUS an
//	event: hb                         `hb` event in the same write — comments
//	data: 1                           keep proxies/idle detection honest, but
//	                                  EventSource never surfaces them to JS,
//	                                  so the client's freshness signal is the
//	                                  event; both ride one flush)
//
// Fan-out MUST NEVER block the runner: publishes happen synchronously on
// runner/state-API goroutines (the tracker's OnChange seam), so each
// subscriber gets a bounded buffered channel and a publish does only a
// non-blocking send. A subscriber whose buffer is full is DROPPED on the
// spot — its channel closed, its handler returning, its connection dying —
// because the client's EventSource will reconnect (retry: 2000) and the
// fresh connect snapshot resyncs it. Drop-and-resync IS the slow-client
// semantics, not an error; it bounds memory and never applies backpressure
// to run execution.
//
// The subscribe-BEFORE-snapshot order in the handler means no lifecycle
// change can fall between the snapshot read and the delta stream: anything
// landing in that window is buffered in the channel and delivered right
// after the snapshot (per-run duplicates are fine — clients merge by run
// id, and buffered deltas are never older than the snapshot's own row for
// long: every later mutation is buffered behind them in order).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

const (
	// streamClientBuffer is each subscriber's channel capacity. A client
	// this far behind (256 lifecycle changes) is not keeping up; it gets
	// dropped and resyncs on reconnect.
	streamClientBuffer = 256
	// streamSnapshotMax caps the connect snapshot (same merged live+history
	// read as /runs) — enough for the dashboard's initial window without
	// shipping the whole retention on every reconnect.
	streamSnapshotMax = 200
)

// streamHeartbeat is the heartbeat cadence. A var so tests can shrink it;
// ~10s keeps intermediaries from idling the connection out and gives
// clients a liveness signal to key staleness off.
var streamHeartbeat = 10 * time.Second

// streamHub fans runs.RunState updates out to the connected SSE clients.
type streamHub struct {
	mu     sync.Mutex
	subs   map[*streamSub]struct{}
	closed bool
}

type streamSub struct {
	ch chan runs.RunState
}

func newStreamHub() *streamHub {
	return &streamHub{subs: map[*streamSub]struct{}{}}
}

// subscribe registers a new client. On a hub that has been closed (server
// shutdown) the returned subscription is already closed, so the handler
// exits immediately instead of racing the shutdown.
func (h *streamHub) subscribe() *streamSub {
	sub := &streamSub{ch: make(chan runs.RunState, streamClientBuffer)}
	h.mu.Lock()
	if h.closed {
		close(sub.ch)
	} else {
		h.subs[sub] = struct{}{}
	}
	h.mu.Unlock()
	return sub
}

// unsubscribe removes a client (idempotent; safe after a drop).
func (h *streamHub) unsubscribe(sub *streamSub) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

// publish delivers st to every subscriber without ever blocking: a full
// buffer drops that subscriber (close wakes its handler). All sends and
// closes happen under h.mu, so a send can never race a close.
func (h *streamHub) publish(st runs.RunState) {
	h.mu.Lock()
	for sub := range h.subs {
		select {
		case sub.ch <- st:
		default:
			delete(h.subs, sub)
			close(sub.ch)
		}
	}
	h.mu.Unlock()
}

// closeAll disconnects every subscriber and marks the hub closed (used at
// server shutdown so long-lived stream responses end and Shutdown's
// handler-drain can finish).
func (h *streamHub) closeAll() {
	h.mu.Lock()
	h.closed = true
	for sub := range h.subs {
		delete(h.subs, sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

// clients reports the current subscriber count (tests).
func (h *streamHub) clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// CloseStreams disconnects every /runs/stream client. Call it at shutdown
// BEFORE http.Server.Shutdown: Shutdown waits for in-flight handlers, and a
// stream handler only returns when its client disconnects, its subscription
// closes, or its request context ends.
func (s *Server) CloseStreams() { s.stream.closeAll() }

// handleRunsStream is GET /runs/stream (admin port).
func (s *Server) handleRunsStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Fixed reconnect delay for the client's EventSource. Never grows — the
	// standing rule: no growing backoffs, no give-up counters.
	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}

	// Subscribe FIRST, snapshot second — see the package comment: changes
	// landing while the snapshot is serialized are buffered and delivered
	// right after it, so nothing falls in the gap.
	sub := s.stream.subscribe()
	defer s.stream.unsubscribe(sub)

	if err := writeSSEEvent(w, "snapshot", s.mergedRuns("", time.Time{}, streamSnapshotMax)); err != nil {
		return
	}
	if rc.Flush() != nil {
		return // no streaming support (or a dead client): nothing to tail
	}

	hb := time.NewTicker(streamHeartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case st, ok := <-sub.ch:
			if !ok {
				// Dropped (slow client) or server shutdown: end the
				// response; the client reconnects and resyncs.
				return
			}
			if err := writeSSEEvent(w, "run", st); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-hb.C:
			// Comment for proxies + event for the client, one write.
			if _, err := io.WriteString(w, ": hb\nevent: hb\ndata: 1\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

// writeSSEEvent writes one SSE event with a JSON payload. Compact JSON has
// no raw newlines, so a single data: line is always well-formed.
func writeSSEEvent(w io.Writer, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
