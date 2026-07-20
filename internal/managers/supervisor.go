package managers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Flat supervision cadences — package vars so tests can shrink them.
// No backoff, no restart budget, no give-up anywhere in this package: a
// build-broken manager restarts (and fails) every RestartDelay until the
// tree is fixed.
var (
	// RestartDelay is the fixed pause between an instance ending (for any
	// reason other than an operator/reload-requested stop) and the next
	// start attempt.
	RestartDelay = 10 * time.Second
	// ParkPoll is the cadence at which a parked loop (disabled manager, or
	// waiting on the lease) re-checks state when nothing pokes it.
	ParkPoll = 2 * time.Second
)

// OutputTailLines bounds the per-manager instance output ring the admin
// surface serves (managers are not runs — their logs live here, not in the
// run store).
const OutputTailLines = 500

// ContainerName is the DETERMINISTIC instance container name for a
// manager: no instance-id suffix, so a crashed runner's orphan is findable
// (and rm -f-able) by the next lease holder, and docker's name uniqueness
// makes two live instances of one manager impossible even mid-race.
func ContainerName(id string) string { return "webhook-runner-mgr-" + id }

// StopRequest asks a running instance to stop GRACEFULLY (docker stop —
// SIGTERM, grace, then kill). Reason is surfaced on the panel and the
// activity feed ("disabled by operator", "superseded by reload", ...).
type StopRequest struct{ Reason string }

// SessionOutcome reports how an instance ended.
type SessionOutcome struct {
	Status runs.Status
	// RequestedStop: the instance ended because the supervisor asked it to
	// (disable, replace, removal, shutdown) — the loop re-evaluates
	// immediately instead of counting a failure or waiting RestartDelay.
	RequestedStop bool
	Err           string
}

// SessionRunner is what the supervisor needs from the runner: run one
// blocking instance (the container's whole lifetime) and clean up
// containers by name. Implemented by *runner.Runner.
type SessionRunner interface {
	RunManagerSession(ctx context.Context, m *hooks.Manager, ib *Inbox, instanceID, containerName string, stop <-chan StopRequest, onStarted func(), sink func(line string)) SessionOutcome
	RemoveManagerContainer(name string)
}

// Options configure a Supervisor.
type Options struct {
	Runner SessionRunner
	// LeasePath is the single-instance flock file (<data-dir>/managers.lock).
	// Empty disables the lease (tests): the supervisor runs as if it held it.
	LeasePath string
	// Disabled reports the operator kill switch's effective verdict for an
	// id, given the manager's enable default (absent = enabled, the hook
	// rule) — wired to overrides.Store.HookDisabled. nil = never disabled.
	Disabled func(id string, defaultEnabled bool) bool
	Events   *events.Recorder
	Logger   *slog.Logger
	// OnAttention receives the CURRENT set of manager problems (managers
	// that should be running but are not) whenever it re-derives — wired to
	// the attention aggregator's manager source. nil-safe.
	OnAttention func([]AttentionEntry)
	// OnInstanceEnd fires after every instance ends, with its identity —
	// the finish-seam analog: serve wires it to release the instance's
	// cooperative locks (managers are not runs, so the tracker's OnFinish
	// seam never sees them). nil-safe.
	OnInstanceEnd func(instanceID string)
}

// AttentionEntry is one "manager should be running but is not" problem for
// the needs-attention surface.
type AttentionEntry struct {
	ID       string
	Failures int
	Message  string
}

// Status is one manager's row on GET /managers.
type Status struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	// Title is the instance's friendly panel title — the run_title template
	// rendered at instance start, overridden live via POST /title.
	Title string `json:"title,omitempty"`
	// State: waiting-lease | disabled | starting | running | restart-wait |
	// stopping | removed.
	State            string    `json:"state"`
	Disabled         bool      `json:"disabled"`
	InstanceID       string    `json:"instance_id,omitempty"`
	InstanceStarted  time.Time `json:"instance_started,omitzero"`
	Restarts         int       `json:"restarts"`
	ConsecutiveFails int       `json:"consecutive_failures,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	LastStopReason   string    `json:"last_stop_reason,omitempty"`
	InboxDepth       int       `json:"inbox_depth"`
	LastDelivered    time.Time `json:"last_delivered,omitzero"`
	LastTick         time.Time `json:"last_tick,omitzero"`

	ReconcileInterval string `json:"reconcile_interval,omitempty"`
	Synchronous       bool   `json:"synchronous,omitempty"`
	ConcurrencyGroup  string `json:"concurrency_group,omitempty"`
	EnabledByDefault  bool   `json:"enabled_by_default"`
}

// Supervisor owns the manager fleet in THIS process: one loop per declared
// manager, gated behind the single-instance lease. Update (the reload
// path) replaces the desired set; the loops converge on it.
type Supervisor struct {
	runner        SessionRunner
	leasePath     string
	disabledFn    func(string, bool) bool
	events        *events.Recorder
	log           *slog.Logger
	onAttention   func([]AttentionEntry)
	onInstanceEnd func(string)

	mu       sync.Mutex
	desired  map[string]*hooks.Manager
	states   map[string]*managed
	leased   bool
	shutdown bool
	runCtx   context.Context
	wg       sync.WaitGroup
}

// managed is the per-manager supervision state.
type managed struct {
	id    string
	inbox *Inbox
	poke  chan struct{}

	state           string
	sessionStop     chan StopRequest
	instanceID      string
	instanceStarted time.Time
	title           string
	runningHash     string
	restarts        int
	consecFails     int
	lastError       string
	lastStopReason  string
	loopRunning     bool

	touch  func() // the live instance's watchdog Touch (declared waits)
	output []string
}

// New constructs a Supervisor. Call Update to declare managers and Run to
// start supervising (Run blocks acquiring the lease, then until ctx ends).
func New(opts Options) *Supervisor {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		runner:        opts.Runner,
		leasePath:     opts.LeasePath,
		disabledFn:    opts.Disabled,
		events:        opts.Events,
		log:           log,
		onAttention:   opts.OnAttention,
		onInstanceEnd: opts.OnInstanceEnd,
		desired:       map[string]*hooks.Manager{},
		states:        map[string]*managed{},
	}
}

// Update replaces the desired manager set — the reload path, called from
// the same loadAndApply closure that replaces hooks/groups/schedules so
// the entity sets can never drift apart. A running instance whose content
// hash no longer matches the fresh declaration is asked to stop gracefully
// ("superseded by reload"); its loop then starts the new image. Removed
// managers stop and their loops exit; new ones start (when the lease is
// held). Buffered inbox events survive replaces — only removal drops the
// inbox.
func (s *Supervisor) Update(managers map[string]*hooks.Manager) {
	if managers == nil {
		managers = map[string]*hooks.Manager{}
	}
	s.mu.Lock()
	s.desired = managers
	for id, m := range managers {
		mg := s.states[id]
		if mg == nil {
			mg = &managed{
				id:    id,
				poke:  make(chan struct{}, 1),
				state: "waiting-lease",
			}
			mg.inbox = NewInbox(0, s.inboxDropReporter(id))
			s.states[id] = mg
			if s.leased && !s.shutdown {
				s.startLoopLocked(mg)
			}
		}
		// Replace-on-change: compare the running instance's content hash to
		// the fresh tree's. ContentHash does I/O (hashes the manager dir +
		// sdk) but only on reload, matching EnsureImage's own cost model.
		if mg.runningHash != "" {
			if newHash, err := m.ContentHash(); err == nil && newHash != mg.runningHash {
				s.requestStopLocked(mg, "superseded by reload")
			}
		}
		s.pokeLocked(mg)
	}
	for id, mg := range s.states {
		if _, ok := managers[id]; !ok {
			s.requestStopLocked(mg, "removed from tree")
			s.pokeLocked(mg)
		}
	}
	s.mu.Unlock()
	s.reportAttention()
}

// Run acquires the single-instance lease (flat-polling until ctx ends),
// starts the loops, and blocks until ctx is done — then stops every
// instance gracefully and returns once all loops exited. The lease is held
// for the whole span: the old process's instances are down BEFORE its
// flock releases, so the successor can never overlap them.
func (s *Supervisor) Run(ctx context.Context) {
	release, ok := s.acquireLease(ctx)
	if !ok {
		return
	}
	defer release()

	s.mu.Lock()
	s.leased = true
	s.runCtx = ctx
	if s.events != nil && len(s.states) > 0 {
		s.events.Record("manager.leased", "manager lease acquired; supervising managers", nil)
	}
	for _, mg := range s.states {
		s.startLoopLocked(mg)
	}
	s.mu.Unlock()

	<-ctx.Done()
	s.Shutdown()
}

// Shutdown stops every instance gracefully ("runner shutting down") and
// waits for the loops to exit. Idempotent. Called by Run on ctx
// cancellation and by serve's shutdown path.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.shutdown = true
	for _, mg := range s.states {
		s.requestStopLocked(mg, "runner shutting down")
		s.pokeLocked(mg)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// startLoopLocked launches a manager's supervision loop (caller holds mu).
func (s *Supervisor) startLoopLocked(mg *managed) {
	if mg.loopRunning || s.shutdown {
		return
	}
	mg.loopRunning = true
	s.wg.Add(1)
	go s.managerLoop(mg)
}

// managerLoop is one manager's whole supervised life: start an instance,
// watch it end, restart flat — forever, until the manager is removed,
// disabled (parks), or the supervisor shuts down.
func (s *Supervisor) managerLoop(mg *managed) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if s.shutdown {
			mg.loopRunning = false
			mg.state = "stopping"
			s.mu.Unlock()
			return
		}
		m := s.desired[mg.id]
		if m == nil {
			mg.loopRunning = false
			delete(s.states, mg.id)
			s.mu.Unlock()
			s.reportAttention()
			return
		}
		disabled := s.disabledFn != nil && s.disabledFn(mg.id, m.EnabledByDefault())
		if disabled {
			mg.state = "disabled"
			s.mu.Unlock()
			s.reportAttention()
			if !s.sleepFlat(ParkPoll) {
				s.markLoopDone(mg)
				return
			}
			continue
		}
		stopCh := make(chan StopRequest, 1)
		mg.sessionStop = stopCh
		mg.state = "starting"
		instanceID := NewInstanceID()
		mg.instanceID = instanceID
		mg.instanceStarted = time.Now().UTC()
		// The instance's panel title: the run_title template's static
		// render (placeholders resolve against nothing — a manager's title
		// context is its own to set via POST /title mid-flight).
		mg.title = m.RenderRunTitle(nil, http.Header{})
		hash, _ := m.ContentHash()
		mg.runningHash = hash
		sink := s.outputSink(mg)
		s.mu.Unlock()
		s.reportAttention()

		name := ContainerName(mg.id)
		// Reap any orphan/stale container first: a crashed predecessor
		// process leaves its (dockerd-owned) instance running; the
		// deterministic name is what makes it findable.
		s.runner.RemoveManagerContainer(name)

		// Seed the instance's first event BEFORE it starts, so the very
		// first /inbox/next returns it: the tick is the handover/crash
		// recovery pass (event-only managers get the start event instead).
		if m.ReconcileInterval() > 0 {
			mg.inbox.PushTick()
		} else {
			mg.inbox.PushStart()
		}
		tickStop := make(chan struct{})
		if iv := m.ReconcileInterval(); iv > 0 {
			go tickPump(iv, mg.inbox, tickStop)
		}

		onStarted := func() { s.instanceRunning(mg) }
		outcome := s.runner.RunManagerSession(s.runContext(), m, mg.inbox, instanceID, name, stopCh, onStarted, sink)
		close(tickStop)

		// The finish-seam analog: the instance is over — release whatever
		// run-shaped resources it held (cooperative locks, incl. pinned).
		if s.onInstanceEnd != nil {
			s.onInstanceEnd(instanceID)
		}

		s.mu.Lock()
		mg.sessionStop = nil
		mg.runningHash = ""
		mg.instanceID = ""
		mg.instanceStarted = time.Time{}
		mg.touch = nil
		mg.restarts++
		if outcome.RequestedStop {
			mg.consecFails = 0
			mg.lastError = ""
			mg.lastStopReason = outcome.Err
		} else if outcome.Status == runs.StatusCancelled {
			// An operator restart/bounce, not a failure.
			mg.consecFails = 0
			mg.lastError = ""
			mg.lastStopReason = outcome.Err
		} else {
			mg.consecFails++
			mg.lastError = fmt.Sprintf("instance ended: %s", outcome.Status)
			if outcome.Err != "" {
				mg.lastError += ": " + outcome.Err
			}
			mg.lastStopReason = ""
		}
		requested := outcome.RequestedStop
		s.mu.Unlock()
		s.reportAttention()

		if requested {
			continue // re-evaluate immediately: disable parks, replace restarts, removal exits
		}
		s.mu.Lock()
		mg.state = "restart-wait"
		s.mu.Unlock()
		if !s.sleepFlat(RestartDelay) {
			s.markLoopDone(mg)
			return
		}
	}
}

// instanceRunning flips the manager to running the moment its container
// actually launched, and captures the live watchdog touch for /wait.
func (s *Supervisor) instanceRunning(mg *managed) {
	s.mu.Lock()
	mg.state = "running"
	mg.touch = mg.inbox.sessionTouch()
	s.mu.Unlock()
	s.reportAttention()
}

func (s *Supervisor) markLoopDone(mg *managed) {
	s.mu.Lock()
	mg.loopRunning = false
	s.mu.Unlock()
}

// runContext returns the supervisor's run context (Background before Run —
// tests that drive loops without Run).
func (s *Supervisor) runContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

// sleepFlat waits d, checking for shutdown on a flat 250ms cadence.
// false = shutting down.
func (s *Supervisor) sleepFlat(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		s.mu.Lock()
		down := s.shutdown
		s.mu.Unlock()
		if down {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		step := remaining
		if step > 250*time.Millisecond {
			step = 250 * time.Millisecond
		}
		time.Sleep(step)
	}
}

func (s *Supervisor) requestStopLocked(mg *managed, reason string) {
	if mg.sessionStop == nil {
		return
	}
	select {
	case mg.sessionStop <- StopRequest{Reason: reason}:
	default: // a stop is already pending; the first reason wins
	}
}

func (s *Supervisor) pokeLocked(mg *managed) {
	select {
	case mg.poke <- struct{}{}:
	default:
	}
}

// Poke nudges a manager's loop to re-evaluate now (operator flipped the
// kill switch back on). Missing ids are a no-op; the ParkPoll cadence
// backstops a missed poke anyway.
func (s *Supervisor) Poke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mg := s.states[id]; mg != nil {
		s.pokeLocked(mg)
	}
}

// RequestStop gracefully stops a manager's RUNNING instance with the given
// reason (the operator disable/restart path). No-op when no instance is
// live — a parked loop just needs the Poke.
func (s *Supervisor) RequestStop(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mg := s.states[id]; mg != nil {
		s.requestStopLocked(mg, reason)
		s.pokeLocked(mg)
	}
}

// Deliver pushes an authenticated delivery into a manager's inbox
// (buffering while the manager restarts; overflow drops oldest, loudly)
// and returns its completion handle — the synchronous hold and
// per-delivery github_status await it. nil = no such manager declared.
func (s *Supervisor) Deliver(id string, headers http.Header, payload []byte) *Delivered {
	s.mu.Lock()
	mg := s.states[id]
	s.mu.Unlock()
	if mg == nil {
		return nil
	}
	return mg.inbox.PushDelivery(headers, payload)
}

// InboxNext long-polls a manager's inbox on behalf of its CURRENT instance
// (see Inbox.Next). ok=false with nil error is the elapsed-wait 204.
func (s *Supervisor) InboxNext(ctx context.Context, id, instanceID string, wait time.Duration) (Event, bool, error) {
	s.mu.Lock()
	mg := s.states[id]
	s.mu.Unlock()
	if mg == nil {
		return Event{}, false, ErrNotSession
	}
	return mg.inbox.Next(ctx, instanceID, wait)
}

// IsCurrentInstance reports whether instanceID is manager id's live
// instance — the liveness guard /spawn, /wait, and /title apply to
// manager-issued tokens.
func (s *Supervisor) IsCurrentInstance(id, instanceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	return mg != nil && mg.instanceID != "" && mg.instanceID == instanceID
}

// TouchInstance credits activity to a manager's live instance (the /wait
// hold's watchdog feed). false = not the current instance.
func (s *Supervisor) TouchInstance(id, instanceID string) bool {
	s.mu.Lock()
	mg := s.states[id]
	var touch func()
	ok := mg != nil && mg.instanceID == instanceID && instanceID != ""
	if ok {
		touch = mg.touch
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	if touch != nil {
		touch()
	}
	return true
}

// SetInstanceTitle names the live instance on the panel (POST /title).
// false = not the current instance.
func (s *Supervisor) SetInstanceTitle(id, instanceID, title string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	if mg == nil || mg.instanceID == "" || mg.instanceID != instanceID {
		return false
	}
	mg.title = title
	return true
}

// Statuses returns the roster for GET /managers, alphabetical by id.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.states))
	for id, mg := range s.states {
		out = append(out, s.statusLocked(id, mg))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// StatusFor returns one manager's row, or ok=false.
func (s *Supervisor) StatusFor(id string) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	if mg == nil {
		return Status{}, false
	}
	return s.statusLocked(id, mg), true
}

// OutputTail returns the live/most-recent instance's recent output lines
// (managers are not runs; their logs live here).
func (s *Supervisor) OutputTail(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mg := s.states[id]
	if mg == nil {
		return nil
	}
	out := make([]string, len(mg.output))
	copy(out, mg.output)
	return out
}

func (s *Supervisor) statusLocked(id string, mg *managed) Status {
	st := Status{
		ID:               id,
		State:            mg.state,
		Title:            mg.title,
		InstanceID:       mg.instanceID,
		InstanceStarted:  mg.instanceStarted,
		Restarts:         mg.restarts,
		ConsecutiveFails: mg.consecFails,
		LastError:        mg.lastError,
		LastStopReason:   mg.lastStopReason,
		InboxDepth:       mg.inbox.Depth(),
	}
	st.LastDelivered, st.LastTick = mg.inbox.Stamps()
	if m := s.desired[id]; m != nil {
		st.Description = m.Description
		st.ReconcileInterval = m.ReconcileIntervalRaw
		st.Synchronous = m.Synchronous
		st.ConcurrencyGroup = m.ConcurrencyGroup
		st.EnabledByDefault = m.EnabledByDefault()
		st.Disabled = s.disabledFn != nil && s.disabledFn(id, m.EnabledByDefault())
	}
	if !s.leased && !s.shutdown {
		st.State = "waiting-lease"
	}
	return st
}

// outputSink returns the line appender for one instance's output ring:
// bounded, timestamped, replacing the ring at instance start (the panel
// shows the CURRENT/most recent instance's tail; history is the events
// feed's job).
func (s *Supervisor) outputSink(mg *managed) func(string) {
	mg.output = mg.output[:0]
	return func(line string) {
		stamped := time.Now().UTC().Format(time.RFC3339) + " " + line
		s.mu.Lock()
		mg.output = append(mg.output, stamped)
		if len(mg.output) > OutputTailLines {
			mg.output = mg.output[len(mg.output)-OutputTailLines:]
		}
		s.mu.Unlock()
	}
}

// inboxDropReporter records one loud event per overflow-dropped inbox
// entry — coalescing is operating-as-designed, but never silent.
func (s *Supervisor) inboxDropReporter(id string) func(Event) {
	return func(e Event) {
		if s.events == nil {
			return
		}
		s.events.Record("manager.inbox_dropped",
			fmt.Sprintf("%s: inbox full; dropped oldest %s event (received %s) — reconcile covers the loss",
				id, e.Kind, e.ReceivedAt.Format(time.RFC3339)),
			map[string]string{"hook": id})
	}
}

// reportAttention re-derives the manager problem set: every declared,
// enabled manager without a live instance and with a recorded failure is a
// problem; it clears the moment an instance runs (or the manager is
// disabled/removed). Pushed whole to the aggregator's manager source.
func (s *Supervisor) reportAttention() {
	if s.onAttention == nil {
		return
	}
	s.mu.Lock()
	var entries []AttentionEntry
	for id, mg := range s.states {
		m := s.desired[id]
		if m == nil {
			continue
		}
		if s.disabledFn != nil && s.disabledFn(id, m.EnabledByDefault()) {
			continue
		}
		if mg.instanceID != "" || mg.lastError == "" {
			continue
		}
		entries = append(entries, AttentionEntry{
			ID:       id,
			Failures: mg.consecFails,
			Message: fmt.Sprintf("manager %s is not running (%d consecutive failed instance(s); last: %s); restarting every %s until it holds",
				id, mg.consecFails, mg.lastError, RestartDelay),
		})
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	s.onAttention(entries)
}

// tickPump pushes coalesced reconcile ticks on the manager's flat cadence
// for the life of one instance.
func tickPump(interval time.Duration, ib *Inbox, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ib.PushTick()
		}
	}
}
