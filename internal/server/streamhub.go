// GET /runs/stream (admin port): the Server-Sent Events live tail of run
// lifecycle changes — what lets the dashboard stop polling /runs.
//
// Contract, in order, per connection:
//
//	retry: (fixed client reconnect delay — never grows)
//	event: snapshot (current live+recent runs, same shape as
//	data: [ ...RunState... ] /runs — output stripped, waiters attached)
//	event: run ( per lifecycle change: created,
//	data: { ...RunState... } pending→running, waiting set/cleared,
//
// title set, cancel requested, terminal)
//
//	event: changed (coarse section-invalidation signal:
//	data: {"sections":["kv", ...]} the named admin sections changed since
//
// the client last heard — refetch each
// ; carries no payload by design)
//
//	: hb (comment heartbeat every ~s, PLUS an
//	event: hb `hb` event in the same write — comments
//	data: {"active":["<run-id>",…]} keep proxies/idle detection honest, but
//
// EventSource never surfaces them to JS,
// so the client's freshness signal is the
// event; both ride flush. The payload
// is the CURRENT non-terminal run-id set —
// the live truth clients diff their local
// state against every beat (drop what the
// server no longer knows, fetch what they
// never saw), so a missed delta can cost
// at most ~ heartbeat of fiction.
// Purely additive: pre-payload clients
// read hb as bare liveness and ignore it)
//
// Fan-out MUST NEVER block the runner: publishes happen synchronously on
// runner/state-API goroutines (the tracker's OnChange seam), so each
// subscriber gets a bounded buffered channel and a publish does only a
// non-blocking send. A subscriber whose buffer is full is DROPPED on the
// spot — its channel closed, its handler returning, its connection dying —
// because the client's EventSource will reconnect (retry: ) and the
// fresh connect snapshot resyncs it. Drop-and-resync IS the slow-client
// semantics, not an error; it bounds memory and never applies backpressure
// to run execution.
//
// Section signals ride the SAME connection but a DIFFERENT mechanism: a
// per-subscriber dirty SET plus a -slot wake channel, not the delta
// queue. A signal storm coalesces into pending drain (the set is
// bounded by the handful of section names), so signals can never overflow
// a subscriber, never drop , and never block a publisher — only run
// deltas can drop a slow client. The payload is deliberately just the
// section names ("changed → refetch "): the client refetches the
// section endpoint it already knows, so a dropped client that reconnects
// simply refetches every section on open (its snapshot rule) and no
// signal is ever load-bearing state.
//
// The subscribe-BEFORE-snapshot order in the handler means no lifecycle
// change can fall between the snapshot read and the delta stream: anything
// landing in that window is buffered in the channel and delivered right
// after the snapshot (per-run duplicates are fine — clients merge by run
// id, and buffered deltas are never older than the snapshot's own row for
// long: every later mutation is buffered behind them in order).
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

const (
	// streamClientBuffer is each subscriber's channel capacity.
	streamClientBuffer = 256
	// streamSnapshotMax caps the connect snapshot (same merged live+history read as /runs) — enough for the dashboard's initial window.
	streamSnapshotMax = 200
)

// streamHeartbeat is the heartbeat cadence.
var streamHeartbeat = 10 * time.Second

// streamHub fans runs.RunState updates out to the connected SSE clients.
type streamHub struct {
	mu     sync.Mutex
	subs   set.Set[*streamSub]
	closed bool
}

type streamSub struct {
	ch chan runs.RunState

	// Section-signal state: dirty is the set of section names signaled since the handler last drained; kick (-buffered) wakes the handler.
	mu    sync.Mutex
	dirty set.Set[string]
	kick  chan struct{}
}

// drainSections atomically takes the dirty set, returning its contents
// sorted (empty when a wake raced an earlier drain).
func (sub *streamSub) drainSections() []string {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.dirty.IsEmpty() {
		return nil
	}
	out := sub.dirty.Values()
	sub.dirty.Clear()
	sort.Strings(out)
	return out
}

func newStreamHub() *streamHub {
	return &streamHub{subs: set.New[*streamSub]()}
}

// subscribe registers a new client. On a hub that has been closed (server
// shutdown) the returned subscription is already closed, so the handler
// exits immediately instead of racing the shutdown.
func (h *streamHub) subscribe() *streamSub {
	sub := &streamSub{
		ch:    make(chan runs.RunState, streamClientBuffer),
		dirty: set.New[string](),
		kick:  make(chan struct{}, 1),
	}
	h.mu.Lock()
	if h.closed {
		close(sub.ch)
	} else {
		h.subs.Add(sub)
	}
	h.mu.Unlock()
	return sub
}

// unsubscribe removes a client (idempotent; safe after a drop).
func (h *streamHub) unsubscribe(sub *streamSub) {
	h.mu.Lock()
	if h.subs.Contains(sub) {
		h.subs.Remove(sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

// publish delivers st to every subscriber without ever blocking: a full buffer drops that subscriber (close wakes its handler).
func (h *streamHub) publish(st runs.RunState) {
	h.mu.Lock()
	for sub := range h.subs.All() {
		select {
		case sub.ch <- st:
		default:
			h.subs.Remove(sub)
			close(sub.ch)
		}
	}
	h.mu.Unlock()
}

// signal marks the named dashboard sections dirty on every subscriber and wakes their handlers.
func (h *streamHub) signal(sections ...string) {
	if len(sections) == 0 {
		return
	}
	h.mu.Lock()
	for sub := range h.subs.All() {
		sub.mu.Lock()
		for _, name := range sections {
			sub.dirty.Add(name)
		}
		sub.mu.Unlock()
		select {
		case sub.kick <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

// sectionsForEvent maps a recorded activity-event kind to the dashboard
// sections whose payloads that event implies changed. Every event dirties
// "events" (it IS the activity feed); the extras cover the sections whose
// state mutates alongside specific kinds. Over-signaling is harmless (the
// client refetches small endpoint ); under-signaling is the bug
// class this map must avoid — prefer prefixes where every current and
// plausible future kind in the family affects the section.
func sectionsForEvent(kind string) []string {
	out := []string{"events"}
	switch {
	case kind == "hooks.reloaded":
		// A reload can change the hook roster, every content-hash image state, the declared concurrency groups, and the reload panel's live-commit.
		out = append(out, "hooks", "managers", "images", "concurrency", "reload", "attention")
	case kind == "hook.disabled", kind == "hook.enabled":
		// A kill-switch flip changes the roster's disabled flags AND which hook-scoped attention entries the read-time filter hides.
		out = append(out, "hooks", "attention")
	case kind == "manager.disabled", kind == "manager.enabled":
		// The manager kill switch: same roster+attention consequences, manager panel instead of the hooks table.
		out = append(out, "managers", "attention")
	case strings.HasPrefix(kind, "manager."):
		// Manager lifecycle (started/exited/leased/wait/skipped/ restart_requested) moves the Managers panel.
		out = append(out, "managers")
	case kind == "hook.load_error":
		out = append(out, "hooks", "managers")
	case strings.HasPrefix(kind, "image."):
		out = append(out, "images")
	case strings.HasPrefix(kind, "concurrency."):
		out = append(out, "concurrency")
	case strings.HasPrefix(kind, "reload."), strings.HasPrefix(kind, "git."), kind == "github.push":
		// Every reload-gate verdict, git pull, and hooks-repo push moves what the reload panel shows (live/pending commit, hold state).
		out = append(out, "reload")
	}
	return out
}

// closeAll disconnects every subscriber and marks the hub closed (used at server shutdown so long-lived stream responses end and Shutdown's.
func (h *streamHub) closeAll() {
	h.mu.Lock()
	h.closed = true
	for sub := range h.subs.All() {
		h.subs.Remove(sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

// clients reports the current subscriber count (tests).
func (h *streamHub) clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subs.Len()
}

// CloseStreams disconnects every /runs/stream client.
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

	// Subscribe , snapshot — see the package comment: changes landing while the snapshot is serialized are buffered and delivered.
	sub := s.stream.subscribe()
	defer s.stream.unsubscribe(sub)

	if err := writeSSEEvent(w, "snapshot", s.mergedRuns("", time.Time{}, streamSnapshotMax, nil)); err != nil {
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
				// Dropped (slow client) or server shutdown: end the response; the client reconnects and resyncs.
				return
			}
			if err := writeSSEEvent(w, "run", st); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-sub.kick:
			// Section signals: drain the coalesced dirty set into changed event. An empty drain (a wake that raced an earlier drain) writes nothing.
			secs := sub.drainSections()
			if len(secs) == 0 {
				continue
			}
			if err := writeSSEEvent(w, "changed", map[string][]string{"sections": secs}); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-hb.C:
			// Comment for proxies + event for the client, write.
			if err := writeSSEHeartbeat(w, s.tracker.ActiveIDs()); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

// writeSSEEvent writes SSE event with a JSON payload. Compact JSON has
// no raw newlines, so a single data: line is always well-formed.
func writeSSEEvent(w io.Writer, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

// writeSSEHeartbeat writes the combined proxy-comment + hb event in write (both ride flush).
func writeSSEHeartbeat(w io.Writer, active []string) error {
	b, err := json.Marshal(map[string][]string{"active": active})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, ": hb\nevent: hb\ndata: %s\n\n", b)
	return err
}
