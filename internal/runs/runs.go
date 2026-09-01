// Package runs tracks the in-memory state of webhook executions.
//
// A Run is created when a hook starts and updated as the underlying
// container produces output and finally exits. The tracker keeps a
// bounded ring of finished runs per hook so the dashboard and the
// /runs/{id} endpoint can show recent history without unbounded memory
// growth.
package runs

import (
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

	// StatusSkipped is a -class "no work was done" terminal state: the delivery matched of the hook's skip_if conditions, so NO.
	StatusSkipped Status = "skipped"
)

// KnownStatus reports whether s is a status a run can actually hold. The
// /runs list filter validates against it so a typo'd status is a rather
// than a silently empty page.
func KnownStatus(s Status) bool {
	switch s {
	case StatusPending, StatusRunning, StatusSuccess, StatusFailure,
		StatusTimeout, StatusError, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

// Terminal reports whether the status is a final state (the run's done
// channel is closed and no further transitions happen).
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusFailure, StatusTimeout, StatusError, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

// MaxOutputLines is the most recent stdout/stderr lines retained per run. Older lines are dropped as new ones arrive.
const MaxOutputLines = 2000

// MaxRunsPerHook is the most recent TERMINAL runs retained per hook ID.
const MaxRunsPerHook = 50

// RunState is the value-type, mutex-free, JSON-marshalable view of a run.
// Use Run.Snapshot to obtain a stable copy; Run owns the canonical state
// behind a mutex and never exposes RunState by reference.
type RunState struct {
	ID     string `json:"id"`
	HookID string `json:"hook_id"`

	// Title is the run's friendly display title — "owner/repo#" instead of the opaque run id — rendered from the hook's run_title template at.
	Title string `json:"title,omitempty"`

	// SpawnedBy identifies the run that started this through the state API's POST /spawn — the parent's run and hook IDs — so the dashboard and.
	SpawnedBy *SpawnedBy `json:"spawned_by,omitempty"`

	// Started is when the run was accepted and began tracking — the moment it was QUEUED, before any concurrency-group wait.
	Started time.Time `json:"started"`

	// StartedAt is when the container actually launched — the pending→running transition stamped by SetRunning.
	StartedAt time.Time `json:"started_at,omitzero"`

	Finished time.Time `json:"finished,omitempty"`
	Status   Status    `json:"status"`
	ExitCode int       `json:"exit_code"`

	// Output is the most recent MaxOutputLines lines of merged stdout+stderr. The slice is newline-free.
	Output []string `json:"output,omitempty"`

	// OutputTimes carries UTC timestamp per Output line: the moment the server recorded that line (≈ when the container emitted it).
	OutputTimes []time.Time `json:"output_times,omitempty"`

	// Error is set when the run failed before or outside the container (for example, "docker: command not found").
	Error string `json:"error,omitempty"`

	// CancelRequested is set the moment a cancel is requested; the run stays in its current status until the container actually dies and the.
	CancelRequested bool `json:"cancel_requested,omitempty"`

	// CancelRequestedAt is when the cancel request arrived ( = never requested).
	CancelRequestedAt time.Time `json:"cancel_requested_at,omitzero"`

	// WaitHistory is the run's accumulated wait segments — entry per SetWaitingOn stamp, closed (End set) when the pause ends or the run.
	WaitHistory          []WaitSegment `json:"wait_history,omitempty"`
	WaitHistoryTruncated bool          `json:"wait_history_truncated,omitempty"`

	// WaitingOn describes what the run is currently paused on — a declared sleep or a contended cooperative lock (see the WaitingOn type). nil.
	WaitingOn *WaitingOn `json:"waiting_on,omitempty"`

	// Phases carries the run's lifecycle instrumentation marks (see phases.go): when the image was ready, when slots were held, when `docker.
	Phases map[Phase]time.Time `json:"phases,omitempty"`

	// Waiters lists the runs currently blocked on cooperative locks THIS run holds.
	Waiters []Waiter `json:"waiters,omitempty"`
}

// WaitingOn kinds.
const (
	// WaitingOnWait is a declared sleep (POST /wait on the state API).
	WaitingOnWait = "wait"
	// WaitingOnLock is a blocking lock acquire (POST /kv/{key}/acquire with "block": true) contending against another run's lock.
	WaitingOnLock = "lock"
	// WaitingOnGroup is a queued concurrency-group acquire: the run is still pending, waiting for a slot in its hook's concurrency_group.
	WaitingOnGroup = "group"
)

// WaitingOn is the "what is this run paused on?" record the dashboard
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
	// HolderRunID/HolderHookID name the current holder of the contended lock (kind "lock"); re-stamped as holders change while blocked.
	HolderRunID  string `json:"holder_run_id,omitempty"`
	HolderHookID string `json:"holder_hook_id,omitempty"`
	// HolderRunIDs lists every run currently holding a slot of the contended resource (kind "group": the group's active runs), in acquire order.
	HolderRunIDs []string `json:"holder_run_ids,omitempty"`
	// Position is the run's -based place in the wait queue ( = next in line, so "N ahead" renders as Position-).
	Position int `json:"position,omitempty"`
}

// MaxWaitSegments bounds a run's recorded wait history.
const MaxWaitSegments = 32

// WaitSegment is historical pause: which kind (wait/lock/group), what it contended on (Key), and exactly when it started and ended.
type WaitSegment struct {
	Kind  string    `json:"kind"`
	Key   string    `json:"key,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
}

// Waiter identifies run blocked on a cooperative lock the annotated run
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

	// onFinish is copied from the tracker at New and immutable after — read without the mutex. See Tracker.SetOnFinish.
	onFinish func(RunState)

	// onChange is copied from the tracker at New and immutable after — read without the mutex. See Tracker.SetOnChange.
	onChange func(RunState)

	// touch resets the runner's idle watchdog for this run.
	touch func()

	// onTerminal is bookkeeping the RUNNER does about this run that must land before the run is observably finished — see SetOnTerminal. nil.
	onTerminal func(RunState)

	// waitSeq numbers SetWaitingOn calls so a stale ClearWaitingOn — from a pause that a newer overlapped — cannot clear the newer pause's.
	waitSeq uint64

	// cancelReason optionally explains a cancel request (e.g. "lock stolen by run X"); the runner uses it in place of the generic "cancelled".
	cancelReason string
}

// ID returns the run's stable ID.
func (r *Run) ID() string { return r.state.ID }

// HookID returns the hook ID this run belongs to.
func (r *Run) HookID() string { return r.state.HookID }

// Started returns the time the run was created, i.e. accepted and queued (immutable after New).
func (r *Run) Started() time.Time { return r.state.Started }

// Status returns the current lifecycle status.
func (r *Run) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Status
}

// ExitCode returns the container exit code (or - for non-exit failures).
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

// Done returns a channel closed when the run has reached a terminal status.
func (r *Run) Done() <-chan struct{} { return r.done }

// notifyChange invokes the tracker's OnChange observer, when is set, with an output-stripped snapshot (Snapshot() — the same shape.
func (r *Run) notifyChange() {
	if r.onChange != nil {
		r.onChange(r.Snapshot(0))
	}
}

// RequestCancel asks the runner to kill this run's container.
func (r *Run) RequestCancel() { r.RequestCancelWithReason("") }

// RequestCancelWithReason is RequestCancel carrying an explanation — e.g. a
// lock steal naming its displacer — which the runner records as the
// cancelled run's error in place of the generic "cancelled", so the reason
// survives into run history. Only the cancel's reason sticks.
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

// Cancelled returns a channel closed a cancel has been requested.
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
	if len(r.state.Phases) > 0 {
		cp.Phases = make(map[Phase]time.Time, len(r.state.Phases))
		for k, v := range r.state.Phases {
			cp.Phases[k] = v
		}
	}
	if cp.WaitingOn != nil {
		// SetWaitingOn always replaces the pointer, never mutates the pointee — but copy anyway so a snapshot can't alias live state.
		w := *cp.WaitingOn
		w.HolderRunIDs = append([]string(nil), w.HolderRunIDs...)
		cp.WaitingOn = &w
	}
	if cp.SpawnedBy != nil {
		// Immutable set, but copy for the same no-aliasing rule.
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
	// The line IS the -output mark: stamped here, under the same lock and from the same instant, so the streams racing to produce it.
	if len(r.state.Output) == 0 {
		r.markAtLocked(PhaseFirstOutput, now)
	}
	r.state.Output = append(r.state.Output, line)
	r.state.OutputTimes = append(r.state.OutputTimes, now)
	if len(r.state.Output) > MaxOutputLines {
		drop := len(r.state.Output) - MaxOutputLines
		r.state.Output = append(r.state.Output[:0], r.state.Output[drop:]...)
		r.state.OutputTimes = append(r.state.OutputTimes[:0], r.state.OutputTimes[drop:]...)
	}
}

// Finish records the terminal state, settles every terminal side effect, and only THEN closes the done channel. Calling Finish more than on the same run is a no-op for the call — which is also what guarantees the OnTerminal and OnFinish observers fire exactly . THE ORDERING IS THE CONTRACT. Everything an observer could reach for must already be in place when the channel closes, so that <-run.Done() is a sufficient barrier on its own: onTerminal the runner's own bookkeeping (the run.finished activity line) onFinish the run-store write, lock release notifyChange the /runs/stream terminal delta close(done) ← observers unblock here, with all of the above settled Closing would make Done() mean only "the status field flipped", and every caller would need a , separate barrier to see the rest.
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
	// A terminal run is never waiting: clear any in-flight pause so neither the dashboard nor the persisted history (the onFinish snapshot below.
	r.state.WaitingOn = nil
	r.closeOpenWaitSegmentLocked(r.state.Finished)
	// Captured under the lock: unlike onFinish/onChange, onTerminal is registered mid-flight by the runner.
	onTerminal := r.onTerminal
	r.mu.Unlock()
	if onTerminal != nil {
		onTerminal(r.Snapshot(-1))
	}
	if r.onFinish != nil {
		r.onFinish(r.Snapshot(-1))
	}
	// Terminal notification AFTER the onFinish seam: by the time stream consumers hear it, the run store write has already been attempted, so a.
	r.notifyChange()
	close(r.done)
}

// SetTitle records the run's friendly display title (trimmed; the empty string is ignored — titles are never cleared, only replaced, so a later /title override wins over a template title but nothing un-names a run).
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

// SetRunning marks the run as actively executing and stamps StartedAt — the point of the processing clock, splitting queue wait (Started→here) from processing time (here→Finished).
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

// StartedAt returns when the container actually launched (pending→running), or the time while the run is still pending — and forever.
func (r *Run) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.StartedAt
}

// SetActivityTouch registers fn as the run's idle-watchdog reset.
func (r *Run) SetActivityTouch(fn func()) {
	r.mu.Lock()
	r.touch = fn
	r.mu.Unlock()
}

// SetOnTerminal registers work that must land BEFORE the run is observably finished.
func (r *Run) SetOnTerminal(fn func(RunState)) {
	r.mu.Lock()
	r.onTerminal = fn
	r.mu.Unlock()
}

// TouchActivity resets the run's idle watchdog, if is registered.
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
// sequence token for ClearWaitingOn: pauses normally run at a time per
// run, but if a newer pause overlaps — or a blocked acquire re-stamps its
// holder — the newest state wins and stale tokens become no-ops. A finished
// run is never marked (returns , which ClearWaitingOn ignores).
func (r *Run) SetWaitingOn(w WaitingOn) uint64 {
	r.mu.Lock()
	if !r.state.Finished.IsZero() {
		r.mu.Unlock()
		return 0
	}
	r.waitSeq++
	seq := r.waitSeq
	r.state.WaitingOn = &w
	// A re-stamp of the SAME logical wait — same kind and key, e.g. a queued group acquire whose position or holder set just changed, or a blocked lock changing hands — CONTINUES the trailing open.
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

// ClearWaitingOn clears the pause recorded by the SetWaitingOn that returned seq. A stale token (a newer SetWaitingOn happened since) or leaves the current state untouched.
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
