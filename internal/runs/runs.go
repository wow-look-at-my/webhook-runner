// Package runs tracks the in-memory state of webhook executions.
//
// A Run is created when a hook starts and updated as the underlying
// container produces output and finally exits. The tracker keeps a
// bounded ring of finished runs per hook so the dashboard and the
// /runs/{id} endpoint can show recent history without unbounded memory
// growth.
package runs

import (
	"crypto/rand"
	"encoding/base32"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the lifecycle stage of a run.
type Status string

const (
	StatusPending Status = "pending" // created, container not yet started
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
	StatusTimeout Status = "timeout"
	StatusError   Status = "error" // failed to start, never produced an exit code
)

// MaxOutputLines is the most recent stdout/stderr lines retained per run.
// Older lines are dropped as new ones arrive.
const MaxOutputLines = 500

// MaxRunsPerHook is the most recent finished runs retained per hook ID.
const MaxRunsPerHook = 50

// RunState is the value-type, mutex-free, JSON-marshalable view of a run.
// Use Run.Snapshot to obtain a stable copy; Run owns the canonical state
// behind a mutex and never exposes RunState by reference.
type RunState struct {
	ID       string    `json:"id"`
	HookID   string    `json:"hook_id"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Status   Status    `json:"status"`
	ExitCode int       `json:"exit_code"`

	// Output is the most recent MaxOutputLines lines of merged
	// stdout+stderr. The slice is newline-free.
	Output []string `json:"output,omitempty"`

	// Error is set when the run failed before or outside the container
	// (for example, "docker: command not found"). When the container
	// itself ran and exited non-zero, Error stays empty and the failure
	// is reflected by ExitCode and Status.
	Error string `json:"error,omitempty"`
}

// Run is the mutex-protected wrapper around a RunState. Always pass *Run
// (never Run by value) — copying the struct would copy its mutex.
type Run struct {
	mu    sync.Mutex
	state RunState
	done  chan struct{}
}

// ID returns the run's stable ID.
func (r *Run) ID() string { return r.state.ID }

// HookID returns the hook ID this run belongs to.
func (r *Run) HookID() string { return r.state.HookID }

// Started returns the time the run was created (immutable after New).
func (r *Run) Started() time.Time { return r.state.Started }

// Status returns the current lifecycle status.
func (r *Run) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Status
}

// ExitCode returns the container exit code (or -1 for non-exit failures).
func (r *Run) ExitCode() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.ExitCode
}

// Error returns the orchestration error message, if any.
func (r *Run) Error() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Error
}

// Done returns a channel closed when the run has reached a terminal
// status.
func (r *Run) Done() <-chan struct{} { return r.done }

// Snapshot returns a JSON-friendly copy of the run with its output
// truncated to the requested tail length (use a negative value for the
// full retained buffer).
func (r *Run) Snapshot(tail int) RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.state.Output
	if tail >= 0 && tail < len(out) {
		out = out[len(out)-tail:]
	}
	cp := r.state
	cp.Output = append([]string(nil), out...)
	return cp
}

// AppendOutput adds a line to the bounded output buffer. The line is
// stored without a trailing newline.
func (r *Run) AppendOutput(line string) {
	line = strings.TrimRight(line, "\r\n")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Output = append(r.state.Output, line)
	if len(r.state.Output) > MaxOutputLines {
		drop := len(r.state.Output) - MaxOutputLines
		r.state.Output = append(r.state.Output[:0], r.state.Output[drop:]...)
	}
}

// Finish records the terminal state and closes the done channel. Calling
// Finish more than once on the same run is a no-op for the second call.
func (r *Run) Finish(status Status, exitCode int, errMsg string) {
	r.mu.Lock()
	if !r.state.Finished.IsZero() {
		r.mu.Unlock()
		return
	}
	r.state.Status = status
	r.state.ExitCode = exitCode
	r.state.Finished = time.Now().UTC()
	if errMsg != "" {
		r.state.Error = errMsg
	}
	r.mu.Unlock()
	close(r.done)
}

// SetRunning marks the run as actively executing. Useful for the dashboard
// to differentiate "queued" from "spawned".
func (r *Run) SetRunning() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Status == StatusPending {
		r.state.Status = StatusRunning
	}
}

// LastLines returns up to n trailing lines of output.
func (r *Run) LastLines(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || len(r.state.Output) == 0 {
		return nil
	}
	if n >= len(r.state.Output) {
		out := make([]string, len(r.state.Output))
		copy(out, r.state.Output)
		return out
	}
	out := make([]string, n)
	copy(out, r.state.Output[len(r.state.Output)-n:])
	return out
}

// Tracker is a concurrency-safe registry of runs.
type Tracker struct {
	mu        sync.RWMutex
	byID      map[string]*Run
	byHook    map[string][]*Run
	maxByHook int
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{
		byID:      make(map[string]*Run),
		byHook:    make(map[string][]*Run),
		maxByHook: MaxRunsPerHook,
	}
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
		done: make(chan struct{}),
	}
	t.mu.Lock()
	t.byID[r.state.ID] = r
	t.byHook[hookID] = append(t.byHook[hookID], r)
	if extra := len(t.byHook[hookID]) - t.maxByHook; extra > 0 {
		for _, old := range t.byHook[hookID][:extra] {
			delete(t.byID, old.state.ID)
		}
		t.byHook[hookID] = append(t.byHook[hookID][:0], t.byHook[hookID][extra:]...)
	}
	t.mu.Unlock()
	return r
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
