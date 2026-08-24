package hooks

import (
	"fmt"
	"strings"
)

// A manifest's `command` (and `script.args`) must not smuggle a SHELL PROGRAM into JSON.
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
