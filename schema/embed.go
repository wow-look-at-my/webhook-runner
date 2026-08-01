// Package schema holds the published JSON Schemas for hook.json,
// manager.json and concurrency.json -- and embeds them, so the binary
// validates manifests against the SAME contract it publishes.
//
// Embedded, not fetched: the loader runs on every reload and must not depend
// on the network, on whether sites.pazer.build is reachable, or on a hooks
// repo pinning some other `$schema` URL. A manifest's own `$schema` field
// still declares which published document it targets (CI validates against
// that), while the runtime gate uses the copy compiled into the binary it is
// running -- which is exactly what "deploy the runner first" means.
package schema

import _ "embed"

//go:embed hook.schema.json
var Hook []byte

//go:embed manager.schema.json
var Manager []byte

//go:embed concurrency.schema.json
var Concurrency []byte
