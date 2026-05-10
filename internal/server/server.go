// Package server wires the hook registry, runner, and HTTP routes
// together. The exported Server type satisfies http.Handler so the
// caller can host it under any net/http listener.
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

// Server is the top-level HTTP handler for webhook-runner.
type Server struct {
	registry *hooks.Registry
	runner   *runner.Runner
	tracker  *runs.Tracker
	gh       *githubstatus.Client
	log      *slog.Logger

	mux *http.ServeMux
}

// Options configure a Server.
type Options struct {
	Registry *hooks.Registry
	Runner   *runner.Runner
	Tracker  *runs.Tracker
	GitHub   *githubstatus.Client
	Logger   *slog.Logger
}

// New constructs a Server, registering all routes.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{
		registry: opts.Registry,
		runner:   opts.Runner,
		tracker:  opts.Tracker,
		gh:       opts.GitHub,
		log:      opts.Logger,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /hooks", s.handleListHooks)
	s.mux.HandleFunc("POST /hook/{id}", s.handleTrigger)
	s.mux.HandleFunc("GET /runs", s.handleListRuns)
	s.mux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /", s.handleDashboard)
}

// runRequestContext returns a background context derived from the server
// process; we deliberately do NOT use the request's context for the docker
// run because async clients hang up after the 202 — that would otherwise
// kill the container.
func (s *Server) runRequestContext() context.Context {
	return context.Background()
}
