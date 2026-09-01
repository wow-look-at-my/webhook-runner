// References inside a settings document: `${env:NAME}` and
// `${settings:a.b[].c}`.
//
// Why both, and why they resolve at DIFFERENT times:
//
// - `${settings:...}` names another value in the SAME document. It depends on
// nothing outside the manifest, so it resolves at LOAD, before the schema
// runs -- the schema then validates real values, not reference text, and a
// typo'd path is a load error rather than a surprise at run time.
// - `${env:NAME}` names a variable on the runner host (the entity's
// sops secrets , then the host environment). Neither exists at
// validation time -- `validate` in CI must never read the runner's
// environment -- so it resolves when the container is about to start, and
// an unresolvable FAILS THE RUN. It is never quietly replaced with an
// empty string: that is precisely how a hook comes up looking healthy with
// no credential and fails downstream instead.
//
// The consequence for schema authors, stated plainly because it is the
// surprising part: a field you intend to fill with `${env:...}` is validated
// AT LOAD against the reference text, since the value does not exist yet. Model
// it so both forms pass, e.g.
//
//	{"anyOf": [{"pattern": "^sst_[A-Za-z-_-]+$"}, {"pattern": "^\\$\\{env:"}]}
//
// There is deliberately no hidden exemption for referenced fields: a schema
// that silently stopped applying to some values would be worse than that
// makes the author say what it accepts.
//
// TYPE PRESERVATION: a string that is EXACTLY `${settings:...}` reference
// takes the referenced value's own type -- `"${settings:limits.max}"` pointing
// at the number yields , not "". A reference embedded in surrounding text
// stringifies, because the result is text either way.
package hooks

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// refPattern matches reference: ${<kind>:<body>}.
var refPattern = regexp.MustCompile(`\$\{([a-zA-Z]+):([^}]*)\}`)

// malformedRefPattern catches an opening `${` whose reference never closes, so a truncated reference is a load error rather than a literal.
var malformedRefPattern = regexp.MustCompile(`\$\{[^}]*$`)

// Reference kinds.
const (
	refKindEnv      = "env"
	refKindSettings = "settings"
)

// maxRefDepth bounds `${settings:...}` chains (a reference to a reference). Reaching it means a cycle the resolver did not otherwise catch.
const maxRefDepth = 10

// ExpandSettingsSelfRefs resolves every `${settings:path}` in the document
// against the document itself, and reports any malformed or unknown-kind
// reference. `${env:...}` references are left untouched for the run path.
func ExpandSettingsSelfRefs(doc []byte) ([]byte, error) {
	var root any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("settings is not valid JSON: %w", err)
	}
	out, err := walkSettings(root, func(s string) (any, error) {
		return expandOne(s, root, 0)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// ExpandSettingsEnvRefs resolves every `${env:NAME}` using lookup, which is the
// run path's secrets-then-host-environment resolution. An unresolvable name is
// an error: the run fails loudly instead of starting with an empty credential.
func ExpandSettingsEnvRefs(doc []byte, lookup func(string) (string, bool)) ([]byte, error) {
	var root any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("settings is not valid JSON: %w", err)
	}
	out, err := walkSettings(root, func(s string) (any, error) {
		return expandEnvIn(s, lookup)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// walkSettings rebuilds the document, applying fn to every string leaf.
func walkSettings(node any, fn func(string) (any, error)) (any, error) {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			nc, err := walkSettings(child, fn)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = nc
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			nc, err := walkSettings(child, fn)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = nc
		}
		return out, nil
	case string:
		return fn(v)
	default:
		return node, nil
	}
}

// expandOne resolves the `${settings:...}` references in string, leaving
// `${env:...}` alone and rejecting anything malformed.
func expandOne(s string, root any, depth int) (any, error) {
	if depth > maxRefDepth {
		return nil, fmt.Errorf("reference chain too deep (>%d): a ${settings:...} cycle", maxRefDepth)
	}
	if loc := malformedRefPattern.FindString(s); loc != "" {
		return nil, fmt.Errorf("unterminated reference %q: expected ${env:NAME} or ${settings:path}", loc)
	}
	matches := refPattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}

	// A string that is EXACTLY settings reference adopts the referenced
	// value's type, so a number stays a number.
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(s) {
		kind, body := s[matches[0][2]:matches[0][3]], s[matches[0][4]:matches[0][5]]
		if kind == refKindSettings {
			val, err := lookupSettingsPath(root, body)
			if err != nil {
				return nil, err
			}
			if str, ok := val.(string); ok {
				return expandOne(str, root, depth+1)
			}
			return val, nil
		}
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		kind, body := s[m[2]:m[3]], s[m[4]:m[5]]
		b.WriteString(s[last:m[0]])
		switch kind {
		case refKindEnv:
			b.WriteString(s[m[0]:m[1]]) // resolved on the run path
		case refKindSettings:
			val, err := lookupSettingsPath(root, body)
			if err != nil {
				return nil, err
			}
			text, err := stringifyRef(val, body)
			if err != nil {
				return nil, err
			}
			nested, err := expandOne(text, root, depth+1)
			if err != nil {
				return nil, err
			}
			text, _ = nested.(string)
			b.WriteString(text)
		default:
			return nil, fmt.Errorf("unknown reference kind %q in %q: expected env or settings", kind, s[m[0]:m[1]])
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// expandEnvIn resolves `${env:NAME}` in string. Same type-preservation
// rule does not apply: an environment variable is always text.
func expandEnvIn(s string, lookup func(string) (string, bool)) (any, error) {
	matches := refPattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		kind, body := s[m[2]:m[3]], s[m[4]:m[5]]
		b.WriteString(s[last:m[0]])
		if kind != refKindEnv {
			// A settings reference that survived load is a bug in the load path, not something to paper over here.
			return nil, fmt.Errorf("unresolved ${%s:%s} reached the run path", kind, body)
		}
		val, ok := lookup(body)
		if !ok {
			return nil, fmt.Errorf("${env:%s} does not resolve: set it in the entity's secrets.sops.env or the runner host's environment", body)
		}
		b.WriteString(val)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// stringifyRef renders a referenced value for interpolation into surrounding
// text. Objects and arrays have no sensible text form, so they are an error
// rather than a JSON blob nobody meant to embed.
func stringifyRef(val any, path string) (string, error) {
	switch v := val.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case nil:
		return "", fmt.Errorf("${settings:%s} is null", path)
	default:
		return "", fmt.Errorf("${settings:%s} is an object or array, which has no text form; reference a scalar", path)
	}
}

// pathSegment splits `a.b[].c` into its walkable parts.
var pathSegment = regexp.MustCompile(`\[(\d+)\]|([^.\[\]]+)`)

// lookupSettingsPath walks a dotted path with optional [n] indexing.
func lookupSettingsPath(root any, path string) (any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("${settings:} needs a path, e.g. ${settings:secret_server.url}")
	}
	cur := root
	for _, seg := range pathSegment.FindAllStringSubmatch(path, -1) {
		switch {
		case seg[1] != "": // [n]
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("${settings:%s}: %q indexes something that is not an array", path, seg[0])
			}
			i, _ := strconv.Atoi(seg[1])
			if i < 0 || i >= len(arr) {
				return nil, fmt.Errorf("${settings:%s}: index %d is out of range (length %d)", path, i, len(arr))
			}
			cur = arr[i]
		default: // key
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("${settings:%s}: %q is not a key of an object", path, seg[2])
			}
			next, exists := obj[seg[2]]
			if !exists {
				return nil, fmt.Errorf("${settings:%s}: no such setting", path)
			}
			cur = next
		}
	}
	return cur, nil
}
