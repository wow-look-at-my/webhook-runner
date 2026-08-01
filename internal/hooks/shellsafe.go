package hooks

import (
	"fmt"
	"strings"
)

// A manifest's `command` (and `script.args`) must not smuggle a SHELL PROGRAM
// into JSON. Command substitution -- `$(...)`, backticks -- and process
// substitution -- `<(...)`, `>(...)` -- are refused at load and by the
// published schema.
//
// What this prevents, using the line that provoked the rule (an example hook
// reading one field out of its settings document):
//
//	"command": ["sh", "-c",
//	  "curl -fsS -X POST --data-binary @$HOOK_PAYLOAD_FILE \"$(sed -n 's/.*\\\"slack_webhook_url\\\": *\\\"\\\\([^\\\"]*\\\\)\\\".*/\\\\1/p' $HOOK_SETTINGS_FILE)\""]
//
// That is a sed program inside a shell substitution inside a JSON string, so
// every character is escaped twice and nothing can check any of it: not the
// schema, not a linter, not a shell -- and not a reviewer. It parses JSON, by
// hand, with a regex. It has no exit-status handling, so an empty match posts
// the payload to the empty string. It cannot be run, tested, or debugged
// outside the running hook.
//
// The fix is always the same and always cheaper: put it in a `.sh` next to
// hook.json, COPY it into the image (code is baked in), and point `command` at
// the file:
//
//	"command": ["sh", "notify.sh"]     // or "script": {"file": "notify.sh", "interpreter": "bash"}
//
// Then it is a file with one level of quoting, `set -euo pipefail`, real error
// handling, shellcheck, and a diff a reviewer can read.
//
// Deliberately NOT banned: plain `$VAR` / `${VAR}` references. Reading
// $HOOK_PAYLOAD_FILE or $HOOK_SETTINGS_FILE in a one-line command is exactly
// what those variables are for, and there is nothing nested to hide in them.
var shellSubstitutions = []struct {
	token string
	what  string
}{
	{"$(", "command substitution"},
	{"`", "command substitution"},
	{"<(", "process substitution"},
	{">(", "process substitution"},
}

// checkNoShellSubstitution rejects argv entries carrying nested shell
// constructs. `field` names the manifest key for the error message.
func checkNoShellSubstitution(field string, argv []string) error {
	for i, arg := range argv {
		for _, s := range shellSubstitutions {
			if !strings.Contains(arg, s.token) {
				continue
			}
			return fmt.Errorf(
				"%s[%d] contains %s (%q): a manifest must not carry a shell program. "+
					"Put it in a .sh file next to the manifest, COPY it into the image, and call that "+
					"(e.g. command: [\"sh\", \"run.sh\"], or the script field) -- "+
					"plain $VAR references are fine, nested commands are not",
				field, i, s.what, s.token)
		}
	}
	return nil
}
