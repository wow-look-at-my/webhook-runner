package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Parsing + validation (fail closed at load) ----------------------------

func TestParseSkipIfValid(t *testing.T) {
	h, err := parseInDir(t, `{
		"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
		// entries ORed; keys within an entry ANDed
		"skip_if": [
			{ "header:x-github-event": "workflow_run" },
			{ "action": { "in": ["labeled", "unlabeled"] }, "sender.type": "Bot" },
			{ "ref": { "prefix": "refs/tags/", "ne": "refs/tags/latest" } },
			{ "workflow_run.conclusion": { "exists": false } },
			{ "repository.full_name": { "regex": "^wow-look-at-my/" } }
		]
	}`)
	require.NoError(t, err)
	require.Len(t, h.SkipIf, 5)
	require.NotNil(t, h.SkipIf[0]["header:x-github-event"].Eq)
	assert.Equal(t, "workflow_run", *h.SkipIf[0]["header:x-github-event"].Eq)
	assert.Equal(t, []string{"labeled", "unlabeled"}, h.SkipIf[1]["action"].In)
	assert.NotNil(t, h.SkipIf[2]["ref"].Prefix)
	assert.NotNil(t, h.SkipIf[2]["ref"].Ne)
	require.NotNil(t, h.SkipIf[3]["workflow_run.conclusion"].Exists)
	assert.False(t, *h.SkipIf[3]["workflow_run.conclusion"].Exists)
	// The regex compiled at load: evaluation never compiles per request.
	assert.NotNil(t, h.SkipIf[4]["repository.full_name"].re)
}

func TestParseSkipIfBadRegexIsValidationError(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"regex":"("}}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid regex")
	assert.Contains(t, err.Error(), `skip_if[0] key "x"`)
}

func TestParseSkipIfUnknownOperator(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"contains":"y"}}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown operator "contains"`)
}

func TestParseSkipIfEmptyCondition(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_if[0]")
	assert.Contains(t, err.Error(), "at least one")
}

func TestParseSkipIfEmptyMatcherObject(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{}}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one operator")
}

func TestParseSkipIfEmptyInSet(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"in":[]}}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"in" must list at least one value`)
}

func TestParseSkipIfMatcherWrongType(t *testing.T) {
	// A number is neither the string shorthand nor an operator object.
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":5}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matcher must be a string")
}

func TestParseSkipIfNullMatcher(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":null}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be null")
}

func TestParseSkipIfOperatorValueWrongType(t *testing.T) {
	for _, doc := range []string{
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"eq":5}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"exists":"yes"}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"in":"not-a-list"}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"in":[1,2]}}]}`,
		`{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"x":{"prefix":false}}]}`,
	} {
		_, err := parseInDir(t, doc)
		require.Error(t, err, "doc should fail: %s", doc)
	}
}

func TestParseSkipIfEmptyKey(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"":"x"}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key must not be empty")
}

func TestParseSkipIfHeaderKeyWithoutName(t *testing.T) {
	_, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":[{"header:":"x"}]}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must name a header")
}

func TestSkipMatcherMarshalRoundTrip(t *testing.T) {
	for _, doc := range []string{
		`"plain"`,
		`{"exists":false}`,
		`{"in":["a","b"],"prefix":"a"}`,
	} {
		var m SkipMatcher
		require.NoError(t, json.Unmarshal([]byte(doc), &m))
		out, err := json.Marshal(&m)
		require.NoError(t, err)
		var back SkipMatcher
		require.NoError(t, json.Unmarshal(out, &back))
		back.re = nil
		assert.Equal(t, m, back, "round trip of %s via %s", doc, out)
	}
}

// --- Evaluation -------------------------------------------------------------

// skipHook builds a validated hook straight from a skip_if JSON document, so
// evaluation tests exercise exactly what a hooks repo would load (regexes
// compiled, conditions validated).
func skipHook(t *testing.T, skipIf string) *Hook {
	t.Helper()
	h, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json","skip_if":`+skipIf+`}`)
	require.NoError(t, err)
	return h
}

func ghHeaders(event string) http.Header {
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("Content-Type", "application/json")
	return h
}

func TestEvaluateSkipHeaderEquality(t *testing.T) {
	// The motivating case: an event type you can't unsubscribe from.
	h := skipHook(t, `[{"header:x-github-event": "workflow_run"}]`)

	reason, matched := h.EvaluateSkip([]byte(`{"action":"completed"}`), ghHeaders("workflow_run"))
	require.True(t, matched)
	assert.Equal(t, `skip_if[0]: header x-github-event == "workflow_run"`, reason)

	_, matched = h.EvaluateSkip([]byte(`{"action":"opened"}`), ghHeaders("pull_request"))
	assert.False(t, matched)
}

func TestEvaluateSkipHeaderNameCaseInsensitive(t *testing.T) {
	for _, key := range []string{"header:x-github-event", "header:X-GitHub-Event", "header:X-GITHUB-EVENT"} {
		h := skipHook(t, fmt.Sprintf(`[{%q: "push"}]`, key))
		_, matched := h.EvaluateSkip(nil, ghHeaders("push"))
		assert.True(t, matched, "condition key %q must match", key)
	}
}

func TestEvaluateSkipMissingHeaderNeverMatchesValues(t *testing.T) {
	// A missing header is absence, not the empty string: eq "" and ne both refuse to match, so the work happens (fail toward work).
	h := skipHook(t, `[{"header:x-absent": ""}]`)
	_, matched := h.EvaluateSkip(nil, ghHeaders("push"))
	assert.False(t, matched)

	h = skipHook(t, `[{"header:x-absent": {"ne": "something"}}]`)
	_, matched = h.EvaluateSkip(nil, ghHeaders("push"))
	assert.False(t, matched)

	// exists:false is the way to match absence.
	h = skipHook(t, `[{"header:x-absent": {"exists": false}}]`)
	_, matched = h.EvaluateSkip(nil, ghHeaders("push"))
	assert.True(t, matched)
}

func TestEvaluateSkipORAcrossConditionsANDWithin(t *testing.T) {
	h := skipHook(t, `[
		{"header:x-github-event": "workflow_run"},
		{"action": {"in": ["labeled","unlabeled"]}, "sender.type": "Bot"}
	]`)

	// First condition matches on its own (OR).
	_, matched := h.EvaluateSkip([]byte(`{}`), ghHeaders("workflow_run"))
	assert.True(t, matched)

	// Second condition: both keys must hold (AND).
	reason, matched := h.EvaluateSkip([]byte(`{"action":"labeled","sender":{"type":"Bot"}}`), ghHeaders("pull_request"))
	require.True(t, matched)
	assert.Equal(t, `skip_if[1]: action in ["labeled", "unlabeled"] and sender.type == "Bot"`, reason)

	// One key failing fails the whole condition.
	_, matched = h.EvaluateSkip([]byte(`{"action":"labeled","sender":{"type":"User"}}`), ghHeaders("pull_request"))
	assert.False(t, matched)
	_, matched = h.EvaluateSkip([]byte(`{"action":"opened","sender":{"type":"Bot"}}`), ghHeaders("pull_request"))
	assert.False(t, matched)
}

func TestEvaluateSkipOperators(t *testing.T) {
	payload := []byte(`{
		"action": "completed",
		"ref": "refs/tags/v1.2.3",
		"number": 41,
		"draft": false,
		"conclusion": null,
		"repository": {"full_name": "wow-look-at-my/webhook-runner"}
	}`)
	cases := []struct {
		name   string
		skipIf string
		want   bool
	}{
		{"eq hit", `[{"action":"completed"}]`, true},
		{"eq miss", `[{"action":"opened"}]`, false},
		{"eq absent path", `[{"nope":"completed"}]`, false},
		{"ne hit", `[{"action":{"ne":"opened"}}]`, true},
		{"ne equal", `[{"action":{"ne":"completed"}}]`, false},
		{"ne absent path does NOT match", `[{"nope":{"ne":"anything"}}]`, false},
		{"in hit", `[{"action":{"in":["opened","completed"]}}]`, true},
		{"in miss", `[{"action":{"in":["opened","closed"]}}]`, false},
		{"exists true on leaf", `[{"action":{"exists":true}}]`, true},
		{"exists true on object node", `[{"repository":{"exists":true}}]`, true},
		{"exists true absent", `[{"nope":{"exists":true}}]`, false},
		{"exists false absent", `[{"nope":{"exists":false}}]`, true},
		{"exists false present", `[{"action":{"exists":false}}]`, false},
		{"prefix hit", `[{"ref":{"prefix":"refs/tags/"}}]`, true},
		{"prefix miss", `[{"ref":{"prefix":"refs/heads/"}}]`, false},
		{"regex hit", `[{"repository.full_name":{"regex":"^wow-look-at-my/"}}]`, true},
		{"regex miss", `[{"repository.full_name":{"regex":"^PazerOP/"}}]`, false},
		{"regex is unanchored substring match", `[{"ref":{"regex":"tags"}}]`, true},
		{"number stringifies as JSON literal", `[{"number":"41"}]`, true},
		{"bool stringifies as true/false", `[{"draft":"false"}]`, true},
		{"null stringifies as null", `[{"conclusion":"null"}]`, true},
		{"multiple ops on one key AND", `[{"ref":{"prefix":"refs/tags/","ne":"refs/tags/latest"}}]`, true},
		{"multiple ops one fails", `[{"ref":{"prefix":"refs/tags/","ne":"refs/tags/v1.2.3"}}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := skipHook(t, tc.skipIf)
			_, matched := h.EvaluateSkip(payload, ghHeaders("push"))
			assert.Equal(t, tc.want, matched)
		})
	}
}

func TestEvaluateSkipPathTraversal(t *testing.T) {
	payload := []byte(`{
		"commits": [
			{"message": "first"},
			{"message": "second [skip ci]"}
		],
		"deep": {"a": {"b": {"c": "leaf"}}},
		"weird": {"": "empty-key", "dotted.name": "unaddressable"}
	}`)
	cases := []struct {
		name   string
		skipIf string
		want   bool
	}{
		{"array element by index", `[{"commits.1.message":{"prefix":"second"}}]`, true},
		{"array index out of range", `[{"commits.7.message":{"exists":true}}]`, false},
		{"array non-numeric segment", `[{"commits.first.message":{"exists":true}}]`, false},
		{"path landing on an array is not a leaf", `[{"commits":"first"}]`, false},
		{"path landing on an object is not a leaf", `[{"deep.a":{"ne":"x"}}]`, false},
		{"deep nesting", `[{"deep.a.b.c":"leaf"}]`, true},
		{"walking through a scalar", `[{"deep.a.b.c.d":{"exists":true}}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := skipHook(t, tc.skipIf)
			_, matched := h.EvaluateSkip(payload, ghHeaders("push"))
			assert.Equal(t, tc.want, matched)
		})
	}
}

// Totality: whatever the payload looks like, evaluation returns without
// error and unresolvable conditions fail toward doing the work.
func TestEvaluateSkipTotality(t *testing.T) {
	h := skipHook(t, `[{"action":"completed"}]`)
	payloads := [][]byte{
		nil,
		{},
		[]byte("not json at all"),
		[]byte("null"),
		[]byte("[1,2,3]"),
		[]byte(`"just a string"`),
		[]byte(`{"action": {"nested": "completed"}}`),
		[]byte(`{"action": ["completed"]}`),
		[]byte(`{"action": 12}`),
		[]byte(strings.Repeat(`{"a":`, 500) + `1` + strings.Repeat(`}`, 500)),
	}
	for _, p := range payloads {
		_, matched := h.EvaluateSkip(p, ghHeaders("push"))
		assert.False(t, matched, "payload %.40q must not match", p)
	}

	// A header condition still matches even when the payload is garbage — header-only conditions never need the body parsed.
	h = skipHook(t, `[{"header:x-github-event": "workflow_run"}]`)
	_, matched := h.EvaluateSkip([]byte("*** not json ***"), ghHeaders("workflow_run"))
	assert.True(t, matched)
}

func TestEvaluateSkipHugeStringLeaf(t *testing.T) {
	huge := strings.Repeat("x", 1<<20) // 1 MiB leaf
	payload, err := json.Marshal(map[string]string{"blob": huge})
	require.NoError(t, err)

	h := skipHook(t, `[{"blob":{"prefix":"xxx"}}]`)
	_, matched := h.EvaluateSkip(payload, ghHeaders("push"))
	assert.True(t, matched)

	h = skipHook(t, `[{"blob":{"regex":"^x+$"}}]`)
	_, matched = h.EvaluateSkip(payload, ghHeaders("push"))
	assert.True(t, matched)
}

func TestEvaluateSkipNoConditions(t *testing.T) {
	h, err := parseInDir(t, `{"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json"}`)
	require.NoError(t, err)
	_, matched := h.EvaluateSkip([]byte(`{"anything":"here"}`), ghHeaders("push"))
	assert.False(t, matched)
}

func TestEvaluateSkipReasonRendering(t *testing.T) {
	h := skipHook(t, `[{
		"header:x-github-event": {"in": ["check_run", "check_suite"]},
		"action": {"ne": "completed"},
		"zebra": {"exists": false},
		"ref": {"prefix": "refs/", "regex": "tags"}
	}]`)
	reason, matched := h.EvaluateSkip(
		[]byte(`{"action":"created","ref":"refs/tags/v1"}`), ghHeaders("check_run"))
	require.True(t, matched)
	// Keys sorted for determinism; clauses joined with "and" (they are ANDed).
	assert.Equal(t, `skip_if[0]: action != "completed" and header x-github-event in ["check_run", "check_suite"] and ref starts with "refs/" and ref matches "tags" and zebra is absent`, reason)
}

// A hook constructed in code (never validated) with a broken regex must
// fail toward work, not panic or skip.
func TestEvaluateSkipUncompiledRegexFailsOpen(t *testing.T) {
	bad := "("
	good := "^v"
	h := &Hook{ID: "h", SkipIf: SkipConditions{{"tag": &SkipMatcher{Regex: &bad}}}}
	_, matched := h.EvaluateSkip([]byte(`{"tag":"v1"}`), nil)
	assert.False(t, matched, "non-compiling regex must not match")

	h = &Hook{ID: "h", SkipIf: SkipConditions{{"tag": &SkipMatcher{Regex: &good}}}}
	_, matched = h.EvaluateSkip([]byte(`{"tag":"v1"}`), nil)
	assert.True(t, matched, "in-code hooks compile lazily")

	// A nil matcher (only possible in code) fails toward work too.
	h = &Hook{ID: "h", SkipIf: SkipConditions{{"tag": nil}}}
	_, matched = h.EvaluateSkip([]byte(`{"tag":"v1"}`), nil)
	assert.False(t, matched)
}
