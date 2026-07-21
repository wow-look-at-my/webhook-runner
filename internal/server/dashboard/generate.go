// This file wires regeneration of the embedded timeline bundle into
// `go generate`. The bundle assets/timeline.js is generated from the TypeScript
// adapter in ts/ by ts0 (config in ts0.json); the generator itself lives in
// gen.go (a build-ignored program that fetches a pinned, prebuilt ts0 from the
// org buildhost and runs it with the local Node.js runtime — no npm, npx,
// node_modules, or git). The committed bundle stays the source of truth for
// go:embed; a normal build never runs this.
//
// Regenerate after editing ts/ with `go generate ./internal/server/dashboard/`
// and commit the updated assets/timeline.js. In CI, go-toolchain runs this
// through its approved generate step (ci.yml's `generate:` hash) and then fails
// on a dirty working tree, so a stale committed bundle turns CI red.
package dashboard

//go:generate go run gen.go
