package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A manifest may not carry a shell program. Both gates fire on these -- the Go
// check (better message) and the published schema -- and either alone would be
// enough to stop the hook loading.
func TestShellSubstitutionInCommandIsALoadError(t *testing.T) {
	cases := map[string]string{
		"command substitution":            `["sh","-c","echo $(date)"]`,
		"backticks":                       "[\"sh\",\"-c\",\"echo `date`\"]",
		"process substitution":            `["bash","-c","diff <(cat a) <(cat b)"]`,
		"output process substitution":     `["bash","-c","tee >(cat)"]`,
		"arithmetic expansion":            `["sh","-c","echo $((1+1))"]`,
		"substitution hidden mid-command": `["sh","-c","curl --data-binary @$HOOK_PAYLOAD_FILE \"$(sed -n 's/x/y/p' $HOOK_SETTINGS_FILE)\""]`,
	}
	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseHookDoc(t, `{`+schemaURL+`, "command": `+cmd+`}`)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must not carry a shell program")
			assert.Contains(t, err.Error(), ".sh file", "the message must say what to do instead")
		})
	}
}

// The same rule applies to script args -- that argv reaches the container too.
func TestShellSubstitutionInScriptArgsIsALoadError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, DockerfileName), []byte("FROM alpine\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	_, err := Parse("h", filepath.Join(dir, "hook.json"),
		[]byte(`{`+schemaURL+`, "script": {"file": "run.sh", "interpreter": "bash", "args": ["$(whoami)"]}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "script.args[0]")
}

// Plain variable references are the POINT of $HOOK_PAYLOAD_FILE and friends --
// there is nothing nested to hide in them, and banning them would break every
// legitimate one-line command.
func TestPlainVariableReferencesStayLegal(t *testing.T) {
	for _, cmd := range []string{
		`["sh","-c","cat $HOOK_PAYLOAD_FILE"]`,
		`["sh","-c","cat ${HOOK_SETTINGS_FILE}"]`,
		`["sh","-c","curl -fsS --data-binary @$HOOK_PAYLOAD_FILE https://example.com"]`,
		`["sh","run.sh"]`,
		`["node","main.ts"]`,
	} {
		require.NoError(t, parseHookDoc(t, `{`+schemaURL+`, "command": `+cmd+`}`), cmd)
	}
}

// Managers share the rule -- same validate, same schema.
func TestShellSubstitutionInManagerCommandIsALoadError(t *testing.T) {
	root := writeManagerTree(t, "m1", `{
	  "$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json",
	  "command": ["sh", "-c", "run $(id -u)"]
	}`)
	_, errs := LoadManagers(DetectLayout(root))
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "must not carry a shell program")
}

// The unit under the load path, so the message contract is pinned directly.
func TestCheckNoShellSubstitutionNamesTheField(t *testing.T) {
	require.NoError(t, checkNoShellSubstitution("command", nil))
	require.NoError(t, checkNoShellSubstitution("command", []string{"sh", "run.sh", "$HOME"}))

	err := checkNoShellSubstitution("command", []string{"sh", "-c", "x=`id`"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command[2]")
	assert.Contains(t, err.Error(), "command substitution")
}
