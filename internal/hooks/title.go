// Friendly run titles ("run_title").
//
// A hook may declare a template for the display title of its runs, so the
// dashboard can say "wow-look-at-my/go-toolchain#47" instead of the opaque
// run id. {{path.to.field}} placeholders resolve against the delivery's
// payload JSON — object fields by name, array elements by numeric index —
// or a request header via the "header:" prefix, using THE SAME bounded
// traversal skip_if keys use (parsePayloadTree/resolvePath/leafString in
// skip.go): stringified scalar leaves only, total, and terminating in time
// bounded by the already size-capped payload. No expressions, no user code.
//
// Semantics (graceful, never blocking — a title is decoration, so run-time
// resolution NEVER errors and never stops a run):
//   - Each placeholder resolves to a scalar leaf's string form. A missing
//     path, a non-leaf (object/array), and JSON null all become "" — null
//     deliberately diverges from skip_if's "null" leaf text, because the
//     whole point of a title is to never render junk.
//   - When EVERY placeholder resolved empty and the template has at least
//     one, the run gets NO title at all: the surrounding literal text alone
//     ("PR " with no PR) is noise, and the dashboard's run-id fallback is
//     more honest.
//   - Otherwise empty placeholders collapse: a literal made purely of
//     separators (no letters or digits) touching an empty placeholder is
//     dropped ("a/b" + dropped "#" for "{{repo}}#{{num}}" with num missing),
//     runs of whitespace fold to one space, and the ends are trimmed.
//   - A template with no placeholders is a static title, always set.
//
// Malformed templates — an unterminated "{{", an empty "{{}}" — are a
// LOAD/validation error (the hook is dropped), the same fail-closed rule as
// a non-compiling skip_if regex: broken config must be caught in CI, never
// discovered as a silently missing title.
package hooks

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRunTitleLen bounds a run title in bytes, template-rendered or set mid-run via the state API's POST /title (which rejects longer; the.
const MaxRunTitleLen = 200

// ScheduleFallbackTitle is the title a schedule-triggered run gets when the hook's template (if any) resolves nothing against the.
const ScheduleFallbackTitle = "schedule"

// titleTemplate is a run_title parsed into literal and placeholder segments at load/validation time, so rendering never re-scans the raw.
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

// compileRunTitle parses RunTitle at load/validation time (into titleTmpl, which render uses), rejecting malformed templates so a broken one can never load — the skip_if regex rule.
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

// RenderRunTitle resolves the hook's run_title template against a
// delivery's payload and headers, returning the run's friendly title or ""
// for "no title" (no template declared, or every placeholder came up
// empty). It never errors: titles are decoration, and resolution happens
// once at run creation on the hot dispatch path.
func (h *Hook) RenderRunTitle(payload []byte, header http.Header) string {
	if h.RunTitle == "" {
		return ""
	}
	tmpl := h.titleTmpl
	if tmpl == nil {
		// Normal loads compile at validation time; this fallback covers hooks constructed in code.
		var err error
		if tmpl, err = parseTitleTemplate(h.RunTitle); err != nil {
			return ""
		}
	}
	return tmpl.render(payload, header)
}

// ScheduleRunTitle titles a schedule-triggered run: the template resolved against the synthetic schedule payload when that yields anything, else the "schedule".
func (h *Hook) ScheduleRunTitle(payload []byte, header http.Header) string {
	if t := h.RenderRunTitle(payload, header); t != "" {
		return t
	}
	return ScheduleFallbackTitle
}

// render substitutes placeholders and applies the collapse rules. The
// payload tree is parsed lazily at most once — same convention as
// EvaluateSkip — so a static template never touches the body.
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
	// All placeholders empty (and there was at least one): no title — the literal scaffolding alone would be noise.
	if t.placeholders > 0 && !anyValue {
		return ""
	}

	var b strings.Builder
	for i, seg := range t.segs {
		if resolved[i] == "" {
			continue
		}
		// A literal that is pure separators (no letter or digit) next to an empty placeholder is that placeholder's leftover scaffolding —.
		if !seg.path && separatorOnly(resolved[i]) &&
			(emptyPlaceholderAt(t.segs, resolved, i-1) || emptyPlaceholderAt(t.segs, resolved, i+1)) {
			continue
		}
		b.WriteString(resolved[i])
	}
	// Fold whitespace runs (dropped segments leave doubles; payload values can carry newlines a one-line chip must not) and clamp.
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

// separatorOnly reports whether s contains no letter or digit — the "pure scaffolding" test for dropping a literal stranded by an empty.
func separatorOnly(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// emptyPlaceholderAt reports whether segs[i] exists, is a placeholder, and resolved empty.
func emptyPlaceholderAt(segs []titleSeg, resolved []string, i int) bool {
	return i >= 0 && i < len(segs) && segs[i].path && resolved[i] == ""
}

// clampTitle bounds a rendered title to MaxRunTitleLen bytes, cutting at a rune boundary (payload values aren't ASCII-only) and re-trimming.
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
