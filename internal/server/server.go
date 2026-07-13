// Package server wires the hook registry, runner, and HTTP routes
// together. It exposes two http.Handlers: one for the public-facing
// hook port and one for the admin port (dashboard, runs, reload).
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/overrides"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
	"github.com/wow-look-at-my/webhook-runner/internal/runstore"
)

// VersionInfo identifies the running build. Version is the same string the
// `webhook-runner version` command prints; Revision and Time are the VCS
// commit and commit time Go stamped into the build, when available.
type VersionInfo struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
}

// Server holds the shared state for both the hook and admin HTTP handlers.
type Server struct {
	registry     *hooks.Registry
	runner       *runner.Runner
	tracker      *runs.Tracker
	gh           *githubstatus.Client
	secrets      *hooks.SecretsLoader
	concurrency  *concurrency.Manager
	events       *events.Recorder
	log          *slog.Logger
	reloadSecret string
	onReload     func() error
	hooksRepo    string
	hookBaseURL  string
	kv           *kv.Store
	runstore     *runstore.Store
	overrides    *overrides.Store
	version      VersionInfo

	// stream fans run lifecycle updates out to GET /runs/stream clients;
	// fed by the tracker's OnChange seam (wired in New). Never nil.
	stream *streamHub

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
	// Events is the activity feed shown on the admin dashboard. nil is
	// fine (events are dropped).
	Events *events.Recorder
	Logger *slog.Logger

	// ReloadSecret is the HMAC-SHA256 secret used to authenticate
	// POST /_reload on the hook port. When empty, the endpoint is
	// not registered.
	ReloadSecret string

	// OnReload is called when a reload is requested (admin POST /reload
	// or authenticated POST /_reload on the hook port). When a hooks
	// repo is configured, this pulls and reloads; otherwise it just
	// reloads from disk.
	OnReload func() error

	// HooksRepo is the Git remote URL of the hooks repository (SSH or
	// HTTPS). Exposed via the admin /config endpoint for the dashboard.
	HooksRepo string

	// HookBaseURL is the public base URL of the hook port (e.g.
	// "https://hooks.example.com"). Used by the dashboard to show the
	// full _reload webhook URL. Optional.
	HookBaseURL string

	// KV is the persistent state store backing the state port and the
	// admin /kv view. nil disables both (the routes report no namespaces).
	KV *kv.Store

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

	// Version identifies the running build; it is reported by /health and
	// /version on both ports. An empty Version falls back to "dev" (the
	// same default the version command uses).
	Version VersionInfo
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
		secrets:      opts.Secrets,
		concurrency:  opts.Concurrency,
		events:       opts.Events,
		log:          opts.Logger,
		reloadSecret: opts.ReloadSecret,
		onReload:     opts.OnReload,
		hooksRepo:    opts.HooksRepo,
		hookBaseURL:  opts.HookBaseURL,
		kv:           opts.KV,
		runstore:     opts.RunStore,
		overrides:    opts.Overrides,
		version:      opts.Version,
		stream:       newStreamHub(),
		hookMux:      http.NewServeMux(),
		adminMux:     http.NewServeMux(),
		stateMux:     http.NewServeMux(),
	}
	// The live tail: every run lifecycle mutation is fanned out to the
	// /runs/stream subscribers. publish never blocks (bounded per-client
	// buffers, slow clients dropped), so hooking it to the tracker's
	// mutating goroutines is safe. Wired here — before any run can exist —
	// because SetOnChange only applies to runs created after it.
	//
	// The same connection also carries coarse "section changed → refetch
	// once" signals for the non-run admin sections, so an idle dashboard
	// polls NOTHING (see streamhub.go). Three seams cover every section:
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
	if opts.Tracker != nil {
		opts.Tracker.SetOnChange(func(st runs.RunState) {
			s.stream.publish(st)
			s.stream.signal("concurrency")
		})
	}
	opts.Events.SetOnRecord(func(kind string) {
		s.stream.signal(sectionsForEvent(kind)...)
	})
	if opts.KV != nil {
		opts.KV.SetOnMutate(func() {
			s.stream.signal("kv")
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
	s.adminMux.HandleFunc("GET /version", s.handleVersion)
	s.adminMux.HandleFunc("GET /hooks", s.handleListHooks)
	s.adminMux.HandleFunc("GET /hooks/{id}", s.handleHookDetail)
	// Operator kill switch (see overrides.go): flip a hook off/on, override
	// a concurrency group's limit live. Admin-port-only by design.
	s.adminMux.HandleFunc("POST /hooks/{id}/disable", s.handleHookDisable)
	s.adminMux.HandleFunc("POST /hooks/{id}/enable", s.handleHookEnable)
	s.adminMux.HandleFunc("PUT /concurrency/{group}/limit", s.handleConcurrencyOverrideSet)
	s.adminMux.HandleFunc("DELETE /concurrency/{group}/limit", s.handleConcurrencyOverrideClear)
	s.adminMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.adminMux.HandleFunc("POST /hook/{id}/cancel/{run}", s.handleCancelRun)
	s.adminMux.HandleFunc("GET /runs", s.handleListRuns)
	// The SSE live tail. The literal "stream" segment wins over the
	// {id} pattern below (most-specific match), so no run id collision.
	s.adminMux.HandleFunc("GET /runs/stream", s.handleRunsStream)
	s.adminMux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	s.adminMux.HandleFunc("POST /runs/{id}/cancel", s.handleAdminCancelRun)
	s.adminMux.HandleFunc("POST /reload", s.handleReload)
	s.adminMux.HandleFunc("GET /config", s.handleConfig)
	s.adminMux.HandleFunc("GET /events", s.handleEvents)
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
	// First-class declared sleep: blocks ~N seconds, shows on the dashboard,
	// and counts as activity for the idle timeout (see wait.go).
	s.stateMux.HandleFunc("POST /wait", s.withNamespace(s.handleWait))
	// Friendly-title override: a run whose subject is only known mid-run
	// (a fleet sweep reaching some repo) names itself (see title.go).
	s.stateMux.HandleFunc("POST /title", s.withNamespace(s.handleRunTitle))
}

// runRequestContext returns a background context derived from the server
// process; we deliberately do NOT use the request's context for the docker
// run because async clients hang up after the 202 — that would otherwise
// kill the container.
func (s *Server) runRequestContext() context.Context {
	return context.Background()
}
