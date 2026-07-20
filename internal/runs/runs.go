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
	StatusPending   Status = "pending" // created, container not yet started
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailure   Status = "failure"
	StatusTimeout   Status = "timeout"
	StatusError     Status = "error"     // failed to start, never produced an exit code
	StatusCancelled Status = "cancelled" // killed by an explicit cancel request

	// StatusSkipped is a first-class "no work was done" terminal state: the
	// delivery matched one of the hook's skip_if conditions, so NO container
	// was ever booted (no image build, no concurrency slot). The run is real
	// — tracked, persisted, on the dashboard — with near-zero duration, a
	// zero StartedAt (nothing launched), ExitCode 0 as a placeholder (there
	// was no container to exit), and its output naming the matched
	// condition. Stats count skips in their own bucket, never against
	// success rates or durations (see HookRunStats.Skipped).
	StatusSkipped Status = "skipped"
)

// Terminal reports whether the status is a final state (the run's done
// channel is closed and no further transitions happen).
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusFailure, StatusTimeout, StatusError, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

// MaxOutputLines is the most recent stdout/stderr lines retained per run.
// Older lines are dropped as new ones arrive. Sized so a hook that echoes
// its working data for the dashboard (pr-describe logs the model's input
// and reply — several hundred lines) fits without evicting its own verdict;
// worst case is ~MaxRunsPerHook*MaxOutputLines lines (~10-20MB) per hook.
const MaxOutputLines = 2000

// MaxRunsPerHook is the most recent finished runs retained per hook ID.
const MaxRunsPerHook = 50

// RunState is the value-type, mutex-free, JSON-marshalable view of a run.
// Use Run.Snapshot to obtain a stable copy; Run owns the canonical state
// behind a mutex and never exposes RunState by reference.
type RunState struct {
	ID     string `json:"id"`
	HookID string `json:"hook_id"`

	// Title is the run's friendly display title — "owner/repo#47" instead
	// of the opaque run id — rendered from the hook's run_title template at
	// run creation, or set mid-run by the hook itself via the state API's
	// POST /title (fleet sweeps only know their subject once they reach
	// it). Optional and purely additive: "" means untitled, and every
	// consumer (the dashboard feature-detects it) falls back to the id.
	Title string `json:"title,omitempty"`

	// SpawnedBy identifies the run that started this one through the state
	// API's POST /spawn — the parent's run and hook IDs — so the dashboard
	// and run history can answer "who started this". nil for runs started
	// by a delivery or a schedule tick. Additive and omitempty like Title,
	// and like Title it persists in the run store's per-run metadata blob
	// only — never the per-hook index value format.
	SpawnedBy *SpawnedBy `json:"spawned_by,omitempty"`

	// Started is when the run was accepted and began tracking — the moment
	// it was QUEUED, before any concurrency-group wait. The JSON name
	// predates the queue-wait/processing split and is kept for
	// compatibility; read it as "queued". Queue wait = StartedAt − Started.
	Started time.Time `json:"started"`

	// StartedAt is when the container actually launched — the
	// pending→running transition stamped by SetRunning. Zero means the run
	// never started (cancelled or failed while still pending/queued), and
	// runs persisted before this field existed also read back as zero.
	// Processing time = Finished − StartedAt.
	StartedAt time.Time `json:"started_at,omitzero"`

	Finished time.Time `json:"finished,omitempty"`
	Status   Status    `json:"status"`
	ExitCode int       `json:"exit_code"`

	// Output is the most recent MaxOutputLines lines of merged
	// stdout+stderr. The slice is newline-free.
	Output []string `json:"output,omitempty"`

	// OutputTimes carries one UTC timestamp per Output line: the moment
	// the server recorded that line (≈ when the container emitted it).
	// Kept as a parallel slice rather than folded into Output so Output
	// stays []string for the commit-status LastLines caller and the
	// existing JSON contract. AppendOutput/Snapshot append, evict, and
	// slice the two together, so OutputTimes always has the same length
	// as Output (or is nil when output is stripped, as in the list view).
	OutputTimes []time.Time `json:"output_times,omitempty"`

	// Error is set when the run failed before or outside the container
	// (for example, "docker: command not found"). When the container
	// itself ran and exited non-zero, Error stays empty and the failure
	// is reflected by ExitCode and Status.
	Error string `json:"error,omitempty"`

	// CancelRequested is set the moment a cancel is requested; the run
	// stays in its current status until the container actually dies and
	// the runner records StatusCancelled.
	CancelRequested bool `json:"cancel_requested,omitempty"`

	// CancelRequestedAt is when the first cancel request arrived (zero =
	// never requested). Additive: CancelRequested stays the boolean it
	// always was; this timestamp lets renderers style the kill tail — the
	// span from the request to the actual death — instead of repainting
	// the run's whole bar as cancelled from birth.
	CancelRequestedAt time.Time `json:"cancel_requested_at,omitzero"`

	// WaitHistory is the run's accumulated wait segments — one entry per
	// SetWaitingOn stamp, closed (End set) when the pause ends or the run
	// finishes. It is what lets the dashboard render a wait as HISTORICAL
	// STATE (hatching that ends exactly when the wait ended) instead of
	// deriving an open-ended hatch from current status. Bounded by
	// MaxWaitSegments; WaitHistoryTruncated flags a capped run. Additive;
	// persisted with the terminal snapshot like every RunState field.
	WaitHistory          []WaitSegment `json:"wait_history,omitempty"`
	WaitHistoryTruncated bool          `json:"wait_history_truncated,omitempty"`

	// WaitingOn describes what the run is currently paused on — a declared
	// sleep or a contended cooperative lock (see the WaitingOn type). nil
	// when the run isn't waiting. Transient: cleared when the pause ends
	// and by Finish, so a terminal run — including the snapshot persisted
	// to the run store — is never waiting.
	WaitingOn *WaitingOn `json:"waiting_on,omitempty"`

	// Waiters lists the runs currently blocked on cooperative locks THIS
	// run holds. It is DERIVED, never stored: the Run itself doesn't set
	// it — the server computes it from live runs' WaitingOn at
	// serialization time, so it only ever appears on read-path snapshots.
	Waiters []Waiter `json:"waiters,omitempty"`
}

// WaitingOn kinds.
const (
	// WaitingOnWait is a declared sleep (POST /wait on the state API).
	WaitingOnWait = "wait"
	// WaitingOnLock is a blocking lock acquire (POST /kv/{key}/acquire
	// with "block": true) contending against another run's lock.
	WaitingOnLock = "lock"
	// WaitingOnGroup is a queued concurrency-group acquire: the run is
	// still pending, waiting for a slot in its hook's concurrency_group.
	// Key names the group; HolderRunIDs/Position say who holds the slots
	// and how deep the queue is.
	WaitingOnGroup = "group"
)

// WaitingOn is the one "what is this run paused on?" record the dashboard
// renders: kind "wait" is a declared sleep with its mandatory Reason; kind
// "lock" is a blocked acquire naming the contended Key and who holds it;
// kind "group" is a queued concurrency-group acquire naming the group (Key)
// and the runs holding its slots. Until is when the pause resolves on its
// own — the sleep's end, or the blocking acquire's give-up deadline (group
// waits have none: they hold until a slot frees or the run is cancelled).
type WaitingOn struct {
	Kind   string    `json:"kind"`
	Reason string    `json:"reason,omitempty"`
	Until  time.Time `json:"until,omitzero"`
	Key    string    `json:"key,omitempty"`
	// HolderRunID/HolderHookID name the current holder of the contended
	// lock (kind "lock"); re-stamped as holders change while blocked.
	HolderRunID  string `json:"holder_run_id,omitempty"`
	HolderHookID string `json:"holder_hook_id,omitempty"`
	// HolderRunIDs lists every run currently holding a slot of the
	// contended resource (kind "group": the group's active runs), in
	// acquire order. Advisory display data, re-stamped as holders change
	// while the run waits. Callers must pass a fresh slice per stamp
	// (SetWaitingOn replaces the pointer and never deep-copies).
	HolderRunIDs []string `json:"holder_run_ids,omitempty"`
	// Position is the run's 1-based place in the wait queue (1 = next in
	// line, so "N ahead" renders as Position-1). 0/omitted = unknown or
	// not a queued kind (lock contention has no queue order).
	Position int `json:"position,omitempty"`
}

// MaxWaitSegments bounds a run's recorded wait history. A run cycling
// through more waits than this keeps its FIRST MaxWaitSegments segments
// and sets WaitHistoryTruncated — bounded memory, explicit truncation.
const MaxWaitSegments = 32

// WaitSegment is one historical pause: which kind (wait/lock/group), what
// it contended on (Key), and exactly when it started and ended. End is
// zero while the pause is still live.
type WaitSegment struct {
	Kind  string    `json:"kind"`
	Key   string    `json:"key,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
}

// Waiter identifies one run blocked on a cooperative lock the annotated run
// holds — the holder-side view of WaitingOn.
type Waiter struct {
	RunID  string `json:"run_id"`
	HookID string `json:"hook_id"`
	// Key is which of the holder's locks the waiter wants.
	Key string `json:"key,omitempty"`
}

// Run is the mutex-protected wrapper around a RunState. Always pass *Run
// (never Run by value) — copying the struct would copy its mutex.
type Run struct {
	mu     sync.Mutex
	state  RunState
	done   chan struct{}
	cancel chan struct{}

	// onFinish is copied from the tracker at New and immutable after —
	// read without the mutex. See Tracker.SetOnFinish.
	onFinish func(RunState)

	// onChange is copied from the tracker at New and immutable after —
	// read without the mutex. See Tracker.SetOnChange. Invoked (with an
	// output-stripped snapshot, outside the run mutex) after every
	// observable lifecycle mutation; nil disables notifications.
	onChange func(RunState)

	// touch resets the runner's idle watchdog for this run. The runner
	// registers it when it arms the watchdog (container launch); the state
	// API's declared waits and blocking lock acquires call it (via
	// TouchActivity) so a waiting run counts as active, never as silent.
	// nil until registered.
	touch func()

	// waitSeq numbers SetWaitingOn calls so a stale ClearWaitingOn — from
	// a pause that a newer one overlapped — cannot clear the newer pause's
	// dashboard state. 0 is never a live sequence.
	waitSeq uint64

	// cancelReason optionally explains a cancel request (e.g. "lock stolen
	// by run X"); the runner uses it in place of the generic "cancelled"
	// when recording the terminal state. Set by the first cancel only.
	cancelReason string
}

// ID returns the run's stable ID.
func (r *Run) ID() string { return r.state.ID }

// HookID returns the hook ID this run belongs to.
func (r *Run) HookID() string { return r.state.HookID }

// Started returns the time the run was created, i.e. accepted and queued
// (immutable after New). See StartedAt for when processing actually began.
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

// notifyChange invokes the tracker's OnChange observer, when one is set,
// with an output-stripped snapshot (Snapshot(0) — the same shape /runs list
// entries have). Callers invoke it OUTSIDE the run mutex, only after an
// actual state mutation: no-op calls (a stale ClearWaitingOn, a SetTitle on
// a finished run) must not emit.
func (r *Run) notifyChange() {
	if r.onChange != nil {
		r.onChange(r.Snapshot(0))
	}
}

// RequestCancel asks the runner to kill this run's container. It only
// signals; the run reaches StatusCancelled when the runner observes the
// signal and the container is actually gone. Calling it on a finished
// run is a harmless no-op (Finish wins).
func (r *Run) RequestCancel() { r.RequestCancelWithReason("") }

// RequestCancelWithReason is RequestCancel carrying an explanation — e.g. a
// lock steal naming its displacer — which the runner records as the
// cancelled run's error in place of the generic "cancelled", so the reason
// survives into run history. Only the first cancel's reason sticks.
func (r *Run) RequestCancelWithReason(reason string) {
	r.mu.Lock()
	already := r.state.CancelRequested
	r.state.CancelRequested = true
	if !already {
		r.state.CancelRequestedAt = time.Now().UTC()
	}
	if !already && reason != "" {
		r.cancelReason = reason
	}
	r.mu.Unlock()
	if !already {
		close(r.cancel)
		r.notifyChange()
	}
}

// CancelReason returns the explanation attached to the cancel request, or
// "" when none was given (or no cancel was requested).
func (r *Run) CancelReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelReason
}

// Cancelled returns a channel closed once a cancel has been requested.
func (r *Run) Cancelled() <-chan struct{} { return r.cancel }

// Snapshot returns a JSON-friendly copy of the run with its output
// truncated to the requested tail length (use a negative value for the
// full retained buffer).
func (r *Run) Snapshot(tail int) RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.state.Output
	times := r.state.OutputTimes
	if tail >= 0 && tail < len(out) {
		out = out[len(out)-tail:]
		times = times[len(times)-tail:]
	}
	cp := r.state
	cp.Output = append([]string(nil), out...)
	cp.OutputTimes = append([]time.Time(nil), times...)
	cp.WaitHistory = append([]WaitSegment(nil), r.state.WaitHistory...)
	if cp.WaitingOn != nil {
		// SetWaitingOn always replaces the pointer, never mutates the
		// pointee — but copy anyway so a snapshot can't alias live state.
		// The holder slice gets the same treatment (stampers hand over a
		// fresh slice, but a snapshot must not rely on caller discipline).
		w := *cp.WaitingOn
		w.HolderRunIDs = append([]string(nil), w.HolderRunIDs...)
		cp.WaitingOn = &w
	}
	if cp.SpawnedBy != nil {
		// Immutable once set, but copy for the same no-aliasing rule.
		sb := *cp.SpawnedBy
		cp.SpawnedBy = &sb
	}
	return cp
}

// AppendOutput adds a line to the bounded output buffer. The line is
// stored without a trailing newline.
func (r *Run) AppendOutput(line string) {
	line = strings.TrimRight(line, "\r\n")
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Output = append(r.state.Output, line)
	r.state.OutputTimes = append(r.state.OutputTimes, now)
	if len(r.state.Output) > MaxOutputLines {
		drop := len(r.state.Output) - MaxOutputLines
		r.state.Output = append(r.state.Output[:0], r.state.Output[drop:]...)
		r.state.OutputTimes = append(r.state.OutputTimes[:0], r.state.OutputTimes[drop:]...)
	}
}

// Finish records the terminal state and closes the done channel. Calling
// Finish more than once on the same run is a no-op for the second call —
// which is also what guarantees the tracker's OnFinish observer fires
// exactly once per run.
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
	// A terminal run is never waiting: clear any in-flight pause so neither
	// the dashboard nor the persisted history (the onFinish snapshot below
	// is what the run store writes) shows a finished run as waiting — and
	// close its wait-history segment at the same instant.
	r.state.WaitingOn = nil
	r.closeOpenWaitSegmentLocked(r.state.Finished)
	r.mu.Unlock()
	close(r.done)
	if r.onFinish != nil {
		r.onFinish(r.Snapshot(-1))
	}
	// Terminal notification AFTER the onFinish seam: by the time stream
	// consumers hear it, the run store write has already been attempted, so
	// a client reacting to the delta (e.g. fetching /runs/{id}) sees the
	// persisted state too.
	r.notifyChange()
}

// SetTitle records the run's friendly display title (trimmed; the empty
// string is ignored — titles are never cleared, only replaced, so a later
// /title override wins over a template title but nothing un-names a run).
// A finished run is immutable: its terminal snapshot already flowed through
// OnFinish into the run store, so a late title would diverge live state
// from history. Callers bound the length (the template renderer clamps,
// the /title route rejects) — this is the model, not the gate.
func (r *Run) SetTitle(title string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	r.mu.Lock()
	if !r.state.Finished.IsZero() {
		r.mu.Unlock()
		return
	}
	r.state.Title = title
	r.mu.Unlock()
	r.notifyChange()
}

// Title returns the run's friendly display title, "" when untitled.
func (r *Run) Title() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Title
}

// SetRunning marks the run as actively executing and stamps StartedAt — the
// zero point of the processing clock, splitting queue wait (Started→here)
// from processing time (here→Finished). Useful for the dashboard to
// differentiate "queued" from "spawned".
func (r *Run) SetRunning() {
	r.mu.Lock()
	changed := false
	if r.state.Status == StatusPending {
		r.state.Status = StatusRunning
		r.state.StartedAt = time.Now().UTC()
		changed = true
	}
	r.mu.Unlock()
	if changed {
		r.notifyChange()
	}
}

// StartedAt returns when the container actually launched (pending→running),
// or the zero time while the run is still pending — and forever, for a run
// that never started.
func (r *Run) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.StartedAt
}

// SetActivityTouch registers fn as the run's idle-watchdog reset. The runner
// calls this when it arms the watchdog at container launch; the state API's
// declared waits and blocking lock acquires then keep the run alive through
// TouchActivity. Touching a finished run's watchdog is harmless (its firing
// loop has exited), so nothing ever needs to deregister.
func (r *Run) SetActivityTouch(fn func()) {
	r.mu.Lock()
	r.touch = fn
	r.mu.Unlock()
}

// TouchActivity resets the run's idle watchdog, if one is registered. Safe
// at any lifecycle stage: before the watchdog is armed and after the run
// finished it is a no-op.
func (r *Run) TouchActivity() {
	r.mu.Lock()
	fn := r.touch
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetWaitingOn marks the run as paused on w (a declared sleep or a
// contended lock) so the dashboard can render it live. It returns a
// sequence token for ClearWaitingOn: pauses normally run one at a time per
// run, but if a newer pause overlaps — or a blocked acquire re-stamps its
// holder — the newest state wins and stale tokens become no-ops. A finished
// run is never marked (returns 0, which ClearWaitingOn ignores).
func (r *Run) SetWaitingOn(w WaitingOn) uint64 {
	r.mu.Lock()
	if !r.state.Finished.IsZero() {
		r.mu.Unlock()
		return 0
	}
	r.waitSeq++
	seq := r.waitSeq
	r.state.WaitingOn = &w
	// A re-stamp of the SAME logical wait — same kind and key, e.g. a
	// queued group acquire whose position or holder set just changed, or a
	// blocked lock changing hands — CONTINUES the trailing open segment:
	// one logical wait is one history entry. (Without this, every restamp
	// closed and reopened the segment — a single 7-deep queue wait
	// accumulated 14 entries in reproduction — bloating each SSE delta and
	// fragmenting the rendered hatch.) Only a different wait closes the
	// open segment and opens a new one.
	if n := len(r.state.WaitHistory); n > 0 {
		if last := &r.state.WaitHistory[n-1]; last.End.IsZero() && last.Kind == w.Kind && last.Key == w.Key {
			r.mu.Unlock()
			r.notifyChange()
			return seq
		}
	}
	r.closeOpenWaitSegmentLocked(time.Now().UTC())
	if len(r.state.WaitHistory) < MaxWaitSegments {
		r.state.WaitHistory = append(r.state.WaitHistory, WaitSegment{
			Kind: w.Kind, Key: w.Key, Start: time.Now().UTC(),
		})
	} else {
		r.state.WaitHistoryTruncated = true
	}
	r.mu.Unlock()
	r.notifyChange()
	return seq
}

// closeOpenWaitSegmentLocked stamps End on the trailing open wait segment,
// if any. Caller holds r.mu.
func (r *Run) closeOpenWaitSegmentLocked(at time.Time) {
	if n := len(r.state.WaitHistory); n > 0 && r.state.WaitHistory[n-1].End.IsZero() {
		r.state.WaitHistory[n-1].End = at
	}
}

// ClearWaitingOn clears the pause recorded by the SetWaitingOn that
// returned seq. A stale token (a newer SetWaitingOn happened since) or 0
// leaves the current state untouched. Calling it after Finish is a harmless
// no-op — Finish already cleared the field.
func (r *Run) ClearWaitingOn(seq uint64) {
	r.mu.Lock()
	if seq == 0 || seq != r.waitSeq || r.state.WaitingOn == nil {
		r.mu.Unlock()
		return
	}
	r.state.WaitingOn = nil
	r.closeOpenWaitSegmentLocked(time.Now().UTC())
	r.mu.Unlock()
	r.notifyChange()
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
	if extra := len(t.byHook[hookID]) - t.maxByHook; extra > 0 {
		for _, old := range t.byHook[hookID][:extra] {
			delete(t.byID, old.state.ID)
		}
		t.byHook[hookID] = append(t.byHook[hookID][:0], t.byHook[hookID][extra:]...)
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
