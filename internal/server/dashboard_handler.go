package server

import (
	"bytes"
	"net/http"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/server/dashboard"
)

// handleDashboard serves the embedded static dashboard. The default http.ServeMux treats "/" as a catch-all, so anything other than the known asset paths 404s rather than silently serving index.html. Cache policy (see the dashboard package doc for the incident behind it): - "/" and the bare asset paths are no-cache, so an edge cache in front of the admin port (Cloudflare caches .css/.js by file extension when the origin says nothing) always revalidates them. - The content-addressed /dashboard.<hash>.css|.js paths — the ones the served index.html references — are immutable and cacheable forever: a new build changes the hash, so a URL's content can never change. - A hashed path whose hash isn't current 404s (falls through to NotFound).
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboard.Index)
	case "/" + dashboard.CSS.HashedName:
		serveAsset(w, r, dashboard.CSS, true)
	case "/" + dashboard.JS.HashedName:
		serveAsset(w, r, dashboard.JS, true)
	case "/" + dashboard.TimelineJS.HashedName:
		serveAsset(w, r, dashboard.TimelineJS, true)
	case "/dashboard.css": // compat: pre-hashing URL, kept working but never cached
		serveAsset(w, r, dashboard.CSS, false)
	case "/dashboard.js":
		serveAsset(w, r, dashboard.JS, false)
	case "/timeline.js":
		serveAsset(w, r, dashboard.TimelineJS, false)
	default:
		http.NotFound(w, r)
	}
}

// serveAsset writes one embedded asset. immutable selects the cache policy:
// forever for content-addressed URLs, always-revalidate for the stable ones.
// The ETag (the asset's content hash) lets ServeContent answer conditional
// requests with 304s either way.
func serveAsset(w http.ResponseWriter, r *http.Request, a dashboard.Asset, immutable bool) {
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", `"`+a.Hash+`"`)
	w.Header().Set("Content-Type", a.ContentType)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.Body))
}
