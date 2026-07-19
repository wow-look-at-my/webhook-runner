// Package jsonc strips JSONC-style comments (// line and /* block */) from
// a byte slice, returning a reader over plain JSON. The webhook-runner
// config files documented to users (hook.json, concurrency.json) allow
// comments; this is the single shared implementation that removes them
// before the standard library decoder sees the bytes.
package jsonc

import "strings"

// NewReader returns a reader over the input with // and /* */ comments
// removed. The implementation is intentionally simple and string-state
// aware: it preserves bytes inside string literals exactly, so a "//" or
// "/*" inside a JSON string is left untouched.
func NewReader(in []byte) *strings.Reader {
	var out strings.Builder
	out.Grow(len(in))
	const (
		stateNormal = iota
		stateString
		stateLineComment
		stateBlockComment
	)
	state := stateNormal
	escape := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch state {
		case stateNormal:
			if c == '/' && i+1 < len(in) {
				switch in[i+1] {
				case '/':
					state = stateLineComment
					i++
					continue
				case '*':
					state = stateBlockComment
					i++
					continue
				}
			}
			if c == '"' {
				state = stateString
			}
			out.WriteByte(c)
		case stateString:
			out.WriteByte(c)
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				state = stateNormal
			}
		case stateLineComment:
			if c == '\n' {
				state = stateNormal
				out.WriteByte(c)
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(in) && in[i+1] == '/' {
				state = stateNormal
				i++
			}
		}
	}
	return strings.NewReader(out.String())
}
