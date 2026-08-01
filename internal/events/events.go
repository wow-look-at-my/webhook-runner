// Package events keeps a bounded in-memory feed of notable server
// activity — reload webhooks from GitHub, git pulls, hook reloads and
// load errors, image builds, run lifecycle — for the admin dashboard.
// Same persistence model as run history: memory only, newest wins.
package events

import (
	"strings"
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

	// onRecord, when set, is invoked with each recorded event,
	// synchronously on the recording goroutine (under the ring mutex, so
	// keep it trivial). It is the server's "activity happened" seam — the
	// stream hub turns it into dashboard section-changed signals, and the
	// attention aggregator derives entries from recognized kinds. It must
	// be fast, must never block, and must never call back into the
	// Recorder. Set once at wiring time, before concurrent Records (the
	// SetOnChange convention).
	onRecord func(ev Event)
}

// NewRecorder returns a recorder retaining the most recent max events.
func NewRecorder(max int) *Recorder {
	if max <= 0 {
		max = 500
	}
	return &Recorder{buf: make([]Event, max)}
}

// SetOnRecord registers fn to be invoked after every recorded event with
// that event. Nil-receiver safe like Record; a nil fn disables the
// callback. See the field comment for the contract.
func (r *Recorder) SetOnRecord(fn func(ev Event)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onRecord = fn
	r.mu.Unlock()
}

// Record appends an event to the feed.
func (r *Recorder) Record(kind, msg string, fields map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := Event{Time: time.Now().UTC(), Kind: kind, Msg: msg, Fields: fields}
	r.buf[r.next] = ev
	r.next = (r.next + 1) % len(r.buf)
	r.total++
	if r.onRecord != nil {
		r.onRecord(ev)
	}
}

// Family is the first dot-separated segment of an event kind — the family
// every kind belongs to ("run" for run.started/run.finished/run.skipped,
// "image" for image.built/image.build_failed). Kinds without a dot are
// their own family.
func Family(kind string) string {
	if i := strings.IndexByte(kind, '.'); i >= 0 {
		return kind[:i]
	}
	return kind
}

// Filter narrows a listing. The zero Filter matches everything.
type Filter struct {
	// Hook, when set, keeps only events whose "hook" field names it — the
	// convention every hook-scoped recorder call already follows.
	// Server-wide activity (reloads, git pulls) carries no hook field and
	// so never matches a hook-scoped listing.
	Hook string

	// ExcludeFamilies drops every event whose kind family (see Family) is
	// listed. The dashboard excludes "run" from both activity feeds: run
	// lifecycle already has a richer home in the runs table, where each run
	// is one row with status, timings, and its output.
	ExcludeFamilies []string
}

func (f Filter) match(ev Event) bool {
	if f.Hook != "" && ev.Fields["hook"] != f.Hook {
		return false
	}
	if len(f.ExcludeFamilies) > 0 {
		fam := Family(ev.Kind)
		for _, ex := range f.ExcludeFamilies {
			if fam == ex {
				return false
			}
		}
	}
	return true
}

// ListFiltered returns up to max events matching f, newest first. max <= 0
// returns every retained match.
//
// FILTERING HAPPENS BEFORE THE CAP, and that ordering is the whole point:
// max bounds the events RETURNED, never the events EXAMINED. Capping first
// and filtering the page after is the bug that makes a busy hook's feed go
// blank — a burst of excluded events fills the page, the filter empties it,
// and the surface reports "nothing here" while the ring still holds plenty
// of matches just behind them. Any future listing walks the whole ring the
// same way.
func (r *Recorder) ListFiltered(f Filter, max int) []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.total
	if n > len(r.buf) {
		n = len(r.buf)
	}
	out := make([]Event, 0, min(n, capHint(max, n)))
	for i := 1; i <= n; i++ {
		if max > 0 && len(out) == max {
			break
		}
		ev := r.buf[(r.next-i+len(r.buf))%len(r.buf)]
		if f.match(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// capHint sizes the result slice: the cap when one is set, else everything
// retained.
func capHint(max, retained int) int {
	if max > 0 {
		return max
	}
	return retained
}

// List returns up to max events, newest first. max <= 0 returns all
// retained events.
func (r *Recorder) List(max int) []Event {
	return r.ListFiltered(Filter{}, max)
}

// ListByHook returns up to max events whose "hook" field names the given
// hook, newest first. max <= 0 returns all retained matches.
func (r *Recorder) ListByHook(hookID string, max int) []Event {
	return r.ListFiltered(Filter{Hook: hookID}, max)
}
