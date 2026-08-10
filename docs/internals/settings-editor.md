# The settings editor: operator overrides of a hook's own configuration

A hook's configuration is its `settings` object in hook.json, validated at
load against the `settings.schema.json` it ships. Changing a value therefore
meant a PR through the hooks repo's CI: the value is reviewed, which is
right, but *trying* one costs a deploy — and the operator who wants to know
whether `map_concurrency: 3` is too high for the gateway is exactly the
person who should not have to open a PR to find out.

The settings editor makes any declared field settable from the admin
dashboard, as a persisted operator override, without touching the manifest.

## The shape: sparse, per field

An override is `entity id -> RFC 6901 JSON Pointer -> value`
(`internal/overrides/settings.go`, persisted in the same `overrides.json`
as the kill switches and the concurrency limits).

It pins ONE field. It is emphatically not a snapshot of the settings
document, because a snapshot silently freezes every other field at whatever
it was when the operator last clicked: a later manifest edit would land in
git, pass CI, deploy, and do nothing. With one pointer per pinned field,
everything the operator has not touched keeps coming from hook.json.

## The four rules that make an override safe

1. **It can only pin a field the manifest already declares.** A schema often
   permits properties the manifest omits; a value with no manifest
   counterpart has no revert target and no reviewed default, and that is
   precisely the config that rots. `setAtPointer` refuses to create
   structure.
2. **No reference syntax.** `${settings:…}` resolves at load and `${env:…}`
   as the container starts, so an override carrying either would not be the
   value the operator typed. Refused, with that as the reason.
3. **The MERGED document is re-validated against the entity's schema.** An
   override is not exempt from the contract the manifest is held to.
4. **A refusal changes nothing.** `ApplySettingsOverrides` restores the
   previous document on any error, so a batch never half-applies and the
   caller's "keep serving the manifest values" is a real fallback rather
   than a partially-mutated document.

## Why a rejected override must NOT refuse the tree

`internal/cli.buildLoadAndApply` is all-or-nothing: if any entity fails to
load, nothing is applied. That is right for a manifest — the tree is
reviewed, CI-gated and rollback-able.

An override is none of those things. It is a value typed into a dashboard
and stored under the data dir, and the tree can move underneath it: a
manifest that renames the field, a schema that tightens a range. If that
could refuse the load, one stale override would take the whole fleet down,
with the fix reachable only by hand-editing `overrides.json` on the runner
host.

So `applySettingsOverrides` degrades per entity: drop that entity's
overrides for this load, serve its manifest values, and be loud on all three
surfaces an operator reads — the log, the activity feed
(`settings.override_rejected`), and the editor itself, which shows the exact
reason inline. The stored override is NOT deleted: the operator may be
mid-way through a manifest change, and silently discarding what they typed
is its own failure.

## When a change takes effect

Settings are materialized into `$HOOK_SETTINGS_FILE` as a run starts, so an
override applies to the NEXT run. A write re-runs the one reload closure, so
the registry serves the merged document immediately.

A manager holds its settings for the lifetime of its instance, so its
override lands when the instance restarts (the dashboard's manager restart
control, or any reload that replaces it).

## The API

    GET    /hooks/{id}/settings   schema + effective + manifest + overrides
    PUT    /hooks/{id}/settings   {"pointer": "/a/b", "value": <json>}
    DELETE /hooks/{id}/settings   ?pointer=/a/b, or everything when omitted

All three answer ONE shape (`settingsView`). That is not tidiness: the
editor re-renders from whatever a write returns, and when a write answered a
trimmed view without the schema, the entire form vanished on the operator's
first change — it read the missing schema as "this hook takes no
configuration". A partial view is not a smaller version of the full one, it
is a different claim.

A write validates the merged document BEFORE storing, so a bad value is a
400 carrying the schema's own message while the operator is still looking at
the field. The load path validates again, because the tree can move under a
stored override; neither check makes the other redundant.

A reload that fails after a successful write answers 500 saying the value is
stored but not live. Silence there would leave the operator believing a
value took effect while the registry still serves the old document.

## The form is generated, never described

`ts/settings.ts` (pure model) and `ts/settingsform.ts` (DOM + writes) build
the whole form from the entity's `settings.schema.json`, which the API
serves verbatim. There is deliberately no server-side "form description":
that would be a second schema to keep in sync, and the moment the two
disagreed the UI would offer values the loader rejects.

What each construct produces:

| schema | control |
|---|---|
| `enum`, or `oneOf`/`anyOf` of `const` | select; each option's `description` is its tooltip, and the SELECTED option's description renders on the page |
| `enum` + `enumDescriptions` | the same, reading the parallel-array convention |
| `integer`/`number` with both bounds | slider + number box, kept in sync |
| `integer`/`number` otherwise | number box alone — a slider with no ends is a lie |
| `boolean` | toggle |
| `string` | text; `format: uri` → url; `writeOnly` or a credential-shaped name → masked with a reveal |
| `array` of primitives | list editor (edit, remove, append) |
| anything else | raw JSON editor |

The last row is load-bearing: a field the operator cannot reach is worse
than one they must type carefully, so an unmodeled construct degrades to
JSON rather than disappearing.

Constraints are checked client-side before anything is sent — bounds,
`multipleOf`, lengths, patterns, enum membership, reference syntax — so a
bad value is explained on the field instead of after a round trip. The
server stays authoritative; the client only ever decides a value is
obviously wrong, and a malformed schema `pattern` defers rather than
blocking the field.

## Where the tests are

- `internal/overrides/settings_test.go` — storage, sparseness, rollback.
- `internal/hooks/settingsoverride_test.go` — the merge, the four rules,
  pointer escaping.
- `internal/server/settings_test.go` — the API, including the stale-pin
  report and the one-shape rule.
- `internal/server/dashboard/testjs/settings-model.test.ts` — schema → form.
- `internal/server/dashboard/testjs/settings-render.test.ts` — what renders
  and what a change sends, against a fake DOM.
