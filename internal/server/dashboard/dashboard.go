// Package dashboard embeds the static HTML/JS for the simple read-only
// dashboard served at GET /.
package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed assets/*
var assets embed.FS

// FS returns a sub-filesystem rooted at the assets directory so callers
// can serve the files at /.
func FS() (fs.FS, error) {
	return fs.Sub(assets, "assets")
}
