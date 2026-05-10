package server

import (
	"net/http"

	"github.com/wow-look-at-my/webhook-runner/internal/server/dashboard"
)

// handleDashboard serves the embedded static dashboard. The default
// http.ServeMux treats "/" as a catch-all, so we 404 anything other
// than the explicit asset paths rather than silently serving index.html
// for unknown URLs.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dashFS, err := dashboard.FS()
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	allowed := map[string]bool{
		"/index.html":    true,
		"/dashboard.css": true,
		"/dashboard.js":  true,
	}
	if !allowed[path] {
		http.NotFound(w, r)
		return
	}
	// Shallow-copy the request and rewrite the URL path so that
	// http.FileServer reads our normalized path, not the raw "/".
	r2 := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = path
	urlCopy.RawPath = ""
	r2.URL = &urlCopy
	http.FileServer(http.FS(dashFS)).ServeHTTP(w, r2)
}
