package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Declarative skip conditions ("skip_if").
//
// A hook may declare conditions under which a delivery is SKIPPED: answered
// immediately, recorded as a first-class run with status "skipped", and never
// given a container — no image build, no concurrency slot, no docker run. The
// motivating case is a webhook source that can't be narrowed at the sender
// (GitHub's event checkboxes bundle events you want with events you don't):
// the unwanted events used to boot a container just to exit, and "nothing
// happened" was invisible.
//
// The matcher is deliberately NOT a language. It is a total, bounded,
// declarative structure evaluated by this process outside any container, so
// it must be provably safe: string comparisons over stringified JSON leaves
// and header values, plus Go's regexp — RE2, guaranteed linear time, no
// backtracking — compiled at load time. No jq, no CEL, no expressions, no
// user code. Every evaluation terminates in time bounded by the (already
// size-capped) payload.
//
// Semantics:
//   - The skip_if LIST is ORed: the first condition that matches skips.
//   - KEYS within one condition are ANDed: every key must match (the same
//     convention as pr-minder's auto_update_pr.triggers).
//   - A key addresses the parsed JSON payload via a dotted path ("action",
//     "workflow_run.conclusion", "commits.0.message" — object fields by
//     name, array elements by numeric index), or a request HEADER via the
//     "header:" prefix ("header:x-github-event", name case-insensitive).
//   - Values are compared as strings. Leaves stringify predictably: strings
//     as themselves, numbers as their JSON literal text, true/false/null as
//     those exact words. Objects and arrays are NOT leaves — a path landing
//     on one simply doesn't match (only `exists` can see it).
//   - Evaluation is total and fails TOWARD DOING THE WORK: a missing path,
//     a non-leaf value, or an unparseable payload just means that key does
//     not match, so the run happens. Skipping is never the failure mode.

// HeaderKeyPrefix marks a skip_if condition key as addressing a request
// header instead of a payload field: "header:x-github-event". The header
// name is case-insensitive; multi-valued headers match on the first value.
const HeaderKeyPrefix = "header:"

// SkipConditions is the hook.json "skip_if" list. Entries are ORed.
type SkipConditions []SkipCondition

// SkipCondition maps payload paths / header keys to matchers. All keys must
// match (AND) for the condition to match.
type SkipCondition map[string]*SkipMatcher

// SkipMatcher is one condition key's test. In hook.json it is either a bare
// string (shorthand for {"eq": ...}) or an object naming one or more
// operators; when several operators are set they must ALL accept (AND).
type SkipMatcher struct {
	// Eq matches a leaf whose stringified value equals this exactly.
	Eq *string
	// Ne matches a leaf whose stringified value differs. A missing path
	// does NOT match Ne — absence is not inequality (fail toward work).
	Ne *string
	// In matches a leaf whose stringified value equals any listed value.
	In []string
	// Exists tests resolution itself: true matches when the path resolves
	// to ANY value (object, array, or leaf — for headers: the header is
	// present); false matches when it does not.
	Exists *bool
	// Prefix matches a leaf whose stringified value starts with this.
	Prefix *string
	// Regex matches a leaf whose stringified value contains a match of this
	// pattern (anchor with ^ and $ for a full match). Go regexp is RE2:
	// linear-time, no backtracking — which is the only reason a
	// caller-supplied pattern is safe to run outside the container. Compiled
	// at load time; a pattern that doesn't compile is a validation error.
	Regex *string

	// re is the pattern compiled by compile() at load/validation time.
	re *regexp.Regexp
}

// skipOps names the accepted matcher operators, for error messages.
const skipOps = "eq, ne, in, exists, prefix, regex"

// UnmarshalJSON accepts the bare-string equality shorthand or the operator
// object. Unknown operator keys are rejected here (the hook-level decoder's
// DisallowUnknownFields does not reach into custom unmarshalers).
func (m *SkipMatcher) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		m.Eq = &s
		return nil
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("matcher must be a string (equality shorthand) or an object of operators (" + skipOps + ")")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		var err error
		switch k {
		case "eq":
			err = unmarshalOp(v, &m.Eq)
		case "ne":
			err = unmarshalOp(v, &m.Ne)
		case "in":
			err = json.Unmarshal(v, &m.In)
		case "exists":
			err = unmarshalOp(v, &m.Exists)
		case "prefix":
			err = unmarshalOp(v, &m.Prefix)
		case "regex":
			err = unmarshalOp(v, &m.Regex)
		default:
			return fmt.Errorf("unknown operator %q (valid: %s)", k, skipOps)
		}
		if err != nil {
			return fmt.Errorf("operator %q: %w", k, err)
		}
	}
	return nil
}

func unmarshalOp[T any](data json.RawMessage, dst **T) error {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*dst = &v
	return nil
}

// MarshalJSON round-trips the matcher: the shorthand string when only Eq is
// set, the operator object otherwise.
func (m *SkipMatcher) MarshalJSON() ([]byte, error) {
	if m.Eq != nil && m.Ne == nil && m.In == nil && m.Exists == nil && m.Prefix == nil && m.Regex == nil {
		return json.Marshal(*m.Eq)
	}
	obj := map[string]any{}
	if m.Eq != nil {
		obj["eq"] = *m.Eq
	}
	if m.Ne != nil {
		obj["ne"] = *m.Ne
	}
	if m.In != nil {
		obj["in"] = m.In
	}
	if m.Exists != nil {
		obj["exists"] = *m.Exists
	}
	if m.Prefix != nil {
		obj["prefix"] = *m.Prefix
	}
	if m.Regex != nil {
		obj["regex"] = *m.Regex
	}
	return json.Marshal(obj)
}

// compile validates the conditions and compiles every regex, once, at load
// time. Malformed skip_if — an empty condition, an empty matcher, a bad
// regex, an empty in-set — fails the hook's load/validation (the hook is
// dropped), the same fail-closed rule as an undeclared concurrency group.
func (cs SkipConditions) compile() error {
	for i, cond := range cs {
		if len(cond) == 0 {
			return fmt.Errorf("skip_if[%d]: condition must set at least one payload path or header: key", i)
		}
		for key, m := range cond {
			if err := validateSkipKey(key); err != nil {
				return fmt.Errorf("skip_if[%d] key %q: %w", i, key, err)
			}
			if m == nil {
				return fmt.Errorf("skip_if[%d] key %q: matcher must not be null", i, key)
			}
			if err := m.compile(); err != nil {
				return fmt.Errorf("skip_if[%d] key %q: %w", i, key, err)
			}
		}
	}
	return nil
}

func validateSkipKey(key string) error {
	if key == "" {
		return errors.New("key must not be empty")
	}
	if name, ok := strings.CutPrefix(key, HeaderKeyPrefix); ok && strings.TrimSpace(name) == "" {
		return errors.New("header: key must name a header")
	}
	return nil
}

func (m *SkipMatcher) compile() error {
	if m.Eq == nil && m.Ne == nil && m.In == nil && m.Exists == nil && m.Prefix == nil && m.Regex == nil {
		return errors.New("matcher must set at least one operator (" + skipOps + ")")
	}
	if m.In != nil && len(m.In) == 0 {
		return errors.New(`"in" must list at least one value`)
	}
	if m.Regex != nil {
		re, err := regexp.Compile(*m.Regex)
		if err != nil {
			return fmt.Errorf("invalid regex %q: %w", *m.Regex, err)
		}
		m.re = re
	}
	return nil
}

// EvaluateSkip reports whether this delivery matches one of the hook's
// skip_if conditions. On a match it returns a rendered reason naming the
// condition, e.g.
//
//	skip_if[0]: header x-github-event == "workflow_run"
//
// Callers must evaluate this only AFTER the request authenticated —
// unauthenticated requests must never probe skip conditions — and a match
// means the run pipeline is bypassed entirely (see runner.Skip).
//
// The payload JSON is parsed at most once, and only when some condition
// actually addresses a payload path — the motivating header-only case never
// parses the body at all.
func (h *Hook) EvaluateSkip(payload []byte, header http.Header) (reason string, matched bool) {
	if len(h.SkipIf) == 0 {
		return "", false
	}
	var root any
	parsed := false
	payloadTree := func() any {
		if !parsed {
			parsed = true
			root = parsePayloadTree(payload)
		}
		return root
	}
	for i, cond := range h.SkipIf {
		if cond.matches(payloadTree, header) {
			return fmt.Sprintf("skip_if[%d]: %s", i, cond.render()), true
		}
	}
	return "", false
}

// matches reports whether every key of the condition matches (AND).
func (c SkipCondition) matches(payloadTree func() any, header http.Header) bool {
	for key, m := range c {
		if m == nil {
			// Only reachable on a hook built in code without validation;
			// fail toward doing the work.
			return false
		}
		var leaf string
		var isLeaf, exists bool
		if name, ok := strings.CutPrefix(key, HeaderKeyPrefix); ok {
			name = strings.TrimSpace(name)
			// http.Header lookups are canonicalized, so the condition's
			// header name is case-insensitive. Multi-valued headers match
			// on the first value.
			vals := header.Values(name)
			if exists = len(vals) > 0; exists {
				leaf, isLeaf = vals[0], true
			}
		} else {
			node, ok := resolvePath(payloadTree(), key)
			if exists = ok; exists {
				leaf, isLeaf = leafString(node)
			}
		}
		if !m.matches(leaf, isLeaf, exists) {
			return false
		}
	}
	return true
}

// matches applies every set operator (AND). Value operators require the path
// to have resolved to a stringifiable leaf; when it didn't, they do not
// match — the total-evaluation rule that fails toward doing the work.
func (m *SkipMatcher) matches(leaf string, isLeaf, exists bool) bool {
	if m.Exists != nil && *m.Exists != exists {
		return false
	}
	if m.Eq != nil && (!isLeaf || leaf != *m.Eq) {
		return false
	}
	if m.Ne != nil && (!isLeaf || leaf == *m.Ne) {
		return false
	}
	if m.In != nil && (!isLeaf || !slices.Contains(m.In, leaf)) {
		return false
	}
	if m.Prefix != nil && (!isLeaf || !strings.HasPrefix(leaf, *m.Prefix)) {
		return false
	}
	if m.Regex != nil {
		if !isLeaf {
			return false
		}
		re := m.re
		if re == nil {
			// Normal loads compile at validation time; this fallback covers
			// hooks constructed in code. A non-compiling pattern matches
			// nothing (work happens).
			var err error
			if re, err = regexp.Compile(*m.Regex); err != nil {
				return false
			}
		}
		if !re.MatchString(leaf) {
			return false
		}
	}
	return true
}

// parsePayloadTree decodes the payload for path lookups. UseNumber keeps
// numeric leaves as their exact JSON literal text (no float mangling), which
// is what makes number stringification predictable. A payload that isn't
// JSON yields nil: every payload path is then unresolved, no condition keyed
// on one matches, and the work happens.
func parsePayloadTree(payload []byte) any {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil
	}
	return root
}

// resolvePath walks a dotted path: object fields by name, array elements by
// numeric index. Anything unresolvable — absent field, out-of-range or
// non-numeric index, walking through a scalar — reports not-found.
func resolvePath(root any, path string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// leafString stringifies a scalar leaf: strings as themselves, numbers as
// their JSON literal text (json.Number), booleans as "true"/"false", JSON
// null as "null". Objects and arrays are not leaves.
func leafString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case nil:
		return "null", true
	default:
		return "", false
	}
}

// render describes the condition for the skip reason (run output, events,
// HTTP response): keys sorted for determinism, clauses joined with "and"
// because they are ANDed. Header keys render as `header <name>`.
func (c SkipCondition) render() string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		display := k
		if name, ok := strings.CutPrefix(k, HeaderKeyPrefix); ok {
			display = "header " + strings.TrimSpace(name)
		}
		parts = append(parts, c[k].render(display)...)
	}
	return strings.Join(parts, " and ")
}

// render emits one clause per set operator, in a fixed order.
func (m *SkipMatcher) render(key string) []string {
	var parts []string
	if m == nil {
		return parts
	}
	if m.Eq != nil {
		parts = append(parts, fmt.Sprintf("%s == %q", key, *m.Eq))
	}
	if m.Ne != nil {
		parts = append(parts, fmt.Sprintf("%s != %q", key, *m.Ne))
	}
	if m.In != nil {
		quoted := make([]string, len(m.In))
		for i, v := range m.In {
			quoted[i] = strconv.Quote(v)
		}
		parts = append(parts, fmt.Sprintf("%s in [%s]", key, strings.Join(quoted, ", ")))
	}
	if m.Exists != nil {
		if *m.Exists {
			parts = append(parts, key+" exists")
		} else {
			parts = append(parts, key+" is absent")
		}
	}
	if m.Prefix != nil {
		parts = append(parts, fmt.Sprintf("%s starts with %q", key, *m.Prefix))
	}
	if m.Regex != nil {
		parts = append(parts, fmt.Sprintf("%s matches %q", key, *m.Regex))
	}
	return parts
}
