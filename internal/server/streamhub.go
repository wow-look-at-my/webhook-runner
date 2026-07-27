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
//	event: changed                   (coarse section-invalidation signal:
//	data: {"sections":["kv", ...]}    the named admin sections changed since
//	                                  the client last heard — refetch each
//	                                  ONCE; carries no payload by design)
//	: hb                             (comment heartbeat every ~10s, PLUS an
//	event: hb                         `hb` event in the same write — comments
//	data: {"active":["<run-id>",…]}   keep proxies/idle detection honest, but
//	                                  EventSource never surfaces them to JS,
//	                                  so the client's freshness signal is the
//	                                  event; both ride one flush. The payload
//	                                  is the CURRENT non-terminal run-id set —
//	                                  the live truth clients diff their local
//	                                  state against every beat (drop what the
//	                                  server no longer knows, fetch what they
//	                                  never saw), so a missed delta can cost
//	                                  at most ~one heartbeat of fiction.
//	                                  Purely additive: pre-payload clients
//	                                  read hb as bare liveness and ignore it)
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
// Section signals ride the SAME connection but a DIFFERENT mechanism: a
// per-subscriber dirty SET plus a 1-slot wake channel, not the delta
// queue. A signal storm coalesces into one pending drain (the set is
// bounded by the handful of section names), so signals can never overflow
// a subscriber, never drop one, and never block a publisher — only run
// deltas can drop a slow client. The payload is deliberately just the
// section names ("changed → refetch once"): the client refetches the
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

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
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

	// Section-signal state: dirty is the set of section names signaled
	// since the handler last drained; kick (1-buffered) wakes the handler.
	// A set + non-blocking wake coalesces bursts and is drop-proof by
	// construction — see the package comment.
	mu    sync.Mutex
	dirty map[string]struct{}
	kick  chan struct{}
}

// drainSections atomically takes the dirty set, returning its contents
// sorted (empty when a wake raced an earlier drain).
func (sub *streamSub) drainSections() []string {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.dirty) == 0 {
		return nil
	}
	out := make([]string, 0, len(sub.dirty))
	for s := range sub.dirty {
		out = append(out, s)
	}
	clear(sub.dirty)
	sort.Strings(out)
	return out
}

func newStreamHub() *streamHub {
	return &streamHub{subs: map[*streamSub]struct{}{}}
}

// subscribe registers a new client. On a hub that has been closed (server
// shutdown) the returned subscription is already closed, so the handler
// exits immediately instead of racing the shutdown.
func (h *streamHub) subscribe() *streamSub {
	sub := &streamSub{
		ch:    make(chan runs.RunState, streamClientBuffer),
		dirty: map[string]struct{}{},
		kick:  make(chan struct{}, 1),
	}
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

// signal marks the named dashboard sections dirty on every subscriber and
// wakes their handlers. Never blocks and never drops anyone: the dirty set
// is bounded by the section-name universe and the wake send is
// non-blocking (a full kick just means a drain is already pending, which
// will pick these sections up too).
func (h *streamHub) signal(sections ...string) {
	if len(sections) == 0 {
		return
	}
	h.mu.Lock()
	for sub := range h.subs {
		sub.mu.Lock()
		for _, name := range sections {
			sub.dirty[name] = struct{}{}
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
// client refetches one small endpoint once); under-signaling is the bug
// class this map must avoid — prefer prefixes where every current and
// plausible future kind in the family affects the section.
func sectionsForEvent(kind string) []string {
	out := []string{"events"}
	switch {
	case kind == "hooks.reloaded":
		// A reload can change the hook roster, every content-hash image
		// state, the declared concurrency groups, and the reload panel's
		// live-commit view at once — and, via a changed hook.json `enable`
		// default, the effective disabled states GET /attention filters on.
		out = append(out, "hooks", "managers", "images", "concurrency", "reload", "attention")
	case kind == "hook.disabled", kind == "hook.enabled":
		// A kill-switch flip changes the roster's disabled flags AND which
		// hook-scoped attention entries the read-time filter hides.
		out = append(out, "hooks", "attention")
	case kind == "manager.disabled", kind == "manager.enabled":
		// The manager kill switch: same roster+attention consequences,
		// manager panel instead of the hooks table.
		out = append(out, "managers", "attention")
	case strings.HasPrefix(kind, "manager."):
		// Manager lifecycle (started/exited/leased/inbox_dropped/wait/
		// skipped/restart_requested) moves the Managers panel.
		out = append(out, "managers")
	case kind == "hook.load_error":
		out = append(out, "hooks", "managers")
	case strings.HasPrefix(kind, "image."):
		out = append(out, "images")
	case strings.HasPrefix(kind, "concurrency."):
		out = append(out, "concurrency")
	case strings.HasPrefix(kind, "reload."), strings.HasPrefix(kind, "git."), kind == "github.push":
		// Every reload-gate verdict, git pull, and hooks-repo push moves
		// what the reload panel shows (live/pending commit, hold state).
		out = append(out, "reload")
	}
	return out
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
		case <-sub.kick:
			// Section signals: drain the coalesced dirty set into ONE
			// changed event. An empty drain (a wake that raced an earlier
			// drain) writes nothing.
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
			// Comment for proxies + event for the client, one write. The hb
			// payload carries the ACTIVE (non-terminal) run-id set — the
			// reconcile beat: reading the tracker here is cheap (ids only,
			// ~26 bytes each, bounded by genuinely concurrent work), and an
			// EMPTY set still serializes as {"active":[]} — a real "nothing
			// is active" verdict clients must act on, never null.
			if err := writeSSEHeartbeat(w, s.tracker.ActiveIDs()); err != nil {
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

// writeSSEHeartbeat writes the combined proxy-comment + hb event in ONE
// write (both ride one flush). The event payload is the active run-id set;
// active is never nil (Tracker.ActiveIDs guarantees []), so the data line
// is always {"active":[...]}.
func writeSSEHeartbeat(w io.Writer, active []string) error {
	b, err := json.Marshal(map[string][]string{"active": active})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, ": hb\nevent: hb\ndata: %s\n\n", b)
	return err
}
