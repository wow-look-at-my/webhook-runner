// The embedded timeline bundle is generated, not written: edit ts/, then
// `go generate ./internal/server/dashboard/` and commit the rebuilt
// assets/timeline.js. CI runs the same directive and fails on a dirty tree.
//
// see docs/timeline-bundle-generation.md
package dashboard

//go:generate go run ./gen
