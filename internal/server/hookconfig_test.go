package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
)

// A manifest holding one of every shape that matters: secrets, non-secret config, a nested block, and a key nobody whitelisted.
const sampleManifest = `{
  "description": "d",
  "secret": "SUPERSECRET_HMAC",
  "api_key": "SUPERSECRET_APIKEY",
  "public_key": "abc123",
  "dind": true,
  "seccomp": {"userns": true},
  "skip_if": [{"action": {"ne": "queued"}}],
  "settings": {"token": "SUPERSECRET_TOKEN", "url": "https://example.test"},
  "future_unknown_key": "SUPERSECRET_FUTURE"
}`

// THE test: no secret value may appear anywhere in the serialized output,
// whatever key carried it. Asserted over the marshaled bytes rather than
// field-by-field, so a future field that forwards a secret fails here even
// if nobody thought to write an assertion for it.
func TestFilterHookConfigNeverLeaksSecrets(t *testing.T) {
	got := filterHookConfig([]byte(sampleManifest))
	encoded, err := json.Marshal(got)
	require.NoError(t, err)

	for _, secret := range []string{
		"SUPERSECRET_HMAC",
		"SUPERSECRET_APIKEY",
		"SUPERSECRET_TOKEN",
		"SUPERSECRET_FUTURE",
	} {
		assert.NotContains(t, string(encoded), secret,
			"a secret value reached the admin response")
	}
}

// An unknown key is DROPPED, not passed through: the whitelist has to fail
// closed or a future credential-bearing field leaks the day it is added.
func TestFilterHookConfigDropsUnknownKeys(t *testing.T) {
	got := filterHookConfig([]byte(sampleManifest))
	_, present := got.Config["future_unknown_key"]
	assert.False(t, present, "an unwhitelisted key must not be forwarded")
	_, secretPresent := got.Config["secret"]
	assert.False(t, secretPresent, "secret must never be forwarded")
	_, apiKeyPresent := got.Config["api_key"]
	assert.False(t, apiKeyPresent, "api_key must never be forwarded")
	_, settingsPresent := got.Config["settings"]
	assert.False(t, settingsPresent, "settings values must never be forwarded")
}

// Whitelisted keys keep their AUTHORED shape -- the point of the rewrite.
// seccomp stays a nested object rather than becoming a flattened
// seccomp_userns, so what an operator reads matches what they would edit.
func TestFilterHookConfigPreservesAuthoredShape(t *testing.T) {
	got := filterHookConfig([]byte(sampleManifest))

	assert.JSONEq(t, `{"userns": true}`, string(got.Config["seccomp"]))
	assert.JSONEq(t, `true`, string(got.Config["dind"]))
	assert.JSONEq(t, `"abc123"`, string(got.Config["public_key"]),
		"public_key is public by construction and stays visible")
	assert.JSONEq(t, `[{"action": {"ne": "queued"}}]`, string(got.Config["skip_if"]))
}

// Credentials are reported as PRESENT without being disclosed.
func TestFilterHookConfigSummarizesSecrets(t *testing.T) {
	got := filterHookConfig([]byte(sampleManifest))
	assert.True(t, got.SecretSet)
	assert.True(t, got.APIKeySet)
	assert.Equal(t, []string{"token", "url"}, got.SettingsKeys,
		"settings are reported as key names only, sorted")
}

// An absent credential reads as absent, not as an empty string that looks
// configured.
func TestFilterHookConfigAbsentCredentials(t *testing.T) {
	got := filterHookConfig([]byte(`{"description": "d", "secret": ""}`))
	assert.False(t, got.SecretSet, "an empty secret is not a configured one")
	assert.False(t, got.APIKeySet)
	assert.Nil(t, got.SettingsKeys)
}

// A hook built outside Parse has no manifest; that must not panic or
// invent config.
func TestFilterHookConfigEmptyInput(t *testing.T) {
	got := filterHookConfig(nil)
	assert.Empty(t, got.Config)
	assert.False(t, got.SecretSet)

	broken := filterHookConfig([]byte("{not json"))
	assert.Empty(t, broken.Config)
}

// Every key the whitelist names must be a real hook.json key. A typo here
// would silently hide a field forever, which is the failure mode a
// whitelist is most prone to.
func TestWhitelistedKeysAreRealManifestKeys(t *testing.T) {
	// Spelled independently of hook.go so a rename has to be made twice,
	// deliberately, rather than drifting silently through a shared const.
	known := set.Of(
		"api_key", "api_key_header", "command",
		"concurrency_group", "description", "dind",
		"enable", "github_status", "idle_timeout",
		"networks", "public_key", "run_title",
		"schedule", "script", "seccomp", "secret",
		"settings", "signature_header", "skip_if",
		"state", "synchronous", "tests", "timeout",
		"user", "volumes", "workdir",
	)
	for _, k := range publicHookKeys.Values() {
		assert.True(t, known.Contains(k), "whitelisted key %q is not a hook.json key", k)
	}
}

// The three credential-bearing keys must NEVER be whitelisted. Stated as
// its own test so removing a name from the exclusion list is a red build.
func TestSecretKeysAreNotWhitelisted(t *testing.T) {
	for _, k := range []string{"api_key", "secret", "settings"} {
		assert.False(t, publicHookKeys.Contains(k),
			"%q holds credentials and must not be whitelisted", k)
	}
}

// Documents the trade the whitelist makes: a NEW hook.json key is invisible
// until somebody adds it here. That is the safe direction (missing beats
// leaked), and this test records it so the behavior is intentional rather
// than surprising.
func TestNewKeysAreInvisibleUntilWhitelisted(t *testing.T) {
	got := filterHookConfig([]byte(`{"brand_new_field": "value"}`))
	assert.Empty(t, got.Config,
		"a new key stays hidden until deliberately whitelisted")
	assert.False(t, strings.Contains(mustJSON(t, got), "brand_new_field"))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
