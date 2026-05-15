// Package server wires the hook registry, runner, and HTTP routes
// together. It exposes two http.Handlers: one for the public-facing
// hook port and one for the admin port (dashboard, runs, reload).
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/githubstatus"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// Server holds the shared state for both the hook and admin HTTP handlers.
type Server struct {
	registry     *hooks.Registry
	runner       *runner.Runner
	tracker      *runs.Tracker
	gh           *githubstatus.Client
	log          *slog.Logger
	reloadSecret string
	onReload     func() error

	hookMux  *http.ServeMux
	adminMux *http.ServeMux
}

// Options configure a Server.
type Options struct {
	Registry *hooks.Registry
	Runner   *runner.Runner
	Tracker  *runs.Tracker
	GitHub   *githubstatus.Client
	Logger   *slog.Logger

	// ReloadSecret is the HMAC-SHA256 secret used to authenticate
	// POST /_reload on the hook port. When empty, the endpoint is
	// not registered.
	ReloadSecret string

	// OnReload is called when a reload is requested (admin POST /reload
	// or authenticated POST /_reload on the hook port). When a hooks
	// repo is configured, this pulls and reloads; otherwise it just
	// reloads from disk.
	OnReload func() error
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
		log:          opts.Logger,
		reloadSecret: opts.ReloadSecret,
		onReload:     opts.OnReload,
		hookMux:      http.NewServeMux(),
		adminMux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// HookHandler returns the handler for the public hook port.
func (s *Server) HookHandler() http.Handler { return s.hookMux }

// AdminHandler returns the handler for the internal admin port.
func (s *Server) AdminHandler() http.Handler { return s.adminMux }

func (s *Server) registerRoutes() {
	// Hook port (public, exposed via tunnel).
	s.hookMux.HandleFunc("GET /health", s.handleHealth)
	s.hookMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	if s.reloadSecret != "" {
		s.hookMux.HandleFunc("POST /_reload", s.handleReloadWebhook)
	}

	// Admin port (internal, behind zero trust).
	s.adminMux.HandleFunc("GET /health", s.handleHealth)
	s.adminMux.HandleFunc("GET /hooks", s.handleListHooks)
	s.adminMux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.adminMux.HandleFunc("GET /runs", s.handleListRuns)
	s.adminMux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	s.adminMux.HandleFunc("POST /reload", s.handleReload)
	s.adminMux.HandleFunc("GET /", s.handleDashboard)
}

// runRequestContext returns a background context derived from the server
// process; we deliberately do NOT use the request's context for the docker
// run because async clients hang up after the 202 — that would otherwise
// kill the container.
func (s *Server) runRequestContext() context.Context {
	return context.Background()
}
