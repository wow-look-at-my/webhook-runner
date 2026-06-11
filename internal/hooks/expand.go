package hooks

import "regexp"

// envRefPattern matches ${NAME} references. Only the braced form is
// expanded — a bare $NAME passes through untouched, so values containing
// natural dollar signs don't need escaping.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnvRefs replaces every ${NAME} in s with lookup(NAME). This is how
// hook.json files reference secrets that live on the runner host instead of
// in the hooks repo (container env values and api_key support it).
//
// References whose lookup reports "not set" expand to the empty string and
// are returned in missing, so callers can warn (env values) or fail closed
// (api_key). Expansion happens at run/request time, never at load time, so
// `webhook-runner validate` in CI passes without the runner host's
// environment.
func ExpandEnvRefs(s string, lookup func(string) (string, bool)) (expanded string, missing []string) {
	expanded = envRefPattern.ReplaceAllStringFunc(s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	return expanded, missing
}
