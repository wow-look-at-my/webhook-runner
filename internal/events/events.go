// Package events keeps a bounded in-memory feed of notable server
// activity — reload webhooks from GitHub, git pulls, hook reloads and
// load errors, image builds, run lifecycle — for the admin dashboard.
// Same persistence model as run history: memory only, newest wins.
package events

import (
	"sync"
	"time"
)

// Event is one entry in the activity feed.
type Event struct {
	Time   time.Time         `json:"time"`
	Kind   string            `json:"kind"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Recorder is a concurrency-safe bounded ring of events. A nil *Recorder
// is valid and drops everything, so callers never need to nil-check.
type Recorder struct {
	mu    sync.Mutex
	buf   []Event
	next  int
	total int
}

// NewRecorder returns a recorder retaining the most recent max events.
func NewRecorder(max int) *Recorder {
	if max <= 0 {
		max = 500
	}
	return &Recorder{buf: make([]Event, max)}
}

// Record appends an event to the feed.
func (r *Recorder) Record(kind, msg string, fields map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = Event{Time: time.Now().UTC(), Kind: kind, Msg: msg, Fields: fields}
	r.next = (r.next + 1) % len(r.buf)
	r.total++
}

// List returns up to max events, newest first. max <= 0 returns all
// retained events.
func (r *Recorder) List(max int) []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.total
	if n > len(r.buf) {
		n = len(r.buf)
	}
	if max > 0 && n > max {
		n = max
	}
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		idx := (r.next - i + len(r.buf)) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// ListByHook returns up to max events whose "hook" field names the given
// hook, newest first — the convention every hook-scoped recorder call
// already follows. Events without that field (server-wide activity like
// reloads and git pulls) never match. max <= 0 returns all retained matches.
func (r *Recorder) ListByHook(hookID string, max int) []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.total
	if n > len(r.buf) {
		n = len(r.buf)
	}
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		if max > 0 && len(out) == max {
			break
		}
		ev := r.buf[(r.next-i+len(r.buf))%len(r.buf)]
		if ev.Fields["hook"] == hookID {
			out = append(out, ev)
		}
	}
	return out
}
