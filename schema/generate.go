package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// hook.schema.json and manager.schema.json are GENERATED from src/, and the
// reason is a bug this repo already shipped: the two documents declared 24 of
// the same properties, hand-copied, and drifted. The manager's `timeout` lost
// the hook's `^[0-9]+(ns|us|ms|s|m|h)+$` pattern, so "banana" validated;
// api_key_header and enable lost their defaults; run_title lost its examples.
// Every one of those was a copy that stopped matching its twin, silently.
//
// The shared CONSTRAINTS therefore live once, in src/common.json, and each
// published document carries them as a single `$defs.common` block it $refs --
// composed, not merged, so the shared block is visible as one object in the
// output instead of dissolved into 24 properties nobody can tell apart from
// the entity's own.
//
// Two things about that composition are load-bearing:
//
//   - `unevaluatedProperties: false`, NEVER `additionalProperties: false`.
//     additionalProperties does not compose through allOf: the $ref'd common
//     block is evaluated on its own, sees the entity's own `schedule` /
//     `spawn_targets`, and rejects a perfectly valid manifest.
//     unevaluatedProperties is the 2020-12 keyword that accounts for what
//     sibling subschemas evaluated. buildOverlay rejects an overlay that
//     brings additionalProperties back.
//   - The prose stays PER-ENTITY. All 24 descriptions differ, and not just in
//     wording -- a manager's concurrency_group slot is held for the instance's
//     whole lifetime, its run_title placeholders always resolve empty, its
//     skip_if creates no run record. Sharing one neutral sentence would delete
//     that. So common.json holds constraints only and each overlay supplies a
//     description-only property entry, which annotates without weakening
//     anything the $ref'd block constrains.
//
// Generated rather than $ref'd across files because the published schemas are
// fetched by editors and by the hooks repo's CI at a canonical URL, and a
// cross-file $ref would make every consumer fetch a second document to
// validate one manifest -- json-validator does not resolve one at all.

const commonRef = "#/$defs/common"

// overlay is an entity's src/<entity>.json: every top-level schema keyword
// except the shared properties, plus this entity's prose for them.
type overlay struct {
	Descriptions map[string]string          `json:"descriptions"`
	Properties   map[string]json.RawMessage `json:"properties"`
}

// Generate assembles one published schema from the shared base and an entity
// overlay. base and doc are the raw src/common.json and src/<entity>.json.
func Generate(base, doc []byte) ([]byte, error) {
	var common map[string]json.RawMessage
	if err := json.Unmarshal(base, &common); err != nil {
		return nil, fmt.Errorf("common.json: %w", err)
	}
	var ov overlay
	if err := json.Unmarshal(doc, &ov); err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}
	out, err := buildOverlay(doc)
	if err != nil {
		return nil, err
	}

	props, err := properties(common, ov)
	if err != nil {
		return nil, err
	}
	defs, err := sharedDefs(common)
	if err != nil {
		return nil, err
	}
	allOf, err := composeAllOf(out["allOf"])
	if err != nil {
		return nil, err
	}

	out["$defs"] = defs
	out["allOf"] = allOf
	out["properties"] = props
	out["unevaluatedProperties"] = json.RawMessage("false")

	encoded, err := encodeObject(out)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// buildOverlay strips the keys Generate owns and refuses the one keyword that
// would silently break composition.
func buildOverlay(doc []byte) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}
	if _, bad := out["additionalProperties"]; bad {
		return nil, fmt.Errorf(
			"overlay sets additionalProperties, which does not compose through allOf " +
				"(the shared block would reject this entity's own properties); " +
				"unevaluatedProperties is applied automatically")
	}
	if _, bad := out["$defs"]; bad {
		return nil, fmt.Errorf("overlay sets $defs, which Generate owns")
	}
	delete(out, "descriptions")
	delete(out, "properties")
	delete(out, "unevaluatedProperties")
	return out, nil
}

// sharedDefs wraps the shared constraints as the one $defs.common subschema
// both documents $ref.
func sharedDefs(common map[string]json.RawMessage) (json.RawMessage, error) {
	encoded, err := encodeObject(common)
	if err != nil {
		return nil, err
	}
	block, err := encodeObject(map[string]json.RawMessage{
		"type":       json.RawMessage(`"object"`),
		"properties": encoded,
	})
	if err != nil {
		return nil, err
	}
	return encodeObject(map[string]json.RawMessage{"common": block})
}

// composeAllOf puts the shared block first, keeping any allOf the entity
// already declared (the manager's mutually-exclusive auth rules).
func composeAllOf(existing json.RawMessage) (json.RawMessage, error) {
	branches := []json.RawMessage{json.RawMessage(`{"$ref": "` + commonRef + `"}`)}
	if len(existing) > 0 {
		var declared []json.RawMessage
		if err := json.Unmarshal(existing, &declared); err != nil {
			return nil, fmt.Errorf("overlay allOf: %w", err)
		}
		branches = append(branches, declared...)
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, b := range branches {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(b)
	}
	buf.WriteByte(']')

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return pretty.Bytes(), nil
}

// properties emits the entity's own properties plus a description-only entry
// per shared property: prose here, constraints in $defs.common.
func properties(common map[string]json.RawMessage, ov overlay) (json.RawMessage, error) {
	props := map[string]json.RawMessage{}
	for name, def := range ov.Properties {
		if _, clash := common[name]; clash {
			// An entity redefining a shared property is the twin coming
			// back: two definitions, one silently shadowing the other.
			return nil, fmt.Errorf("property %q is declared in both common.json and the overlay", name)
		}
		props[name] = def
	}
	for name := range common {
		desc, ok := ov.Descriptions[name]
		if !ok {
			return nil, fmt.Errorf("shared property %q has no description in this overlay", name)
		}
		encoded, err := json.Marshal(map[string]string{"description": desc})
		if err != nil {
			return nil, err
		}
		props[name] = encoded
	}
	for name := range ov.Descriptions {
		if _, ok := common[name]; !ok {
			return nil, fmt.Errorf("description for %q, which is not a shared property", name)
		}
	}
	return encodeObject(props)
}

// encodeObject writes an object with sorted keys and no HTML escaping, so the
// generated bytes are a pure function of the sources.
func encodeObject(fields map[string]json.RawMessage) ([]byte, error) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(fields[name])
	}
	buf.WriteByte('}')

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return pretty.Bytes(), nil
}
