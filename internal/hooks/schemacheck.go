package hooks

import (
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wow-look-at-my/json-validator/validator"

	"github.com/wow-look-at-my/webhook-runner/schema"
)

// Every manifest is validated against the PUBLISHED schema at load, by the
// same implementation the hooks repo's CI runs: wow-look-at-my/json-validator.
//
// Why it exists: the Go decode is not the contract. `DisallowUnknownFields`
// plus hand-written checks catch a lot, but the schema knows things the struct
// cannot express -- enum values, string patterns, minimums, required
// combinations, format -- and the two drifted silently by construction. A
// hooks repo could pass CI and still be rejected at runtime, or worse, load
// with a value the published contract calls invalid.
//
// Two properties are deliberate:
//   - ONE implementation. json-validator is the CI gate; importing it (rather
//     than re-implementing the same checks against the same jsonschema
//     library) is what keeps "valid in CI" and "loads at runtime" the same
//     sentence, including its JSONC handling and its format assertions.
//   - The EMBEDDED schema, never the network (see package schema): a reload
//     must not depend on a fetch, and the binary's own contract is what it can
//     honestly enforce.

// The compile is done once per process: the schema is a constant, and a
// hooks-repo reload validates every manifest again.
var (
	hookSchemaOnce    sync.Once
	hookSchemaVal     *jsonschema.Schema
	hookSchemaErr     error
	managerSchemaOnce sync.Once
	managerSchemaVal  *jsonschema.Schema
	managerSchemaErr  error
)

// Names the embedded documents are registered under; they are compiler
// resource ids, not URLs to fetch.
const (
	hookSchemaID    = "embedded:hook.schema.json"
	managerSchemaID = "embedded:manager.schema.json"
)

// The CLI defaults this flag to "2020"; a zero Options does not, so it is
// spelled out here rather than inherited from a struct default that is really
// a flag default. Format assertions stay ON (json-validator's own deviation
// from the spec default) -- a `format: uri` in the schema means the manifest's
// value must actually be one.
func schemaOptions() validator.Options { return validator.Options{Draft: "2020"} }

func compileEmbedded(id string, raw []byte) (*jsonschema.Schema, error) {
	c, err := validator.NewCompiler(schemaOptions())
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("embedded %s is not valid JSON: %w", id, err)
	}
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("embedded %s is not a valid JSON Schema: %w", id, err)
	}
	return validator.CompileSchema(c, id)
}

func hookSchema() (*jsonschema.Schema, error) {
	hookSchemaOnce.Do(func() { hookSchemaVal, hookSchemaErr = compileEmbedded(hookSchemaID, schema.Hook) })
	return hookSchemaVal, hookSchemaErr
}

func managerSchema() (*jsonschema.Schema, error) {
	managerSchemaOnce.Do(func() { managerSchemaVal, managerSchemaErr = compileEmbedded(managerSchemaID, schema.Manager) })
	return managerSchemaVal, managerSchemaErr
}

// validateAgainstSchema checks one manifest's raw bytes (JSONC allowed, as
// everywhere else) against the embedded published schema. A failure is a LOAD
// error like any other: the entity is dropped, never loaded on the hope that
// the Go decode is close enough.
func validateAgainstSchema(sch *jsonschema.Schema, err error, filename string, raw []byte) error {
	if err != nil {
		// A broken embedded schema is our bug, and it must not silently
		// downgrade into "no schema validation" for the whole fleet.
		return fmt.Errorf("compiling the embedded schema: %w", err)
	}
	res := validator.Validate(bytes.NewReader(raw), filename, sch, schemaOptions())
	if res.Err != nil {
		return res.Err
	}
	if !res.Valid {
		return fmt.Errorf("does not match the published schema: %s", schemaErrorDetail(res.Error))
	}
	return nil
}

// schemaErrorDetail renders a validation error as the one-line, actionable
// form a load error needs: the failing location and its cause, not the full
// nested tree the CLI prints.
func schemaErrorDetail(e *jsonschema.ValidationError) string {
	if e == nil {
		return "unknown validation error"
	}
	var lines []string
	var walk func(*jsonschema.ValidationError)
	walk = func(cur *jsonschema.ValidationError) {
		if len(cur.Causes) == 0 {
			loc := strings.Join(cur.InstanceLocation, "/")
			if loc == "" {
				loc = "(root)"
			}
			lines = append(lines, fmt.Sprintf("%s: %v", loc, cur.ErrorKind))
			return
		}
		for _, c := range cur.Causes {
			walk(c)
		}
	}
	walk(e)
	if len(lines) == 0 {
		return e.Error()
	}
	return strings.Join(lines, "; ")
}

// ValidateHookJSON validates a hook.json document against the embedded schema.
func ValidateHookJSON(filename string, raw []byte) error {
	sch, err := hookSchema()
	return validateAgainstSchema(sch, err, filename, raw)
}

// ValidateManagerJSON validates a manager.json document against the embedded schema.
func ValidateManagerJSON(filename string, raw []byte) error {
	sch, err := managerSchema()
	return validateAgainstSchema(sch, err, filename, raw)
}
