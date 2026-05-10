package server

import (
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/server/dashboard"
)

// handleDashboard serves the embedded static dashboard. The default
// http.ServeMux treats "/" as a catch-all, so we explicitly 404 anything
// other than the known asset paths rather than silently serving
// index.html for unknown URLs.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dashFS, err := dashboard.FS()
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	allowed := map[string]bool{
		"/":              true,
		"/dashboard.css": true,
		"/dashboard.js":  true,
	}
	if !allowed[r.URL.Path] {
		http.NotFound(w, r)
		return
	}
	// Pass through to FileServer, which serves index.html for "/" and
	// redirects "/index.html" to "/" — we don't rewrite paths here
	// because that triggers exactly that redirect.
	http.FileServer(http.FS(dashFS)).ServeHTTP(w, r)
}
