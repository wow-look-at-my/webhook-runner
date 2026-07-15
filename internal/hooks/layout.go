package hooks

// Layout: where a hooks tree keeps its pieces. Two shapes exist, detected —
// never configured — by one rule, applied identically in serve, validate,
// and test:
//
//	LEGACY              <root>/<id>/hook.json, concurrency.json at
//	                    <root>/concurrency.json, docker build context =
//	                    the hook's own directory. Exactly today's shape.
//	SRC (SDK layout)    <root>/src/hooks/ EXISTS → hooks at
//	                    <root>/src/hooks/<id>/hook.json, shared
//	                    dependency-free code at <root>/src/sdk/ (imported
//	                    relatively — ../../sdk/... resolves identically
//	                    in-repo and in-image), concurrency config at
//	                    <root>/cfg/concurrency.json (at the repo root —
//	                    repo-wide config, not source), and docker
//	                    build context = <root>/src with the hook's own
//	                    Dockerfile (-f). Hook IDs, routes, api_keys, and
//	                    KV namespaces are unchanged — a pure relocation.
//
// The layouts are never mixed: under the src layout, a root-level hook
// dir is a HARD ERROR (IgnoredLegacyDirError in the loader) — not loaded,
// and loud enough to fail `validate` and every `serve` reload, so a stray
// top-level hook left by an incomplete move can never silently vanish.
// The guard fires only when src/hooks/ exists, so pure-legacy trees are
// unaffected.
import (
	"os"
	"path/filepath"
)

// Layout is the detected shape of a hooks root.
type Layout struct {
	// Root is the hooks root as given to the loader/CLI.
	Root string
	// SDK is true for the src/ layout (Root/src/hooks exists).
	SDK bool
}

// DetectLayout applies the one detection rule: <root>/src/hooks being a
// directory selects the src layout; anything else is legacy.
func DetectLayout(root string) Layout {
	fi, err := os.Stat(filepath.Join(root, "src", "hooks"))
	if err == nil && fi.IsDir() {
		return Layout{Root: root, SDK: true}
	}
	return Layout{Root: root}
}

// String names the layout for logs and error messages.
func (l Layout) String() string {
	if l.SDK {
		return "src"
	}
	return "legacy"
}

// HooksDir is the directory whose immediate children are scanned for
// hook.json folders.
func (l Layout) HooksDir() string {
	if l.SDK {
		return filepath.Join(l.Root, "src", "hooks")
	}
	return l.Root
}

// SrcDir is the docker build context for src-layout hooks (<root>/src), or
// "" under the legacy layout (each hook builds from its own directory).
func (l Layout) SrcDir() string {
	if l.SDK {
		return filepath.Join(l.Root, "src")
	}
	return ""
}

// SDKDir is the shared-code directory hashed into every src-layout hook's
// content tag (<root>/src/sdk), or "" under the legacy layout. It need not
// exist — a src tree without shared code is fine.
func (l Layout) SDKDir() string {
	if l.SDK {
		return filepath.Join(l.Root, "src", "sdk")
	}
	return ""
}

// ConcurrencyPath is where the central concurrency-groups file lives for
// this layout.
func (l Layout) ConcurrencyPath() string {
	if l.SDK {
		return filepath.Join(l.Root, "cfg", "concurrency.json")
	}
	return filepath.Join(l.Root, "concurrency.json")
}
