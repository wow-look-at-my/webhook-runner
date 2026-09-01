package hooks

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Friendly run titles ("run_title"): a hook can template the display title
// of its runs. {{path.to.field}} placeholders resolve using skip_if's exact
// traversal (parsePayloadTree/resolvePath/leafString in skip.go).
// Resolution never errors: a title is decoration, so a missing or bad
// placeholder just yields no title, never a stopped run.
// see docs/internals/runs-concurrency-and-overrides.md

// MaxRunTitleLen bounds a run title in bytes: enough for a subject line,
// small enough to stay a chip label. POST /title rejects longer; the
// template renderer clamps instead of erroring.
const MaxRunTitleLen = 200

// ScheduleFallbackTitle titles a schedule-triggered run when its template
// resolves nothing against the synthetic schedule payload.
const ScheduleFallbackTitle = "schedule"

// titleTemplate is a run_title parsed into segments at load time.
type titleTemplate struct {
	segs         []titleSeg
	placeholders int
}

// titleSeg is one template segment: literal text, or (path=true) a
// placeholder's payload path / "header:" key.
type titleSeg struct {
	text string
	path bool
}

// parseTitleTemplate splits "PR {{repository.full_name}}#{{pull_request.number}}"
// into segments. Errors here are load/validation errors — see compile below.
func parseTitleTemplate(raw string) (*titleTemplate, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("template must not be blank")
	}
	t := &titleTemplate{}
	rest := raw
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			if rest != "" {
				t.segs = append(t.segs, titleSeg{text: rest})
			}
			return t, nil
		}
		if open > 0 {
			t.segs = append(t.segs, titleSeg{text: rest[:open]})
		}
		rest = rest[open+2:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			return nil, errors.New(`unterminated "{{" placeholder (close it with "}}")`)
		}
		key := strings.TrimSpace(rest[:end])
		if key == "" {
			return nil, errors.New(`empty "{{}}" placeholder: name a payload path or "header:<name>"`)
		}
		if strings.ContainsAny(key, "{}") {
			return nil, fmt.Errorf("invalid placeholder %q: placeholders cannot nest", key)
		}
		// Placeholder keys are exactly skip_if condition keys (dotted payload
		// path or "header:<name>"), so they share its validation too.
		if err := validateSkipKey(key); err != nil {
			return nil, fmt.Errorf("invalid placeholder %q: %w", key, err)
		}
		t.segs = append(t.segs, titleSeg{text: key, path: true})
		t.placeholders++
		rest = rest[end+2:]
	}
}

// compileRunTitle parses RunTitle at load/validation time (into titleTmpl,
// which render uses), rejecting malformed templates so a broken one can
// never load — the skip_if regex rule. No-op when the hook declares none.
func (h *Hook) compileRunTitle() error {
	if h.RunTitle == "" {
		return nil
	}
	tmpl, err := parseTitleTemplate(h.RunTitle)
	if err != nil {
		return fmt.Errorf("invalid run_title: %w", err)
	}
	h.titleTmpl = tmpl
	return nil
}

// RenderRunTitle resolves the run_title template into the run's title, or
// "" for no title. It never errors; titles are decoration.
func (h *Hook) RenderRunTitle(payload []byte, header http.Header) string {
	if h.RunTitle == "" {
		return ""
	}
	tmpl := h.titleTmpl
	if tmpl == nil {
		// fallback for a hook built in code; parsed per call to avoid a
		// data race on h.titleTmpl across concurrent deliveries
		var err error
		if tmpl, err = parseTitleTemplate(h.RunTitle); err != nil {
			return ""
		}
	}
	return tmpl.render(payload, header)
}

// ScheduleRunTitle titles a schedule-triggered run: the resolved template,
// else ScheduleFallbackTitle so a tick chip is never gibberish.
func (h *Hook) ScheduleRunTitle(payload []byte, header http.Header) string {
	if t := h.RenderRunTitle(payload, header); t != "" {
		return t
	}
	return ScheduleFallbackTitle
}

// render substitutes placeholders and applies the collapse rules.
func (t *titleTemplate) render(payload []byte, header http.Header) string {
	var root any
	parsed := false
	payloadTree := func() any {
		if !parsed {
			parsed = true
			root = parsePayloadTree(payload)
		}
		return root
	}

	resolved := make([]string, len(t.segs))
	anyValue := false
	for i, seg := range t.segs {
		if !seg.path {
			resolved[i] = seg.text
			continue
		}
		v := resolveTitleKey(seg.text, payloadTree, header)
		resolved[i] = v
		if v != "" {
			anyValue = true
		}
	}
	// All placeholders empty (and there was at least one): no title — the
	// literal scaffolding alone would be noise. A placeholder-free template
	// is static and always set.
	if t.placeholders > 0 && !anyValue {
		return ""
	}

	var b strings.Builder
	for i, seg := range t.segs {
		if resolved[i] == "" {
			continue
		}
		// A literal that is pure separators (no letter or digit) next to an
		// empty placeholder is that placeholder's leftover scaffolding —
		// "{{repo}}#{{num}}" with num missing must render "a/b", not "a/b#".
		// Segments alternate literal/placeholder (only placeholders can be
		// adjacent), so checking the immediate neighbors is exhaustive.
		if !seg.path && separatorOnly(resolved[i]) &&
			(emptyPlaceholderAt(t.segs, resolved, i-1) || emptyPlaceholderAt(t.segs, resolved, i+1)) {
			continue
		}
		b.WriteString(resolved[i])
	}
	// Fold whitespace runs (dropped segments leave doubles; payload values
	// can carry newlines a one-line chip must not) and clamp.
	return clampTitle(strings.Join(strings.Fields(b.String()), " "))
}

// resolveTitleKey resolves one placeholder to its display string: request
// headers via the "header:" prefix (first value, name case-insensitive —
// skip_if's exact convention), payload paths via skip.go's shared
// resolvePath/leafString walk. Missing paths and non-leaves are "", and so
// is JSON null — "repo#null" is exactly the junk a title must never show.
func resolveTitleKey(key string, payloadTree func() any, header http.Header) string {
	if name, ok := strings.CutPrefix(key, HeaderKeyPrefix); ok {
		if vals := header.Values(strings.TrimSpace(name)); len(vals) > 0 {
			return strings.TrimSpace(vals[0])
		}
		return ""
	}
	node, ok := resolvePath(payloadTree(), key)
	if !ok || node == nil {
		return ""
	}
	leaf, isLeaf := leafString(node)
	if !isLeaf {
		return ""
	}
	return strings.TrimSpace(leaf)
}

// separatorOnly reports whether s contains no letter or digit — the "pure
// scaffolding" test for dropping a literal stranded by an empty neighbor.
func separatorOnly(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// emptyPlaceholderAt reports whether segs[i] exists, is a placeholder, and
// resolved empty. Out-of-range i (a literal at either end) is false.
func emptyPlaceholderAt(segs []titleSeg, resolved []string, i int) bool {
	return i >= 0 && i < len(segs) && segs[i].path && resolved[i] == ""
}

// clampTitle bounds a rendered title to MaxRunTitleLen bytes, cutting at a
// rune boundary (payload values aren't ASCII-only) and re-trimming. The
// renderer clamps silently — run-time resolution never errors — while the
// state API's /title rejects overlong input instead (a hook naming itself
// can be told no; a template can't).
func clampTitle(s string) string {
	if len(s) <= MaxRunTitleLen {
		return s
	}
	cut := MaxRunTitleLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
