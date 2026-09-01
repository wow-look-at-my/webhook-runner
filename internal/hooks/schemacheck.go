// Every manifest is validated against the PUBLISHED schema at load, by the
// same implementation the hooks repo's CI runs: wow-look-at-my/json-validator.
//
// Why it exists: the Go decode is not the contract. `DisallowUnknownFields`
// plus hand-written checks catch a lot, but the schema knows things the struct
// cannot express -- enums, patterns, minimums, required combinations, format --
// and the drifted silently by construction. A hooks repo could pass CI and
// still be rejected at runtime, or worse, load with a value the published
// contract calls invalid.
//
// properties are deliberate:
// - implementation. json-validator is the CI gate; importing it (rather
// than re-implementing the same checks against the same jsonschema
// library) is what keeps "valid in CI" and "loads at runtime" the same
// sentence, including its JSONC handling and its format assertions.
// - The EMBEDDED schema, never the network (see package schema): a reload
// must not depend on a fetch, and the binary's own contract is what it can
// honestly enforce.
package hooks

import (
	"fmt"
	"sync"

	"github.com/wow-look-at-my/json-validator/validator"

	"github.com/wow-look-at-my/webhook-runner/schema"
)

// Compiled per process: the schemas are constants, and every hooks-repo
// reload revalidates every manifest.
var (
	hookValidatorOnce    sync.Once
	hookValidator        *validator.Validator
	hookValidatorErr     error
	managerValidatorOnce sync.Once
	managerValidator     *validator.Validator
	managerValidatorErr  error
)

// Ids the embedded documents are registered under; they name the schema in error messages and resolve its internal $refs -- they are never.
const (
	hookSchemaID    = "embedded:hook.schema.json"
	managerSchemaID = "embedded:manager.schema.json"
)

func hookSchemaValidator() (*validator.Validator, error) {
	hookValidatorOnce.Do(func() {
		hookValidator, hookValidatorErr = validator.NewFromBytes(hookSchemaID, schema.Hook, validator.Options{})
	})
	return hookValidator, hookValidatorErr
}

func managerSchemaValidator() (*validator.Validator, error) {
	managerValidatorOnce.Do(func() {
		managerValidator, managerValidatorErr = validator.NewFromBytes(managerSchemaID, schema.Manager, validator.Options{})
	})
	return managerValidator, managerValidatorErr
}

func validateAgainstSchema(v *validator.Validator, compileErr error, filename string, raw []byte) error {
	if compileErr != nil {
		// A broken embedded schema is our bug, and it must not silently downgrade into "no schema validation" for the whole fleet.
		return fmt.Errorf("compiling the embedded schema: %w", compileErr)
	}
	res := v.ValidateBytes(raw, filename)
	if res.Err != nil {
		return res.Err
	}
	if !res.Valid {
		return fmt.Errorf("does not match the published schema: %s", res.Detail())
	}
	return nil
}

// ValidateHookJSON validates a hook.json document against the embedded schema.
func ValidateHookJSON(filename string, raw []byte) error {
	v, err := hookSchemaValidator()
	return validateAgainstSchema(v, err, filename, raw)
}

// ValidateManagerJSON validates a manager.json document against the embedded schema.
func ValidateManagerJSON(filename string, raw []byte) error {
	v, err := managerSchemaValidator()
	return validateAgainstSchema(v, err, filename, raw)
}
