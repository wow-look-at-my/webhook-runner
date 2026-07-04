// Package dashboard embeds the static HTML/JS for the simple read-only
// dashboard served at GET /.
//
// The CSS and JS are content-addressed: each is also reachable at
// /dashboard.<hash>.css|.js (hash = first 12 hex chars of the SHA-256 of
// the embedded bytes), and the served index.html references those hashed
// URLs. Hashed URLs are safe to cache forever (immutable); index.html and
// the bare asset paths must never be cached (no-cache), or an edge cache
// (Cloudflare caches .css/.js by extension when the origin sends no cache
// headers) can pair a new index.html with stale assets after a deploy —
// which is exactly the incident that motivated this.
package dashboard

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

//go:embed assets/*
var assets embed.FS

// Asset is one embedded dashboard file plus its content identity.
type Asset struct {
	Body        []byte
	Hash        string // first 12 hex chars of SHA-256(Body); doubles as the ETag value
	HashedName  string // content-addressed file name, e.g. "dashboard.ab12cd34ef56.css"
	ContentType string
}

// CSS and JS are the dashboard's static assets; Index is index.html with
// its asset references rewritten to the content-addressed names.
var (
	CSS   Asset
	JS    Asset
	Index []byte
)

func init() {
	CSS = load("dashboard.css", "text/css; charset=utf-8")
	JS = load("dashboard.js", "text/javascript; charset=utf-8")
	idx := mustRead("index.html")
	// Dumb-but-sufficient rewrite: the quoted literals appear exactly once
	// each (the <link href> and <script src>); quotes keep prose mentions
	// of the file names in comments untouched.
	idx = bytes.ReplaceAll(idx, []byte(`"dashboard.css"`), []byte(`"`+CSS.HashedName+`"`))
	idx = bytes.ReplaceAll(idx, []byte(`"dashboard.js"`), []byte(`"`+JS.HashedName+`"`))
	Index = idx
}

func load(name, contentType string) Asset {
	body := mustRead(name)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])[:12]
	base, ext, _ := bytes.Cut([]byte(name), []byte("."))
	return Asset{
		Body:        body,
		Hash:        hash,
		HashedName:  string(base) + "." + hash + "." + string(ext),
		ContentType: contentType,
	}
}

// mustRead returns an embedded asset. The files are baked in at compile
// time, so a read can only fail if the embed directive and these names
// drift — a programmer error worth failing fast on.
func mustRead(name string) []byte {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		panic("dashboard: missing embedded asset " + name + ": " + err.Error())
	}
	return b
}
