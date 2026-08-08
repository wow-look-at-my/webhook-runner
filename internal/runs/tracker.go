package runs

// The Tracker: the concurrency-safe registry of live and recent runs. Run
// (the per-run state machine) lives in runs.go; this file owns the maps,
// the bounded per-hook retention trim (terminal runs only — see New), and
// the read-side listings the server builds /runs views from.

import (
	"crypto/rand"
	"encoding/base32"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tracker is a concurrency-safe registry of runs.
type Tracker struct {
	mu        sync.RWMutex
	byID      map[string]*Run
	byHook    map[string][]*Run
	maxByHook int
	onFinish  func(RunState)
	onChange  func(RunState)
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{
		byID:      make(map[string]*Run),
		byHook:    make(map[string][]*Run),
		maxByHook: MaxRunsPerHook,
	}
}

// SetOnFinish registers fn to be invoked exactly once per run — with a full
// terminal snapshot, synchronously on the finishing goroutine — when the run
// reaches a terminal status. Set it before the first New: runs created
// earlier never see it. This is the persistence seam (the run store's
// write-once-at-terminal hook) without the runs package knowing about disk.
func (t *Tracker) SetOnFinish(fn func(RunState)) {
	t.mu.Lock()
	t.onFinish = fn
	t.mu.Unlock()
}

// SetOnChange registers fn to be invoked after every observable lifecycle
// mutation of runs created AFTER the call — creation, pending→running,
// title set, waiting_on set/cleared, cancel requested, and the terminal
// transition (after OnFinish) — each time with an output-stripped snapshot,
// synchronously on the mutating goroutine. This is the live-stream seam
// (the /runs/stream fan-out) without the runs package knowing about HTTP;
// like OnFinish, set it before the first New. fn must be fast and must
// never block: it runs on runner/state-API goroutines (the server's stream
// hub only does a non-blocking channel send). nil disables notifications
// (the events.Recorder nil-safety convention).
func (t *Tracker) SetOnChange(fn func(RunState)) {
	t.mu.Lock()
	t.onChange = fn
	t.mu.Unlock()
}

// New starts tracking a fresh run for the given hook ID. The run begins
// in StatusPending; call SetRunning when the container actually starts.
func (t *Tracker) New(hookID string) *Run {
	r := &Run{
		state: RunState{
			ID:      newID(),
			HookID:  hookID,
			Started: time.Now().UTC(),
			Status:  StatusPending,
		},
		done:   make(chan struct{}),
		cancel: make(chan struct{}),
	}
	t.mu.Lock()
	r.onFinish = t.onFinish
	r.onChange = t.onChange
	t.byID[r.state.ID] = r
	t.byHook[hookID] = append(t.byHook[hookID], r)
	// Trim past maxByHook — oldest TERMINAL runs only, NEVER an active one.
	// A non-terminal run is the server's current truth: evicting it made
	// GET /runs/{id} 404 for a genuinely-running run during floods (the
	// runstore fallback holds terminal snapshots only), cut live runs out
	// of /runs windows, and degraded /concurrency's holder rows. The
	// per-hook list may therefore exceed maxByHook while more than
	// maxByHook runs are truly active — bounded by real concurrent work,
	// and later News keep trimming as those runs finish. (Status() locks
	// the run under t.mu: tracker-then-run is the package's lock order,
	// same as HasActive.)
	if extra := len(t.byHook[hookID]) - t.maxByHook; extra > 0 {
		kept := t.byHook[hookID][:0]
		for _, old := range t.byHook[hookID] {
			if extra > 0 && old.Status().Terminal() {
				delete(t.byID, old.state.ID)
				extra--
				continue
			}
			kept = append(kept, old)
		}
		t.byHook[hookID] = kept
	}
	t.mu.Unlock()
	// The creation notification: a fresh pending run is a lifecycle event
	// too (the dashboard shows queued runs the moment they are accepted).
	r.notifyChange()
	return r
}

// HasActive reports whether the hook currently has a run that has not reached
// a terminal status (pending or running). The scheduler uses it for
// skip-if-already-running overlap protection so a sweep that outlasts its
// interval cannot stack on itself. Lock ordering is tracker-then-run
// (consistent with the rest of the package), so this can't deadlock.
func (t *Tracker) HasActive(hookID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, r := range t.byHook[hookID] {
		if !r.Status().Terminal() {
			return true
		}
	}
	return false
}

// Get returns the run with the given ID, or nil if absent or evicted.
func (t *Tracker) Get(id string) *Run {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byID[id]
}

// ListByHook returns the retained runs for a hook, newest-first.
func (t *Tracker) ListByHook(hookID string, max int) []*Run {
	t.mu.RLock()
	defer t.mu.RUnlock()
	src := t.byHook[hookID]
	out := make([]*Run, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].state.Started.After(out[j].state.Started)
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// ListAll returns all retained runs across hooks, newest-first.
func (t *Tracker) ListAll(max int) []*Run {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*Run, 0, len(t.byID))
	for _, r := range t.byID {
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].state.Started.After(out[j].state.Started)
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// ActiveIDs returns the ID of every tracked run that has not reached a
// terminal status, sorted. This is the live-set truth the stream's hb
// heartbeat carries so clients can reconcile their view against what is
// actually active RIGHT NOW (drop runs the server no longer knows, fetch
// runs they never saw). Always non-nil — an empty active set must
// serialize as [] (a real "nothing is active" verdict), never null.
func (t *Tracker) ActiveIDs() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := make([]string, 0, len(t.byID))
	for _, r := range t.byID {
		if !r.Status().Terminal() {
			ids = append(ids, r.state.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// newID returns 16 random bytes encoded as lowercase base32 without
// padding (26 ASCII characters), giving 128 bits of entropy.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never returns an error in practice; fall back to
		// a timestamp-based ID rather than panic in this hot path.
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t >> (i % 8 * 8))
		}
	}
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b[:]), "="))
}
