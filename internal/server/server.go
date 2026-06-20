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
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

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
}

// New constructs a Server, registering routes on both muxes.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
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
		hookMux:      http.NewServeMux(),
		adminMux:     http.NewServeMux(),
		stateMux:     http.NewServeMux(),
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
	s.hookMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.hookMux.HandleFunc("POST /hook/{id}/cancel/{run}", s.handleCancelRun)
	if s.reloadSecret != "" {
		s.hookMux.HandleFunc("POST /_reload", s.handleReloadWebhook)
	}

	// Admin port (internal, behind zero trust).
	s.adminMux.HandleFunc("GET /health", s.handleHealth)
	s.adminMux.HandleFunc("GET /hooks", s.handleListHooks)
	s.adminMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.adminMux.HandleFunc("POST /hook/{id}/cancel/{run}", s.handleCancelRun)
	s.adminMux.HandleFunc("GET /runs", s.handleListRuns)
	s.adminMux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	s.adminMux.HandleFunc("POST /runs/{id}/cancel", s.handleAdminCancelRun)
	s.adminMux.HandleFunc("POST /reload", s.handleReload)
	s.adminMux.HandleFunc("GET /config", s.handleConfig)
	s.adminMux.HandleFunc("GET /events", s.handleEvents)
	s.adminMux.HandleFunc("GET /images", s.handleImages)
	s.adminMux.HandleFunc("GET /concurrency", s.handleConcurrency)
	s.adminMux.HandleFunc("GET /kv", s.handleKVStats)
	s.adminMux.HandleFunc("GET /", s.handleDashboard)

	// State port (internal): hook containers reach their own namespace,
	// authenticated by the per-hook bearer token the runner injects. The
	// namespace comes from the verified token, never the URL.
	s.stateMux.HandleFunc("GET /kv/{key}", s.withNamespace(s.handleKVGet))
	s.stateMux.HandleFunc("PUT /kv/{key}", s.withNamespace(s.handleKVPut))
	s.stateMux.HandleFunc("DELETE /kv/{key}", s.withNamespace(s.handleKVDelete))
	s.stateMux.HandleFunc("GET /kv", s.withNamespace(s.handleKVList))
	s.stateMux.HandleFunc("POST /kv/{key}/incr", s.withNamespace(s.handleKVIncr))
}

// runRequestContext returns a background context derived from the server
// process; we deliberately do NOT use the request's context for the docker
// run because async clients hang up after the 202 — that would otherwise
// kill the container.
func (s *Server) runRequestContext() context.Context {
	return context.Background()
}
