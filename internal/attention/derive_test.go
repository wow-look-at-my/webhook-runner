package attention

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/go-containers/set"
	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// Every error class buildLoadAndApply collects maps to an attributed
// entry: the typed per-hook errors pin their hook, the zero-hooks guard
// gets its own source, anything else keys by its own message.
func TestFromLoadErrors(t *testing.T) {
	errs := []error{
		hooks.HookLoadError{HookID: "parse-fail", Err: errors.New("decode hook.json: bad")},
		concurrency.RefError{HookID: "ghost-group", Group: "nope"},
		hooks.IgnoredLegacyDirError{Dir: "/repo/legacy-hook"},
		hooks.ZeroHooksError{Dir: "/repo", Layout: "legacy"},
		errors.New("parse concurrency.json: unexpected token"),
	}
	load, zero := FromLoadErrors(errs)

	require.Len(t, zero, 1)
	assert.Equal(t, SourceZeroHooks, zero[0].Source)
	assert.Equal(t, KeyZeroHooks, zero[0].Key)
	assert.Contains(t, zero[0].Message, "no hooks loaded")

	require.Len(t, load, 4)
	byHook := map[string]Entry{}
	for _, e := range load {
		assert.Equal(t, SourceLoad, e.Source)
		byHook[e.Hook] = e
	}
	assert.Contains(t, byHook["parse-fail"].Message, "decode hook.json: bad")
	assert.Contains(t, byHook["parse-fail"].Message, "dropped")
	assert.Contains(t, byHook["ghost-group"].Message, `undeclared concurrency group "nope"`)
	assert.Contains(t, byHook["legacy-hook"].Message, "mixed hook layout")
	generic := byHook[""]
	assert.Contains(t, generic.Message, "parse concurrency.json")
	assert.Equal(t, "err:parse concurrency.json: unexpected token", generic.Key)

	// Empty input yields empty (non-nil) sets so ReplaceSource clears.
	load, zero = FromLoadErrors(nil)
	assert.Empty(t, load)
	assert.Empty(t, zero)
}

// The static probe flags unresolvable ${NAME} api_key/env references with
// the same resolution order the request/run paths use — and names ONLY the
// reference, never any value.
func TestProbeHooksReferences(t *testing.T) {
	t.Setenv("WHR_ATTN_TEST_SET", "resolved-value")
	loaded := map[string]*hooks.Hook{
		"bad-key": {ID: "bad-key", APIKey: "${WHR_ATTN_TEST_UNSET_X9}"},
		"healthy": {ID: "healthy", APIKey: "${WHR_ATTN_TEST_SET}"},
	}
	entries := ProbeHooks(loaded, hooks.NewSecretsLoader(""))
	require.Len(t, entries, 1)
	byKey := map[string]Entry{}
	for _, e := range entries {
		byKey[e.Hook+"/"+e.Key] = e
	}
	keyEnt := byKey["bad-key/"+KeyAPIKey]
	assert.Contains(t, keyEnt.Message, "${WHR_ATTN_TEST_UNSET_X9}")
	assert.Contains(t, keyEnt.Message, "401")
	assert.NotContains(t, keyEnt.Message, "resolved-value", "probe messages must never carry resolved values")

	// An api_key that RESOLVES to empty is just as broken (fail-closed 401).
	t.Setenv("WHR_ATTN_TEST_EMPTY", "")
	entries = ProbeHooks(map[string]*hooks.Hook{
		"empty-key": {ID: "empty-key", APIKey: "${WHR_ATTN_TEST_EMPTY}"},
	}, hooks.NewSecretsLoader(""))
	require.Len(t, entries, 1)
	assert.Equal(t, KeyAPIKey, entries[0].Key)
	assert.Contains(t, entries[0].Message, "empty value")
}

// A hook shipping a secrets.sops.env that cannot decrypt gets ONE sops
// entry — reference checks are skipped (auth/runs fail on the decrypt
// before any reference is expanded, so per-ref entries would be noise).
func TestProbeHooksSopsFailure(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets.sops.env"), []byte("not really encrypted"), 0o600))
	h := &hooks.Hook{
		ID:         "sopsy",
		SourcePath: filepath.Join(dir, "hook.json"),
		APIKey:     "${FROM_SOPS}",
	}
	loader := hooks.NewSecretsLoader(filepath.Join(dir, "no-such-sops-binary"))
	entries := ProbeHooks(map[string]*hooks.Hook{"sopsy": h}, loader)
	require.Len(t, entries, 1)
	assert.Equal(t, KeySops, entries[0].Key)
	assert.Contains(t, entries[0].Message, "cannot be decrypted")
}

// ApplyServeProbe owns the event-source clear rules that key off reload
// state: the request-time api_key entry clears once the probe finds the
// reference resolvable, and ANY event entry clears when its hook leaves
// the loaded set. Entries for still-broken hooks survive.
func TestApplyServeProbeSettlesEventEntries(t *testing.T) {
	a, _, _ := fixedClock()
	RegisterStandardEventRules(a)
	loader := hooks.NewSecretsLoader("")

	// Two deliveries were denied at request time; one hook also
	// self-reported a problem.
	a.ObserveEvent(KindHookMisconfigured, "fixed", "fixed: api_key reference ${WHR_ATTN_TEST_FIX} did not resolve")
	a.ObserveEvent(KindHookMisconfigured, "still-broken", "still-broken: api_key reference ${WHR_ATTN_TEST_UNSET_Z9} did not resolve")
	a.ObserveEvent(KindHookMisconfigured, "removed", "removed: api_key reference ${GONE} did not resolve")
	a.ObserveEvent(KindHookReported, "reporter", "permission missing: contents write")
	require.Equal(t, 4, a.Count())

	// The operator fixes one ref, removes one hook, and reloads.
	t.Setenv("WHR_ATTN_TEST_FIX", "now-set")
	loaded := map[string]*hooks.Hook{
		"fixed":        {ID: "fixed", APIKey: "${WHR_ATTN_TEST_FIX}"},
		"still-broken": {ID: "still-broken", APIKey: "${WHR_ATTN_TEST_UNSET_Z9}"},
		"reporter":     {ID: "reporter"},
	}
	ApplyServeProbe(a, loaded, loader)

	left := a.Snapshot()
	byID := set.New[string]()
	for _, e := range left {
		byID.Add(e.Source + "/" + e.Hook + "/" + e.Key)
	}
	assert.False(t, byID.Contains(SourceEvent+"/fixed/"+KeyAPIKey), "probe-clean hook's event entry clears")
	assert.False(t, byID.Contains(SourceEvent+"/removed/"+KeyAPIKey), "unloaded hook's event entry clears")
	assert.True(t, byID.Contains(SourceEvent+"/still-broken/"+KeyAPIKey), "still-broken hook's event entry survives")
	assert.True(t, byID.Contains(SourceSecrets+"/still-broken/"+KeyAPIKey), "and the probe reports it as state too")
	assert.True(t, byID.Contains(SourceEvent+"/reporter/reported:permission missing: contents write"),
		"hook-reported entries survive reloads while the hook stays loaded (a reload cannot fix a runtime problem)")

	// The reporter hook is removed on the next reload: its reported
	// entries clear (nothing left to fix here).
	delete(loaded, "reporter")
	ApplyServeProbe(a, loaded, loader)
	for _, e := range a.Snapshot() {
		assert.NotEqual(t, "reporter", e.Hook)
	}

	// A hook whose sops file is broken keeps its event entry alive: the
	// api_key is unresolvable as long as decryption fails.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets.sops.env"), []byte("garbage"), 0o600))
	loaded["still-broken"] = &hooks.Hook{
		ID:         "still-broken",
		SourcePath: filepath.Join(dir, "hook.json"),
		APIKey:     "${FROM_SOPS}",
	}
	ApplyServeProbe(a, loaded, hooks.NewSecretsLoader(filepath.Join(dir, "missing-sops")))
	found := false
	for _, e := range a.Snapshot() {
		if e.Source == SourceEvent && e.Hook == "still-broken" && e.Key == KeyAPIKey {
			found = true
		}
	}
	assert.True(t, found, "a failed decrypt must keep the event entry (auth still fails closed)")
}
