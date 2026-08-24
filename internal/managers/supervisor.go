package managers

import (
	"context"
	"log/slog"
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
	// RestartDelay is the fixed pause between an instance ending (for any reason other than an operator/reload-requested stop) and the next.
	RestartDelay = 10 * time.Second
	// ParkPoll is the cadence at which a parked loop (disabled manager, or waiting on the lease) re-checks state when nothing pokes it.
	ParkPoll = 2 * time.Second
)

// OutputTailLines bounds the per-manager instance output ring the admin surface serves (managers are not runs — their logs live here, not in.
const OutputTailLines = 500

// ContainerName is the DETERMINISTIC instance container name for a manager: no instance-id suffix, so a crashed runner's orphan is.
func ContainerName(id string) string { return "webhook-runner-mgr-" + id }

// StopRequest asks a running instance to stop GRACEFULLY (docker stop — SIGTERM, grace, then kill).
type StopRequest struct{ Reason string }

// SessionOutcome reports how an instance ended.
type SessionOutcome struct {
	Status runs.Status
	// RequestedStop: the instance ended because the supervisor asked it to (disable, replace, removal, shutdown) — the loop re-evaluates.
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
	LeasePath string
	// Disabled reports the operator kill switch's effective verdict for an id, given the manager's enable default (absent = enabled, the hook.
	Disabled func(id string, defaultEnabled bool) bool
	Events   *events.Recorder
	Logger   *slog.Logger
	// OnAttention receives the CURRENT set of manager problems (managers that should be running but are not) whenever it re-derives — wired to.
	OnAttention func([]AttentionEntry)
	// OnInstanceEnd fires after every instance ends, with its identity — the finish-seam analog: serve wires it to release the instance's.
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
	// Title is the instance's friendly panel title — the run_title template rendered at instance start, overridden live via POST /title.
	Title string `json:"title,omitempty"`
	// State: waiting-lease | disabled | starting | running | restart-wait | stopping | removed.
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

	mu sync.Mutex
	// onChange is the admin-surface change seam (see SetOnChange): every mutation the /managers roster or a /managers/{id} drill-down would.
	onChange func()
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

// SetOnChange registers fn to run after every change to the ADMIN-VISIBLE manager surface: roster state, instance identity/title, inbox.
func (s *Supervisor) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// changed fires the admin-surface seam. Call it with s.mu RELEASED.
func (s *Supervisor) changed() {
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
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
			mg.inbox = NewInbox()
			// Inbox depth and the last-delivery/last-tick stamps are part of the admin surface: route their mutations through the same seam.
			mg.inbox.SetOnChange(s.changed)
			s.states[id] = mg
			if s.leased && !s.shutdown {
				s.startLoopLocked(mg)
			}
		}
		// Replace-on-change: compare the running instance's content hash to the fresh tree's.
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
	s.changed()
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
	s.changed() // the lease landed: every row leaves "waiting-lease"

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
