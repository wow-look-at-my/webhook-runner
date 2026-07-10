package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandEnvRefs(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "FOO":
			return "foo-value", true
		case "EMPTY":
			return "", true
		}
		return "", false
	}

	cases := map[string]struct {
		in      string
		want    string
		missing []string
	}{
		"plain string untouched":  {"hello", "hello", nil},
		"whole value":             {"${FOO}", "foo-value", nil},
		"embedded":                {"pre-${FOO}-post", "pre-foo-value-post", nil},
		"two refs":                {"${FOO}/${FOO}", "foo-value/foo-value", nil},
		"set but empty":           {"${EMPTY}", "", nil},
		"unset becomes empty":     {"${NOPE}", "", []string{"NOPE"}},
		"mixed set and unset":     {"${FOO}${NOPE}", "foo-value", []string{"NOPE"}},
		"bare dollar untouched":   {"$FOO", "$FOO", nil},
		"unterminated untouched":  {"${FOO", "${FOO", nil},
		"bad name untouched":      {"${1FOO}", "${1FOO}", nil},
		"dollar in value is fine": {"a$b${FOO}", "a$bfoo-value", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, missing := ExpandEnvRefs(tc.in, lookup)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.missing, missing)
		})
	}
}

func TestHookDir(t *testing.T) {
	h := &Hook{SourcePath: "/var/lib/webhook-runner/hooks/my-hook/hook.json"}
	assert.Equal(t, "/var/lib/webhook-runner/hooks/my-hook", h.Dir())

	none := &Hook{}
	assert.Equal(t, "", none.Dir())
}
