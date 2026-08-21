package server

// The admin drill-down's config view: the hook's OWN hook.json, filtered
// through an explicit key whitelist.
//
// The previous shape hand-copied a dozen fields into a typed struct, which
// meant every new hook.json key was invisible to operators until somebody
// remembered to mirror it here -- and mirrored keys drifted from their
// source spelling (seccomp.userns arrived as a flattened "seccomp_userns").
// A whitelist inverts that: the document passes through as authored, and
// the ONLY thing this file decides is which keys are safe to show.
//
// Fail-closed is the whole design. An unrecognized key is DROPPED, never
// passed through, so a future hook.json field that happens to hold a
// credential cannot leak by default -- it simply does not appear until
// someone adds it to the whitelist deliberately.

import (
	"encoding/json"
	"sort"

	"github.com/wow-look-at-my/go-containers/set"
)

// publicHookKeys are the hook.json keys safe to return verbatim: they carry
// configuration, never credentials. Anything absent here is dropped.
//
// DELIBERATELY EXCLUDED (each holds or can hold key material):
//   - api_key: a bearer token. Surfaced as the boolean api_key_set instead.
//   - secret:  the HMAC webhook secret. Surfaced as secret_set.
//   - settings: the hook's own config document, which routinely holds
//     credentials (secret-server tokens, app private-key names). Surfaced
//     as settings_keys -- TOP-LEVEL KEY NAMES ONLY, never values.
//
// public_key is intentionally INCLUDED: an ed25519 public key is public by
// construction, and an operator checking which key verifies a hook's
// deliveries needs to see it.
var publicHookKeys = set.Of(
	"api_key_header",
	"command",
	"concurrency_group",
	"description",
	"dind",
	"enable",
	"github_status",
	"idle_timeout",
	"networks",
	"public_key",
	"run_title",
	"schedule",
	"script",
	"seccomp",
	"signature_header",
	"skip_if",
	"state",
	"synchronous",
	"tests",
	"timeout",
	"user",
	"volumes",
	"workdir",
)

// HookConfig is the filtered hook.json plus the presence-only summaries
// standing in for the keys that cannot be shown.
type HookConfig struct {
	// Config holds the whitelisted keys exactly as hook.json spelled them:
	// `seccomp` stays a nested object, `skip_if` stays the real conditions.
	// What the operator reads here matches what they would edit.
	Config map[string]json.RawMessage `json:"config"`
	// APIKeySet and SecretSet report that a credential is configured
	// without disclosing it.
	APIKeySet bool `json:"api_key_set"`
	SecretSet bool `json:"secret_set"`
	// SettingsKeys are the TOP-LEVEL key names of the hook's settings
	// document. Names only: a settings object routinely holds credentials.
	SettingsKeys []string `json:"settings_keys,omitempty"`
}

// filterHookConfig applies the whitelist to a raw hook.json document.
// Unparseable input yields an empty config rather than an error: this is a
// display surface, and a hook that failed to parse never loaded anyway.
func filterHookConfig(raw []byte) HookConfig {
	out := HookConfig{Config: map[string]json.RawMessage{}}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	for k, v := range doc {
		if publicHookKeys.Contains(k) {
			out.Config[k] = v
		}
	}
	out.APIKeySet = hasNonEmptyString(doc["api_key"])
	out.SecretSet = hasNonEmptyString(doc["secret"])
	out.SettingsKeys = topLevelKeys(doc["settings"])
	return out
}

// hasNonEmptyString reports whether a raw value is a JSON string with
// content -- the presence test for a credential field.
func hasNonEmptyString(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	return s != ""
}

// topLevelKeys returns the sorted top-level key names of a JSON object,
// discarding every value.
func topLevelKeys(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
