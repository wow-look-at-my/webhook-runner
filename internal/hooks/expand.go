package hooks

import "regexp"

// envRefPattern matches ${NAME} references.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnvRefs replaces every ${NAME} in s with lookup(NAME). This is how hook.json files reference secrets that live on the runner host instead of in the hooks repo (container env values and api_key support it).
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
