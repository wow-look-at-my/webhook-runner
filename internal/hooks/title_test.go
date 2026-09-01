package hooks

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// titledHook builds a hook whose run_title has been compiled the way a real load compiles it, so tests exercise the validated path (RenderRunTitle's.
func titledHook(t *testing.T, tmpl string) *Hook {
	t.Helper()
	h := &Hook{ID: "t", RunTitle: tmpl}
	require.NoError(t, h.compileRunTitle())
	return h
}

// The operator's motivating example, verbatim: a PR-shaped payload renders
// "owner/repo#number" in place of run-id gibberish.
func TestRunTitlePRExample(t *testing.T) {
	h := titledHook(t, "{{repository.full_name}}#{{pull_request.number}}")
	payload := []byte(`{"repository":{"full_name":"wow-look-at-my/go-toolchain"},"pull_request":{"number":47}}`)
	assert.Equal(t, "wow-look-at-my/go-toolchain#47", h.RenderRunTitle(payload, http.Header{}))
}

func TestRunTitleResolution(t *testing.T) {
	payload := []byte(`{
		"action": "opened",
		"repository": {"full_name": "o/r"},
		"pull_request": {"number": 47, "draft": false, "body": null},
		"commits": [{"message": "first"}, {"message": "second"}],
		"labels": ["a", "b"]
	}`)
	hdr := http.Header{"X-Github-Event": []string{"pull_request"}}

	cases := []struct {
		name, tmpl, want string
	}{
		// Leaves stringify exactly like skip_if's: numbers as their JSON literal text, booleans as the words.
		{"nested number", "PR {{pull_request.number}}", "PR 47"},
		{"boolean leaf", "draft={{pull_request.draft}}", "draft=false"},
		{"array index", "{{commits.1.message}}", "second"},
		// Headers resolve via the same header: prefix as skip_if keys, name case-insensitive.
		{"header placeholder", "{{header:x-github-event}} {{action}}", "pull_request opened"},
		// Missing paths and non-leaves (objects/arrays) render empty, and a pure-separator literal touching the hole is dropped as that.
		{"missing field trims trailing separator", "{{repository.full_name}}#{{issue.number}}", "o/r"},
		{"missing field trims leading separator", "{{issue.number}}#{{pull_request.number}}", "47"},
		{"non-leaf renders empty", "{{repository.full_name}} {{labels}}", "o/r"},
		// JSON null is "no value" for a title — never the word "null" (deliberate divergence from skip_if's leaf text).
		{"null renders empty", "{{repository.full_name}} {{pull_request.body}}", "o/r"},
		// Wordy literals survive an empty neighbor; only pure scaffolding drops, and whitespace runs left by drops fold to space.
		{"wordy literal kept", "PR {{issue.number}}#{{pull_request.number}} opened", "PR 47 opened"},
		{"whitespace folds", "run {{action}} {{issue.number}} done", "run opened done"},
		// A template with no placeholders is a static title, always set.
		{"static", "nightly sweep", "nightly sweep"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := titledHook(t, tc.tmpl)
			assert.Equal(t, tc.want, h.RenderRunTitle(payload, hdr))
		})
	}
}

// Every placeholder empty + at least placeholder = NO title: the
// literal scaffolding alone would be noise, and the run-id fallback wins.
func TestRunTitleAllPlaceholdersEmptyMeansNoTitle(t *testing.T) {
	h := titledHook(t, "PR {{repository.full_name}}#{{pull_request.number}}")
	assert.Empty(t, h.RenderRunTitle([]byte(`{"action":"opened"}`), http.Header{}))
	// A non-JSON payload resolves no payload path at all — same outcome.
	assert.Empty(t, h.RenderRunTitle([]byte("not json"), http.Header{}))
	// But resolving placeholder is enough to render.
	assert.Equal(t, "PR 9",
		h.RenderRunTitle([]byte(`{"pull_request":{"number":9}}`), http.Header{}))
}

// No template, no title — and a hook constructed in code with a template
// that never went through validate still renders (lazy parse), while
// whose template cannot parse yields no title instead of erroring.
func TestRunTitleUncompiledFallbacks(t *testing.T) {
	h := &Hook{ID: "t"}
	assert.Empty(t, h.RenderRunTitle([]byte(`{}`), http.Header{}))

	lazy := &Hook{ID: "t", RunTitle: "{{action}}"}
	assert.Equal(t, "opened", lazy.RenderRunTitle([]byte(`{"action":"opened"}`), http.Header{}))

	broken := &Hook{ID: "t", RunTitle: "{{action"}
	assert.Empty(t, broken.RenderRunTitle([]byte(`{"action":"opened"}`), http.Header{}))
}

// Rendered titles are clamped to MaxRunTitleLen bytes at a rune boundary —
// run-time resolution never errors, it degrades.
func TestRunTitleClamped(t *testing.T) {
	h := titledHook(t, "{{msg}}")
	long := strings.Repeat("x", MaxRunTitleLen+50)
	longPayload, err := json.Marshal(map[string]string{"msg": long})
	require.NoError(t, err)
	got := h.RenderRunTitle(longPayload, http.Header{})
	assert.Len(t, got, MaxRunTitleLen)

	// Multi-byte content still cuts on a rune boundary and stays valid.
	wide := strings.Repeat("é", MaxRunTitleLen) // bytes each
	widePayload, err := json.Marshal(map[string]string{"msg": wide})
	require.NoError(t, err)
	got = h.RenderRunTitle(widePayload, http.Header{})
	assert.LessOrEqual(t, len(got), MaxRunTitleLen)
	assert.True(t, strings.HasPrefix(wide, got), "clamp must cut whole runes: %q", got)
}

// Malformed templates are LOAD errors — the fail-closed rule shared with
// skip_if regexes — surfaced through Hook.validate via compileRunTitle.
func TestRunTitleValidation(t *testing.T) {
	bad := []string{
		"{{repository.full_name", // unterminated
		"prefix {{a}} then {{b",  // unterminated after a good
		"{{}}",                   // empty placeholder
		"{{   }}",                // blank placeholder
		"{{a{{b}}",               // nested braces
		"   ",                    // blank template
		"{{header: }}",           // header: must name a header (shared key rule)
	}
	for _, tmpl := range bad {
		h := &Hook{ID: "t", RunTitle: tmpl}
		assert.Errorf(t, h.compileRunTitle(), "template %q must fail validation", tmpl)
	}

	good := []string{
		"static",
		"{{a}}",
		"{{ padded.path }}",
		"{{header:x-github-event}}",
		"{{a}}#{{b.c.0.d}} trailing",
	}
	for _, tmpl := range good {
		h := &Hook{ID: "t", RunTitle: tmpl}
		assert.NoErrorf(t, h.compileRunTitle(), "template %q must validate", tmpl)
	}
}

// Parse rejects a malformed run_title end-to-end, and accepts + compiles a
// good (titleTmpl set, so rendering never re-parses).
func TestParseRunTitle(t *testing.T) {
	_, err := parseInDir(t, `{
		"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
		"run_title": "{{unterminated"
	}`)
	require.ErrorContains(t, err, "invalid run_title")

	h, err := parseInDir(t, `{
		"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
		"run_title": "{{repository.full_name}}#{{pull_request.number}}"
	}`)
	require.NoError(t, err)
	require.NotNil(t, h.titleTmpl)
	assert.Equal(t, "o/r#1",
		h.RenderRunTitle([]byte(`{"repository":{"full_name":"o/r"},"pull_request":{"number":1}}`), nil))
}

// Schedule ticks: the template resolves against the synthetic schedule
// payload when it can, and everything else — no template, or keyed on
// webhook fields a tick lacks — falls back to "schedule".
func TestScheduleRunTitle(t *testing.T) {
	tick := []byte(`{"trigger":"schedule","hook":"sweeper","time":"2026-07-13T00:00:00Z"}`)
	hdr := http.Header{"X-Webhook-Runner-Schedule": []string{"sweeper"}}

	assert.Equal(t, ScheduleFallbackTitle, (&Hook{ID: "sweeper"}).ScheduleRunTitle(tick, hdr))
	assert.Equal(t, ScheduleFallbackTitle,
		titledHook(t, "{{repository.full_name}}").ScheduleRunTitle(tick, hdr))
	assert.Equal(t, "sweep schedule",
		titledHook(t, "sweep {{trigger}}").ScheduleRunTitle(tick, hdr))
	assert.Equal(t, "nightly", titledHook(t, "nightly").ScheduleRunTitle(tick, hdr))
}
