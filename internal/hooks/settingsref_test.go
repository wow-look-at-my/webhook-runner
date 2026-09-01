package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func expandSelf(t *testing.T, doc string) string {
	t.Helper()
	out, err := ExpandSettingsSelfRefs([]byte(doc))
	require.NoError(t, err)
	return string(out)
}

// ${settings:...} resolves against the document itself, so a value written
// can be referenced everywhere it is needed.
func TestSettingsSelfReferences(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		"simple key": {
			`{"base":"https://x.test","url":"${settings:base}/v1"}`,
			`{"base":"https://x.test","url":"https://x.test/v1"}`,
		},
		"nested path": {
			`{"a":{"b":{"c":"deep"}},"got":"${settings:a.b.c}"}`,
			`{"a":{"b":{"c":"deep"}},"got":"deep"}`,
		},
		"array index": {
			`{"xs":["zero","one","two"],"got":"${settings:xs[1]}"}`,
			`{"got":"one","xs":["zero","one","two"]}`,
		},
		"path through an array": {
			`{"fleets":[{"name":"a"},{"name":"b"}],"got":"${settings:fleets[1].name}"}`,
			`{"fleets":[{"name":"a"},{"name":"b"}],"got":"b"}`,
		},
		"several in one string": {
			`{"h":"host","p":"443","url":"https://${settings:h}:${settings:p}/"}`,
			`{"h":"host","p":"443","url":"https://host:443/"}`,
		},
		"reference to a reference": {
			`{"a":"real","b":"${settings:a}","c":"${settings:b}"}`,
			`{"a":"real","b":"real","c":"real"}`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.JSONEq(t, c.want, expandSelf(t, c.doc))
		})
	}
}

// A whole-string reference adopts the referenced value's TYPE -- otherwise
// every referenced number would arrive as a string and every schema
// describing it would have to lie.
func TestWholeStringReferenceKeepsType(t *testing.T) {
	assert.JSONEq(t, `{"max":5,"got":5}`,
		expandSelf(t, `{"max":5,"got":"${settings:max}"}`))
	assert.JSONEq(t, `{"on":true,"got":true}`,
		expandSelf(t, `{"on":true,"got":"${settings:on}"}`))
	// Embedded in text there is only sensible answer: text.
	assert.JSONEq(t, `{"max":5,"got":"up to 5"}`,
		expandSelf(t, `{"max":5,"got":"up to ${settings:max}"}`))
}

// ${env:...} is NOT resolved at load: the host environment does not exist at
// validation time, and `validate` in CI must never read the runner's.
func TestEnvReferencesSurviveLoad(t *testing.T) {
	assert.JSONEq(t, `{"tok":"${env:SECRET}"}`, expandSelf(t, `{"tok":"${env:SECRET}"}`))
}

// Everything a reference can get wrong is a LOAD error, named precisely --
// none of it may reach a container as literal text.
func TestBadSettingsReferencesAreLoadErrors(t *testing.T) {
	cases := map[string]struct{ doc, want string }{
		"no such path":         {`{"got":"${settings:nope}"}`, "no such setting"},
		"path through scalar":  {`{"a":"x","got":"${settings:a.b}"}`, "not a key of an object"},
		"index out of range":   {`{"xs":["a"],"got":"${settings:xs[3]}"}`, "out of range"},
		"index into non-array": {`{"a":{},"got":"${settings:a[0]}"}`, "not an array"},
		"empty path":           {`{"got":"${settings:}"}`, "needs a path"},
		"object has no text":   {`{"a":{"k":1},"got":"x${settings:a}"}`, "no text form"},
		"null has no text":     {`{"a":null,"got":"x${settings:a}"}`, "is null"},
		"unknown kind":         {`{"got":"${nope:x}"}`, "unknown reference kind"},
		"unterminated":         {`{"got":"${settings:a"}`, "unterminated reference"},
		"direct cycle":         {`{"a":"${settings:b}","b":"${settings:a}"}`, "cycle"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ExpandSettingsSelfRefs([]byte(c.doc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// The run path resolves ${env:...} with the same secrets-then-host lookup the
// rest of the runner uses.
func TestEnvReferencesResolveOnTheRunPath(t *testing.T) {
	lookup := func(name string) (string, bool) {
		v, ok := map[string]string{"TOKEN": "s3cr3t", "HOST": "api.test"}[name]
		return v, ok
	}
	out, err := ExpandSettingsEnvRefs(
		[]byte(`{"tok":"${env:TOKEN}","url":"https://${env:HOST}/v1","plain":"untouched"}`), lookup)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tok":"s3cr3t","url":"https://api.test/v1","plain":"untouched"}`, string(out))
}

// An unresolvable ${env:...} FAILS -- it is never an empty string. A hook
// that starts with a blank credential looks healthy and fails downstream,
// which is the failure mode settings exist to end.
func TestUnresolvableEnvReferenceIsAnError(t *testing.T) {
	_, err := ExpandSettingsEnvRefs([]byte(`{"tok":"${env:MISSING}"}`),
		func(string) (string, bool) { return "", false })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "${env:MISSING} does not resolve")
	assert.Contains(t, err.Error(), "secrets.sops.env", "the message must say where to set it")
}

// An empty-STRING value is a real value, not a missing : a lookup that
// says "present" wins over the failure path.
func TestEmptyEnvValueIsNotMissing(t *testing.T) {
	out, err := ExpandSettingsEnvRefs([]byte(`{"tok":"${env:EMPTY}"}`),
		func(string) (string, bool) { return "", true })
	require.NoError(t, err)
	assert.JSONEq(t, `{"tok":""}`, string(out))
}

// End to end through the loader: the schema sees the RESOLVED value, which is
// the reason self-references resolve at load rather than at run.
func TestSchemaValidatesTheResolvedValue(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/"+DockerfileName, "FROM alpine\n")
	writeFile(t, dir+"/"+SettingsSchemaFile,
		`{"type":"object","properties":{"url":{"type":"string","pattern":"^https://"}},"required":["url"]}`)

	h, err := Parse("h", dir+"/hook.json", []byte(`{`+schemaURL+`,
	  "command":["true"], "settings":{"base":"https://x.test","url":"${settings:base}/v1"}}`))
	require.NoError(t, err, "the reference resolves to a value the schema accepts")
	assert.JSONEq(t, `{"base":"https://x.test","url":"https://x.test/v1"}`, string(h.SettingsJSON()))

	// ...and a reference resolving to something the schema REJECTS is a load
	// error, caught before the hook ever runs.
	_, err = Parse("h", dir+"/hook.json", []byte(`{`+schemaURL+`,
	  "command":["true"], "settings":{"base":"ftp://x.test","url":"${settings:base}/v1"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "settings does not match")
}
