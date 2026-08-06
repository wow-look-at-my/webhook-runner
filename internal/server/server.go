// Package server wires the hook registry, runner, and HTTP routes
// together. It exposes two http.Handlers: one for the public-facing
// hook port and one for the admin port (dashboard, runs, reload).
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/attention"
	"github.com/wow-look-at-my/webhook-runner/internal/backlog"
	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/reloadgate"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/runstore"
	"github.com/wow-look-at-my/webhook-runner/internal/spool"
)

// VersionInfo identifies the running build. Version is the same string the
// `webhook-runner version` command prints; Revision and Time are the VCS
// commit and commit time Go stamped into the build, when available.
type VersionInfo struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
}

// ReloadGate is the consumer-side seam for the hooks-repo CI green-gate
// (implemented by *reloadgate.Gate). HandleEvent processes one
// HMAC-verified /_reload delivery — the X-GitHub-Event header plus body —
// and returns a short status for the HTTP response; a non-nil error is
// answered 500 so GitHub records a red, redeliverable delivery.
type ReloadGate interface {
	HandleEvent(event string, body []byte) (status string, err error)
}

// Server holds the shared state for both the hook and admin HTTP handlers.
type Server struct {
	registry     *hooks.Registry
	runner       *runner.Runner
	tracker      *runs.Tracker
	gh           *githubstatus.Client
	secrets      *hooks.SecretsLoader
	concurrency  *concurrency.Manager
	globalCap    *concurrency.Global
	events       *events.Recorder
	attention    *attention.Aggregator
	log          *slog.Logger
	reloadSecret string
	onReload     func() error
	gate         ReloadGate
	treeState    func() reloadgate.TreeState
	hooksRepo    string
	hooksBranch  string
	hookBaseURL  string
	kv           *kv.Store
	backlogs     *backlog.Store
	runstore     *runstore.Store
	overrides    *overrides.Store
	managers     ManagerControl
	version      VersionInfo

	// The admin reload panel's seams (reloadpanel.go): the hooks-repo
	// clone's read surface, the gate's manual-control surface, and the
	// small CI-verdict cache.
	reloadRepo    ReloadRepo
	reloadControl ReloadControl
	ciMu          sync.Mutex
	ciCache       map[string]ciCacheEntry
	// ciInflight dedupes the background CI refreshes reloadCIState kicks
	// off, so a page polling every second cannot stack one GitHub call per
	// tick on a sha whose probe is already running.
	ciInflight map[string]bool

	// stream fans run lifecycle updates out to GET /runs/stream clients;
	// fed by the tracker's OnChange seam (wired in New). Never nil.
	stream *streamHub

	// The docker-updater pre-check (restartready.go): how long a busy
	// fleet may hold off an update, and the continuously-blocked clock
	// that bounds it.
	restartMaxDefer time.Duration
	restart         restartGate

	// spool parks deliveries that arrive while the runner is draining, so a
	// deploy window costs a webhook its latency instead of its existence
	// (spooldelivery.go). nil keeps the old 503-and-lose behavior.
	spool *spool.Store

	hookMux  *http.ServeMux
	adminMux *http.ServeMux
	stateMux *http.ServeMux
}

// Options configure a Server.
type Options struct {
	Registry *hooks.Registry
	Runner   *runner.Runner
	Tracker  *runs.Tracker
	GitHub   *githubstatus.Client
	// Secrets decrypts per-hook sops secrets files; api_key ${NAME}
	// references resolve through it. nil disables decryption.
	Secrets *hooks.SecretsLoader
	// Concurrency exposes the live state of the named concurrency groups
	// on the admin port. nil is fine (the endpoint reports no groups).
	Concurrency *concurrency.Manager
	// GlobalCap is the server-wide run cap surfaced on GET /concurrency
	// and controlled by PUT|DELETE /concurrency-global/limit. nil is fine
	// (the view omits it and the endpoints answer 500 "not configured");
	// serve always wires one.
	GlobalCap *concurrency.Global
	// Events is the activity feed shown on the admin dashboard. nil is
	// fine (events are dropped).
	Events *events.Recorder
	// Attention is the aggregated "needs attention" problem set behind
	// GET /attention and the dashboard's red banner. The server wires its
	// onChange seam to the stream's "attention" section signal and feeds
	// it every recorded activity event (the event-derived entry seam).
	// nil is fine (the endpoint reports zero problems).
	Attention *attention.Aggregator
	Logger    *slog.Logger

	// ReloadSecret is the HMAC-SHA256 secret used to authenticate
	// POST /_reload on the hook port. When empty, the endpoint is
	// not registered.
	ReloadSecret string

	// OnReload is called when a reload is requested (admin POST /reload
	// or authenticated POST /_reload on the hook port). When a hooks
	// repo is configured, this pulls and reloads; otherwise it just
	// reloads from disk. With a Gate configured, serve wires this to the
	// gate's Force — admin /reload is the operator's deliberate bypass.
	OnReload func() error

	// Gate, when set, makes POST /_reload event-aware (the hooks-repo CI
	// green-gate, internal/reloadgate): after HMAC verification the
	// delivery's X-GitHub-Event and body are handed to it, and only a
	// green gating status moves the hooks tree. nil keeps the legacy
	// behavior (any signed POST pulls + reloads).
	Gate ReloadGate

	// TreeState, when set, reports the reload gate's hooks-tree state —
	// which commit the served hooks tree is at, and the pending commit +
	// hold reason while the gate is holding — included as hooks_tree in
	// /version on both ports (serve wires it to reloadgate.Gate.TreeState).
	// nil means no gate tracks the tree (no hooks repo, or the legacy
	// gate-disabled mode) and /version names that mode instead.
	TreeState func() reloadgate.TreeState

	// HooksRepo is the Git remote URL of the hooks repository (SSH or
	// HTTPS). Exposed via the admin /config endpoint for the dashboard.
	HooksRepo string

	// HooksBranch is the tracked hooks-repo branch ("" = the repo
	// default). Shown by the admin reload panel.
	HooksBranch string

	// ReloadRepo exposes read/inspect operations on the hooks-repo clone
	// for the admin reload panel (nil = no repo configured; the panel
	// reports mode "none" and hides). See reloadpanel.go.
	ReloadRepo ReloadRepo

	// ReloadControl is the reload gate's manual-control surface (status
	// snapshot, on-demand reconcile, manual commit switch, CI probe) —
	// *reloadgate.Gate in production. nil = the gate is disabled (legacy
	// mode): the panel degrades and per-commit switching is refused.
	ReloadControl ReloadControl

	// HookBaseURL is the public base URL of the hook port (e.g.
	// "https://hooks.example.com"). Used by the dashboard to show the
	// full _reload webhook URL. Optional.
	HookBaseURL string

	// KV is the persistent state store backing the state port and the
	// admin /kv view. nil disables both (the routes report no namespaces).
	KV *kv.Store

	// Backlogs is the durable batch-backlog store behind the state port's
	// /backlog routes (internal/backlog — the drain-a-slice sibling of
	// internal/queue's run scheduler). Nil disables them (503), like KV.
	Backlogs *backlog.Store

	// RunStore is the persisted completed-run history. When set, /runs,
	// /runs/{id}, and /hooks/{id} serve the live tracker merged with it
	// (deduped by run ID, newest-first); nil keeps the old memory-only
	// behavior.
	RunStore *runstore.Store

	// Overrides is the operator kill-switch store: per-hook disable
	// switches (deliveries 503, scheduled runs skipped) and concurrency
	// limit overrides, persisted under the data dir. nil disables the
	// override endpoints (reads treat every hook as enabled).
	Overrides *overrides.Store

	// Managers is the manager supervisor's server surface (deliveries into
	// inboxes, /inbox/next, the admin roster/kill switch). nil disables
	// manager support (a delivery for a declared manager then 503s, which
	// cannot happen in serve — the supervisor is always wired there).
	Managers ManagerControl

	// Version identifies the running build; it is reported by /health and
	// /version on both ports. An empty Version falls back to "dev" (the
	// same default the version command uses).
	Version VersionInfo

	// Spool parks deliveries that arrive during shutdown drain for the next
	// process to run. nil means a draining server answers 503 and the
	// delivery is lost — GitHub does not re-send it.
	Spool *spool.Store

	// RestartMaxDefer bounds how long GET /restart-ready (the
	// docker-updater pre-check) may keep answering 503 because runs are in
	// flight. Zero uses DefaultRestartMaxDefer; negative disables the force
	// so the check blocks for as long as the fleet stays busy.
	RestartMaxDefer time.Duration
}

// New constructs a Server, registering routes on both muxes.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version.Version == "" {
		opts.Version.Version = "dev"
	}
	s := &Server{
		registry:     opts.Registry,
		runner:       opts.Runner,
		tracker:      opts.Tracker,
		gh:           opts.GitHub,
		managers:     opts.Managers,
		secrets:      opts.Secrets,
		concurrency:  opts.Concurrency,
		globalCap:    opts.GlobalCap,
		events:       opts.Events,
		attention:    opts.Attention,
		log:          opts.Logger,
		reloadSecret: opts.ReloadSecret,
		onReload:     opts.OnReload,
		gate:         opts.Gate,
		treeState:    opts.TreeState,
		hooksRepo:    opts.HooksRepo,
		hooksBranch:  opts.HooksBranch,
		hookBaseURL:  opts.HookBaseURL,
		kv:           opts.KV,
		backlogs:     opts.Backlogs,
		runstore:     opts.RunStore,
		overrides:    opts.Overrides,
		version:      opts.Version,
		spool:        opts.Spool,
		restartMaxDefer: func() time.Duration {
			if opts.RestartMaxDefer == 0 {
				return DefaultRestartMaxDefer
			}
			return opts.RestartMaxDefer
		}(),
		stream:   newStreamHub(),
		hookMux:  http.NewServeMux(),
		adminMux: http.NewServeMux(),
		stateMux: http.NewServeMux(),

		reloadRepo:    opts.ReloadRepo,
		reloadControl: opts.ReloadControl,
	}
	// The live tail: every run lifecycle mutation is fanned out to the
	// /runs/stream subscribers. publish never blocks (bounded per-client
	// buffers, slow clients dropped), so hooking it to the tracker's
	// mutating goroutines is safe. Wired here — before any run can exist —
	// because SetOnChange only applies to runs created after it.
	//
	// The same connection also carries coarse "section changed → refetch
	// once" signals for the non-run admin sections, so an idle dashboard
	// polls NOTHING (see streamhub.go). Four seams cover every section:
	//   - run lifecycle (below): concurrency-group active/waiting/holder
	//     state moves exactly with run lifecycle and waiting_on changes
	//     (acquire = start, release = finish, queue join/position =
	//     waiting_on) — a superset signal, cheap for the client to honor.
	//   - the activity feed: every recorded event dirties "events", and
	//     sectionsForEvent maps kinds to the sections they imply (reloads →
	//     hooks/images/concurrency, image builds → images, kill-switch
	//     flips → hooks, limit overrides → concurrency). The runner's
	//     records flow through the same shared Recorder.
	//   - kv entry mutations: the store's own seam (state-API writes and
	//     sweeper reclaims alike).
	//   - the manager supervisor: instance output, inbox depth/stamps and
	//     state transitions, none of which record an activity event.
	if opts.Tracker != nil {
		opts.Tracker.SetOnChange(func(st runs.RunState) {
			s.stream.publish(st)
			s.stream.signal("concurrency")
		})
	}
	opts.Events.SetOnRecord(func(ev events.Event) {
		s.stream.signal(sectionsForEvent(ev.Kind)...)
		// The attention aggregator's event seam: recognized kinds (see
		// attention.RegisterStandardEventRules) become "needs attention"
		// entries; everything else is a no-op. Any resulting set change
		// signals "attention" via the aggregator's own onChange below.
		s.attention.ObserveEvent(ev.Kind, ev.Fields["hook"], ev.Msg)
	})
	// The attention seam: any real change to the active problem set —
	// a reload re-derivation, a boot verdict, an event-derived entry —
	// dirties the dashboard's "attention" section (banner + panel).
	opts.Attention.SetOnChange(func() {
		s.stream.signal("attention")
	})
	if opts.KV != nil {
		opts.KV.SetOnMutate(func() {
			s.stream.signal("kv")
		})
	}
	// The manager seam: instance OUTPUT lines, inbox depth/stamps, and
	// supervision state transitions are all on the Managers panel and the
	// #manager=<id> drill-down, and none of them record an activity event —
	// so without this the panel only moved on the occasional lifecycle
	// event (manager.started/exited) and F5 was the operator's refresh.
	if opts.Managers != nil {
		opts.Managers.SetOnChange(func() {
			s.stream.signal("managers")
		})
	}
	s.registerRoutes()
	return s
}

// HookHandler returns the handler for the public hook port.
func (s *Server) HookHandler() http.Handler { return s.hookMux }

// AdminHandler returns the handler for the internal admin port.
func (s *Server) AdminHandler() http.Handler { return s.adminMux }

// StateHandler returns the handler for the internal state (KV) port. Its
// audience is hook containers, authenticated per-hook by bearer token — kept
// off the public hook port and separate from the admin surface.
func (s *Server) StateHandler() http.Handler { return s.stateMux }

func (s *Server) registerRoutes() {
	// Hook port (public, exposed via tunnel).
	s.hookMux.HandleFunc("GET /health", s.handleHealth)
	s.hookMux.HandleFunc("GET /version", s.handleVersion)
	s.hookMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.hookMux.HandleFunc("POST /hook/{id}/cancel/{run}", s.handleCancelRun)
	if s.reloadSecret != "" {
		s.hookMux.HandleFunc("POST /_reload", s.handleReloadWebhook)
	}

	// Admin port (internal, behind zero trust).
	s.adminMux.HandleFunc("GET /health", s.handleHealth)
	s.adminMux.HandleFunc("GET /restart-ready", s.handleRestartReady)
	s.adminMux.HandleFunc("GET /version", s.handleVersion)
	s.adminMux.HandleFunc("GET /hooks", s.handleListHooks)
	s.adminMux.HandleFunc("GET /hooks/{id}", s.handleHookDetail)
	// Operator kill switch (see overrides.go): flip a hook off/on, override
	// a concurrency group's limit live. Admin-port-only by design.
	s.adminMux.HandleFunc("POST /hooks/{id}/disable", s.handleHookDisable)
	s.adminMux.HandleFunc("POST /hooks/{id}/enable", s.handleHookEnable)
	// Managers: the first-class roster (state, instance, restarts, inbox,
	// output tail), the kill switch, and the instance bounce.
	s.adminMux.HandleFunc("GET /managers", s.handleListManagers)
	s.adminMux.HandleFunc("GET /managers/{id}", s.handleManagerDetail)
	s.adminMux.HandleFunc("POST /managers/{id}/disable", s.handleManagerDisable)
	s.adminMux.HandleFunc("POST /managers/{id}/enable", s.handleManagerEnable)
	s.adminMux.HandleFunc("POST /managers/{id}/restart", s.handleManagerRestart)
	s.adminMux.HandleFunc("PUT /concurrency/{group}/limit", s.handleConcurrencyOverrideSet)
	s.adminMux.HandleFunc("DELETE /concurrency/{group}/limit", s.handleConcurrencyOverrideClear)
	// The GLOBAL run cap's override pair. A dedicated literal path —
	// deliberately NOT /concurrency/{group}/… — so it can never collide
	// with a declared group name (group names come from the hooks repo).
	s.adminMux.HandleFunc("PUT /concurrency-global/limit", s.handleGlobalCapOverrideSet)
	s.adminMux.HandleFunc("DELETE /concurrency-global/limit", s.handleGlobalCapOverrideClear)
	s.adminMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.adminMux.HandleFunc("POST /hook/{id}/cancel/{run}", s.handleCancelRun)
	s.adminMux.HandleFunc("GET /runs", s.handleListRuns)
	// The SSE live tail. The literal "stream" segment wins over the
	// {id} pattern below (most-specific match), so no run id collision.
	s.adminMux.HandleFunc("GET /runs/stream", s.handleRunsStream)
	s.adminMux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	s.adminMux.HandleFunc("POST /runs/{id}/cancel", s.handleAdminCancelRun)
	s.adminMux.HandleFunc("POST /reload", s.handleReload)
	// The hooks-repo reload panel (reloadpanel.go): live/pending commit
	// status, recent origin history, reload-on-demand, and the manual
	// commit switch with its server-enforced informed override.
	s.adminMux.HandleFunc("GET /reload/status", s.handleReloadStatus)
	s.adminMux.HandleFunc("GET /reload/commits", s.handleReloadCommits)
	s.adminMux.HandleFunc("POST /reload/check", s.handleReloadCheck)
	s.adminMux.HandleFunc("POST /reload/switch", s.handleReloadSwitch)
	s.adminMux.HandleFunc("GET /config", s.handleConfig)
	s.adminMux.HandleFunc("GET /events", s.handleEvents)
	// The persistent misconfiguration surface (see attention.go): the
	// dashboard's red banner + Needs attention panel read this.
	s.adminMux.HandleFunc("GET /attention", s.handleAttention)
	s.adminMux.HandleFunc("GET /images", s.handleImages)
	s.adminMux.HandleFunc("GET /concurrency", s.handleConcurrency)
	s.adminMux.HandleFunc("GET /kv", s.handleKVStats)
	// State inspection (deliberately value-bearing — see kvadmin.go).
	s.adminMux.HandleFunc("GET /kv/{namespace}", s.handleKVNamespace)
	s.adminMux.HandleFunc("GET /kv/{namespace}/{key}", s.handleKVEntry)
	s.adminMux.HandleFunc("GET /", s.handleDashboard)

	// State port (internal): hook containers reach their own namespace,
	// authenticated by the per-hook bearer token the runner injects. The
	// namespace comes from the verified token, never the URL.
	s.stateMux.HandleFunc("GET /kv/{key}", s.withNamespace(s.handleKVGet))
	s.stateMux.HandleFunc("PUT /kv/{key}", s.withNamespace(s.handleKVPut))
	s.stateMux.HandleFunc("DELETE /kv/{key}", s.withNamespace(s.handleKVDelete))
	s.stateMux.HandleFunc("GET /kv", s.withNamespace(s.handleKVList))
	s.stateMux.HandleFunc("POST /kv/{key}/incr", s.withNamespace(s.handleKVIncr))
	// Cooperative run-owned locks: atomic acquire/release bound to the run
	// identity in the token (internal/kv/lock.go). Lock state is separate
	// from the entries the routes above serve. Acquire names the holder on
	// contention and can block ({"block": true}); steal is a separate route
	// because it cancels the displaced holder — destructive intent must be
	// unmistakable.
	s.stateMux.HandleFunc("POST /kv/{key}/acquire", s.withNamespace(s.handleKVAcquire))
	s.stateMux.HandleFunc("POST /kv/{key}/release", s.withNamespace(s.handleKVRelease))
	s.stateMux.HandleFunc("POST /kv/{key}/steal", s.withNamespace(s.handleKVSteal))
	// Pin/unpin: the holder toggles its lock's steal-protection — a pinned
	// lock refuses steals (409 naming the pinned holder) until unpinned,
	// released, or the run ends. Separate routes like steal: a mode change
	// must be unmistakable in request lines and logs (see state.go).
	s.stateMux.HandleFunc("POST /kv/{key}/pin", s.withNamespace(s.handleKVPin))
	s.stateMux.HandleFunc("POST /kv/{key}/unpin", s.withNamespace(s.handleKVUnpin))
	// First-class declared sleep: blocks ~N seconds, shows on the dashboard,
	// and counts as activity for the idle timeout (see wait.go).
	s.stateMux.HandleFunc("POST /wait", s.withNamespace(s.handleWait))
	// The manager inbox pop: the long-poll a live manager instance loops on
	// (deliveries + reconcile ticks; see managers.go).
	s.stateMux.HandleFunc("POST /inbox/next", s.withNamespace(s.handleInboxNext))
	// Friendly-title override: a run whose subject is only known mid-run
	// (a fleet sweep reaching some repo) names itself (see title.go).
	s.stateMux.HandleFunc("POST /title", s.withNamespace(s.handleRunTitle))
	// Instrumentation: the injected shim reports the container's first
	// instruction, the one lifecycle mark the host cannot see (see phase.go).
	// Deliberately a single fixed route, not POST /phase/{name}: hooks must
	// not be able to stamp arbitrary marks, or the measurement stops meaning
	// what it says.
	s.stateMux.HandleFunc("POST /phase/container-entry", s.withNamespace(s.handleContainerEntry))
	// Spawn: a permitted MANAGER starts runs of ANOTHER hook through the
	// runner itself — deny-by-default manager.json spawn_targets, normal
	// dispatch, skip_if deliberately bypassed like scheduled fires (see
	// spawn.go).
	s.stateMux.HandleFunc("POST /spawn", s.withNamespace(s.handleSpawn))
	// Durable batch backlogs: what is LEFT for a run that already exists and
	// can only afford part of the work (internal/backlog — distinct from
	// internal/queue, which decides WHEN to start a run). A hook run is a
	// container that lives for one delivery, so a backlog cannot live inside
	// it; every hook that tried built a cursor out of KV strings and stranded
	// its tail. Push is a set union that keeps order; take REMOVES a slice.
	s.stateMux.HandleFunc("POST /backlog/{name}/push", s.withNamespace(s.handleBacklogPush))
	s.stateMux.HandleFunc("POST /backlog/{name}/take", s.withNamespace(s.handleBacklogTake))
	s.stateMux.HandleFunc("GET /backlog/{name}", s.withNamespace(s.handleBacklogStat))
	s.stateMux.HandleFunc("GET /backlogs", s.withNamespace(s.handleBacklogList))
}

// runRequestContext returns a background context derived from the server
// process; we deliberately do NOT use the request's context for the docker
// run because async clients hang up after the 202 — that would otherwise
// kill the container.
func (s *Server) runRequestContext() context.Context {
	return context.Background()
}
