# Gotchas: first-class waits and the spawn primitive

Declared in-process waits and their watchdog coupling, and the deny-by-default, manifest-authorized run-spawn primitive.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- First-class waits (`internal/server/wait.go`, `POST /wait` on the state
  socket): a hook that wants to pause SLEEPS IN-PROCESS by declaring it —
  `{"seconds": 1..600, "reason": "..."}`, both required (waits must be
  explained; one call caps at 10 min, loop for longer) — instead of
  deferring work to a timer/tick pattern. The server blocks ~N seconds and
  returns `{"waited": N}`, or early with `{"interrupted": true, "cause":
  "run finished"|"run cancelled"}` when the run ends/cancels (client
  disconnect just releases the handler). Requires `state: true` — only
  those hooks have the socket + token. Three coupled mechanisms, don't
  break any of them: (1) the runner registers the idle watchdog's Touch on
  the run (`run.SetActivityTouch`, at watchdog arm) and the wait handler
  keeps calling `TouchActivity` on a cadence derived from the hook's OWN
  timeout — `min(5s, timeout/3)`, 50ms floor — so a wait always outpaces
  the watchdog it is holding off (touches before arm / after finish are
  harmless no-ops; nothing deregisters). (2) Dashboard state is the ONE
  unified `RunState.WaitingOn` struct (`{kind: "wait"|"lock", reason,
  until, key, holder_run_id, holder_hook_id}` — declared sleeps AND
  blocked lock acquires share it) set by
  `Run.SetWaitingOn`/`ClearWaitingOn` (sequence-tokened so an overlapping
  newer pause — or a blocked acquire re-stamping its holder — can't be
  cleared by a stale older token); run rows and the run modal render
  "waiting Ns: reason" / "waiting on lock K held by RUN (HOOK)" with the
  remaining time computed client-side from `until`. (3) The field is
  TRANSIENT: `Finish` clears it before the OnFinish snapshot, so the
  persisted run history never shows a terminal run as waiting — don't
  "fix" that. One `run.wait` event is
  recorded per wait start (hook-scoped, so `?hook=` filters); there is
  deliberately NO wait-end event — the start message carries the duration,
  and a retry loop of short waits would double the feed volume. Deploy-first
  rule as usual: older runners 404 `/wait` (hooks should fall back to a
  plain sleep — they lose the badge and the activity credit, nothing else).
- Spawn (`POST /spawn` on the state API, `internal/server/spawn.go`): a
  MANAGER starts `count` runs of ANOTHER hook through the runner
  itself — the runner-native replacement for a coordinator POSTing
  HMAC-signed synthetic webhooks at the public endpoints. The CALLER
  (parent + run/instance id) comes from the verified bearer token, never
  the body. Authorization is DENY-BY-DEFAULT and MANIFEST-SOURCED: the
  caller's own manager.json `spawn_targets` array names the hook ids it
  may spawn (absent/empty = spawns nothing), loaded from the hooks tree
  like every other declaration — granting a spawn is a hooks-repo
  change, never host env. Entries must name declared HOOKS: an entry
  naming an unknown id or a manager fails load/validation and DROPS the
  manager (fail closed, the undeclared-concurrency-group rule —
  `hooks.CheckSpawnTargets`, run against the post-rejection sets in
  BOTH `buildLoadAndApply` and `validate`, so hooks-repo CI catches it).
  Only managers carry the field — the published hook schema stays
  frozen, so a hook-run caller is 403'd outright, and managers are never
  spawnable targets. Pre-validation is
  all-or-nothing BEFORE anything starts — 400/413 bounds (count 1..100,
  payload a JSON object ≤256KiB, optional `event` ≤100 chars), 409
  parent run/instance not active (the /wait rule), 404 unknown target,
  403 caller not a manager or target not in its spawn_targets, 409
  target effectively disabled (the SAME effective-disabled state
  handleTrigger and buildScheduleFire read) —
  each denial a loud `spawn.denied` event. A spawned run is a NORMAL run
  dispatched the scheduler-Fire way (`runner.StartSpawned` with
  context.Background() + a synthetic payload/headers pair — the target's
  concurrency_group applies, excess spawns queue as pending; `run_title`
  renders from the target's template; `event` becomes the
  `X-GitHub-Event` header, plus `X-Webhook-Runner-Spawned-By(-Run)`).
  skip_if is BYPASSED exactly like scheduled fires — a spawn is operator
  machinery's own doing, not an unwanted delivery; the target's in-code
  guards still run. The response (`200 {"run_ids":[...]}`, start order)
  is immediate — Start is async dispatch, the caller never waits on
  slots — and a mid-loop start failure answers 500 listing the runs that
  DID start plus the error (honest partial report). Attribution:
  `RunState.SpawnedBy` {run_id, hook_id} is additive/omitempty (the
  Title precedent — meta blob ONLY, never the runstore per-hook index
  value format, byte-asserted in runstore tests), and the
  run.started/run.finished event MESSAGES carry ", spawned by <hook> run
  <id>" (runRef style — no event-schema change). Deploy-first rule:
  a runner with manifest-spawn support deploys BEFORE any manager
  declaring `spawn_targets` merges (older manager-capable runners fail
  the manager's load on the unknown field; pre-manager runners 404
  `/spawn`), and callers must fail LOUD on 404/405 ("primitive
  unavailable") and 403 (no grant), never silently skip their fan-out.
