// Package dashboard embeds the static HTML/JS for the simple read-only
// dashboard served at GET /.
//
// The CSS and JS are content-addressed: each is also reachable at
// /dashboard.<hash>.css|.js (hash = hex chars of the SHA- of
// the embedded bytes), and the served index.html references those hashed
// URLs. Hashed URLs are safe to cache forever (immutable); index.html and
// the bare asset paths must never be cached (no-cache), or an edge cache
// (Cloudflare caches .css/.js by extension when the origin sends no cache
// headers) can pair a new index.html with stale assets after a deploy —
// which is exactly the incident that motivated this.
//
// js-snippets components are imported by the BROWSER at runtime and are
// never shipped here: <timeline-view> (the runs chart) and <activity-feed>
// (both Activity feeds — the overview page and the per-hook section). Their
// import URLs point at js-snippets' buildhost library site, live at that
// repo's master head, so component fixes reach this dashboard with no runner
// change; fix component bugs upstream in js-snippets. <activity-feed> is
// loaded by a small inline module at the bottom of index.html (it needs
// nothing but the import, so it stays out of the ts bundle) and is fed by
// dashboard.js, which sets .entries on elements that have usually not
// upgraded yet — the component's connectedCallback replays them.
//
// assets/timeline.js is GENERATED — never edit it. Its TypeScript source
// lives in ts/ (the runs-timeline ADAPTER only — the <timeline-view>
// component itself is imported by the browser at runtime from
// js-snippets' buildhost library site, never shipped here) and is
// compiled by ts (type-check + bundle, config in ts.json; the component
// URL passes through unbundled) via the //go:generate below, which runs
// generate-timeline.sh (this directory, cwd = this package dir): curl a
// pinned ts build from buildhost, curl the component's published .d.ts
// pair from the library site into the committed ts/js-snippets/ (the types
// the adapter type-imports, instead of the live URL — see ts/timeline.ts),
// run `node ts.cjs build`. Needs curl and Node + —
// no npm, no npx, no git auth. The bundle and the fetched declarations are
// committed (a fresh clone builds and embeds without Node); CI regenerates
// both and fails on any diff, so the bundle can't go stale and an upstream
// component API change turns CI red instead of drifting. To bump ts:
// change ?v=N in generate-timeline.sh. The go-toolchain approval hash
// covers the directive line itself — it re-keys only when that line is
// edited or moved (a bare go-toolchain run prints the new ; update
// ci.yml's generate: input to match).
package dashboard

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

//go:generate sh generate-timeline.sh

//go:embed assets/*
var assets embed.FS

// Asset is embedded dashboard file plus its content identity.
type Asset struct {
	Body        []byte
	Hash        string // hex chars of SHA-(Body); doubles as the ETag value
	HashedName  string // content-addressed file name, e.g. "dashboard.abcdef.css"
	ContentType string
}

// CSS and JS are the dashboard's static assets; TimelineJS is the generated runs-timeline bundle (ts output, committed — see the package.
var (
	CSS        Asset
	JS         Asset
	TimelineJS Asset
	Index      []byte
)

func init() {
	CSS = load("dashboard.css", "text/css; charset=utf-8")
	JS = load("dashboard.js", "text/javascript; charset=utf-8")
	TimelineJS = load("timeline.js", "text/javascript; charset=utf-8")
	idx := mustRead("index.html")
	// Dumb-but-sufficient rewrite: the quoted literals appear exactly each (the <link href> and <script src>); quotes keep prose.
	idx = bytes.ReplaceAll(idx, []byte(`"dashboard.css"`), []byte(`"`+CSS.HashedName+`"`))
	idx = bytes.ReplaceAll(idx, []byte(`"dashboard.js"`), []byte(`"`+JS.HashedName+`"`))
	idx = bytes.ReplaceAll(idx, []byte(`"timeline.js"`), []byte(`"`+TimelineJS.HashedName+`"`))
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

// mustRead returns an embedded asset.
func mustRead(name string) []byte {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		panic("dashboard: missing embedded asset " + name + ": " + err.Error())
	}
	return b
}
