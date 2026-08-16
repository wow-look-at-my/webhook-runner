# Gotchas: timeouts, skips, titles, concurrency, overrides, scheduling, history

The optional wall-clock `timeout` and the independent `idle_timeout`, declarative skips, run titles, concurrency groups and the global run cap, atomic reloads, the operator kill switch, the scheduler, and the persistent run store.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- **Terminal ordering — `Run.Finish` closes `done` LAST.** The sequence is
  `onTerminal` → `onFinish` → `notifyChange` → `close(done)`, so by the
  time anything observes `<-run.Done()` the terminal activity line, the
  run-store write, the lock release and the stream delta have all landed.
  `Done()` is therefore a sufficient barrier on its own — callers must
  never need a second one. This is a fix, not an accident: the close used
  to happen first, which meant `Done()` only promised "the status field
  flipped", and three tests independently raced the other writes and had
  to bolt on `Runner.Wait()`. Work that must precede the close belongs in
  `Run.SetOnTerminal` (the runner registers the `run.finished` feed line
  there — it needs the image name and spawn note, which `RunState` does
  not carry), NOT after `Finish` returns. The GitHub commit-status POST
  is the deliberate exception: it stays outside the seam because blocking
  every `Done()` observer on a network call trades one footgun for a
  worse one. `internal/runs/terminal_test.go` pins the ordering by
  finishing on another goroutine and asserting from the `Done()` side.

- The run `timeout` bounds **only container processing**. In
  `runner.execute` the timeout context (`runContext`) is created *after*
  secrets decrypt, image build, and (crucially) after the concurrency-group
  slot is acquired — never at the top. A run waiting in a group's queue
  stays `pending` with no timeout running; if you move the context creation
  back up, queued runs start timing out while they wait, which is the exact
  bug this avoids. `run.SetRunning()` (pending→running) still fires only
  once the container launches, so the dashboard shows queued runs as
  `pending` — and it stamps `RunState.StartedAt`, the queue-wait/processing
  split point (`started` in JSON stays the QUEUED/accepted instant for
  compatibility; waited = StartedAt−Started, duration = Finished−StartedAt,
  and a zero StartedAt means the run never started). `timeout` is
  **optional with no default**: omitted means NO absolute ceiling —
  `runContext` arms no deadline (a plain cancellable child, so parent
  cancellation still kills), and the run is bounded only by `idle_timeout`,
  if set. A hook that omits both runs until it exits — deliberately the
  operator's call, no nanny validation. Older binaries applied a 5m
  `DefaultTimeout` to a timeout-less hook (they still *validate* it fine —
  absence was always legal — they just cap the run), so deploy the runner
  first when the uncapped semantics matter. `parseWaitParams` separately
  bounds the synchronous HTTP hold at `defaultSyncHold` (5m) for uncapped
  hooks — that degrades the *response* to a 202, never kills the run.
- `idle_timeout` (optional, independent of `timeout`) kills a run only when
  its container produces **no output** (stdout or stderr) for that long —
  the progress-aware timeout for hooks whose healthy runtime varies (added
  after a 47-part map-reduce run logging every ≤45s was killed by a 15m
  wall-clock `timeout`). Any output **byte** resets the clock: the runner
  wraps the pipe read side in a `touchReader` (internal/runner/watchdog.go),
  so even a long line without a newline counts. The `idleWatchdog` follows
  the **same arming rule** as `timeout`: `Arm()` is called only after the
  concurrency slot is acquired and `cmd.Start` succeeded — a queued run must
  never idle out, and an unarmed watchdog never fires (that invariant is
  unit-tested; keep it). An idle kill reuses the docker-kill-by-name path and
  ends the run as status `timeout` with the distinguishable error
  `idle timeout after <d> (no output)` (the total ceiling says "timed out
  after <d>"); the `run.finished` event message carries that reason. Like
  `state`/`concurrency_group`/`schedule`, `idle_timeout` is a newer hook.json
  field — old binaries reject it (`DisallowUnknownFields`), so deploy
  webhook-runner before any hook sets it. **Declared waits count as activity
  too**: while a state hook's `POST /wait` is in flight, the wait handler
  keeps touching the run's watchdog (see the wait bullet below), so an
  announced in-process sleep is never reaped as silence — only *undeclared*
  silence times out.
- Declarative skips (`skip_if` in hook.json, `internal/hooks/skip.go`):
  conditions over the request HEADERS (`"header:x-github-event"` keys,
  name case-insensitive) and the parsed JSON payload (dotted paths,
  `"workflow_run.conclusion"`, array elements by numeric index) — list
  entries ORed, keys within one condition ANDed (pr-minder's triggers
  convention), matchers a bare string (equality) or
  `{eq,ne,in,exists,prefix,regex}` (several ops on one key AND). It is
  deliberately NOT a language: total, bounded matching over stringified
  scalar leaves (numbers as their JSON literal via json.Number,
  true/false/null as those words; objects/arrays are not leaves), and
  `regex` is Go's RE2 (linear, no backtracking) **compiled at load time** —
  a malformed skip_if (unknown op, non-compiling regex, empty condition)
  is a load/validation error and the hook is dropped, same fail-closed
  rule as an undeclared concurrency group. **Dispatch order is
  load-bearing** (`handleTrigger` in internal/server/handlers.go): kill
  switch → body read → **authentication** → **skip_if** → wait params →
  `runner.Start`. Auth strictly first — an unauthenticated delivery that
  would match gets the plain 401 and must never probe the conditions or
  leave a record; and the skip strictly before any work — a match calls
  `runner.Skip`, which boots NO container (no temp files, no secrets
  decrypt, no image build, no concurrency slot, no onStart/onFinish GitHub
  statuses) yet creates a REAL terminal run: status `skipped`, ExitCode 0
  (placeholder — nothing exited), zero StartedAt, output
  `skipped: skip_if[N]: <rendered condition>`, flowing through the normal
  Finish → OnFinish seam into the runstore, a `run.skipped` activity
  event, and a purple `skipped` chip on the dashboard. The HTTP answer is
  immediate — `200 {"run_id","status":"skipped","reason"}` — for sync
  hooks too (never the sync snapshot path, whose non-success rule would
  500 a skip). Stats keep skips honest: `HookRunStats.Skipped` is its own
  bucket, EXCLUDED from Completed/SuccessRate/duration/wait so non-work
  can't dilute them (they still show in ByStatus and can be LastRun).
  Scheduled fires bypass skip_if by design (they don't pass through
  `handleTrigger`; a timer fire is the operator's own doing, not an
  unwanted delivery). Same deploy-first rule as
  state/concurrency_group/schedule: old binaries reject the unknown
  `skip_if` field, so deploy webhook-runner before merging a hook that
  sets it.
- Friendly run titles (`run_title` in hook.json, `internal/hooks/title.go`;
  the mid-run override `POST /title` in internal/server/title.go): a
  template whose `{{path.to.field}}` / `{{header:<name>}}` placeholders
  resolve with skip_if's EXACT shared traversal
  (parsePayloadTree/resolvePath/leafString — reuse, never fork) into
  `RunState.Title` (json `title`, omitempty, purely additive — the
  dashboard feature-detects it and falls back to the run id). Semantics
  are graceful-total, never blocking: scalars stringify like skip_if
  leaves EXCEPT JSON null → empty (titles must never render junk);
  missing/non-leaf → empty; ALL placeholders empty (with ≥1 declared) →
  NO title; pure-separator literals touching an empty placeholder drop;
  placeholder-free templates are static titles. A malformed template
  (unterminated `{{`, empty `{{}}`) is a load/validation error — the
  skip_if regex rule — while RUN-TIME resolution never errors. Ordering
  is load-bearing: `handleTrigger` renders the title ONCE, after auth,
  BEFORE EvaluateSkip, and hands it to runner.Skip/Start — so skipped
  runs are titled (SetTitle precedes Finish, putting the title in the
  persisted OnFinish snapshot), and `Run.SetTitle` refuses terminal runs
  so live state never diverges from history. Scheduled fires title via
  `Hook.ScheduleRunTitle` (template against the synthetic tick payload,
  else the `"schedule"` fallback — a tick chip is never gibberish).
  Titles persist in the runstore META BLOB ONLY — the per-hook index
  value format (`"<status> <finished-nanos> <startedat-nanos>"`) is
  untouched (byte-asserted in runstore tests; don't let a title near
  it). The activity feed carries titles inside the existing
  run.started/run.finished/run.skipped message strings (`runRef` in
  internal/runner — `id (title)`), never as event-schema changes.
  Bounds: `hooks.MaxRunTitleLen` (200) — the renderer clamps rune-safe,
  the /title route 400s instead (mirroring /wait's reason validation;
  409 for a terminal/foreign run). Same deploy-first rule as the other
  newer hook.json fields; older runners also 404 `/title` (hooks should
  shrug, not wedge — it's decoration).
- Concurrency groups (`internal/concurrency`) are declared centrally in
  `concurrency.json` at the hooks root, NOT per-hook: a hook only references
  a group by name via `concurrency_group`, and referencing an undeclared
  group is a load/validation error (the hook is dropped, not run unbounded —
  fail closed). The `concurrency.Manager` holds one buffered-channel
  semaphore per group; `Acquire` captures the channel in its release closure
  so a reload that swaps a group's semaphore can't lose or double-count a
  token — HOLDERS release into the exact channel they acquired from, for the
  life of their run. Blocked WAITERS do NOT stay bound: every swap closes the
  retired sem's `retired` channel and Acquire re-binds them to the group's
  current semaphore, so a limit change (reload or dashboard override) takes
  effect for already-queued runs immediately — a raise admits them at once
  (pre-fix they drained at the OLD limit, the "2→10 gha-runner override did
  nothing" production bug) and a group removed mid-queue fails those acquires
  loudly rather than stranding them. Pre-existing transients unchanged:
  in-flight holders above a lowered limit finish normally, and a raise
  briefly runs the old holders on top of the fresh channel's admissions.
  Alongside the semaphores the Manager keeps ADVISORY queue
  bookkeeping keyed by group NAME (who holds slots, who waits, in order —
  `QueueDetail`, surfaced as `/concurrency`'s `holders`/`waiting_runs` and
  the dashboard's expandable group rows): display data only, never part of
  gating, and name-keyed on purpose so it survives semaphore swaps (holders
  of a retired channel stay listed until they release). `Acquire` takes the
  run ID plus an `onQueue` callback invoked (serialized under the manager
  mutex — keep it fast, never call back into the Manager) when the run
  first has to wait and again on every holder/position change; the runner's
  `groupQueueObserver` mirrors those into the run's `waiting_on` {kind
  "group", key, holder_run_ids, position} via the SetWaitingOn seq-token
  machinery (deduped on identical states), cleared on acquire — and
  `attachWaiters` inverts group waits onto the HOLDERS as waiters with key
  `group:<name>`, exactly like lock waits. `concurrency_group` is a new hook.json field (so `Parse`'s
  `DisallowUnknownFields` means old binaries reject it — same deploy-first
  rule as above), and `concurrency.json` has its own published schema.
- The GLOBAL run cap (`concurrency.Global`, `internal/concurrency/global.go`)
  bounds how many hook executions run containers SIMULTANEOUSLY across ALL
  hooks — the Docker-bridge IPv4 guard (every running container holds a
  bridge IP; an unbounded flood exhausts the pool). It is a Manager
  pseudo-group in its own PRIVATE Manager instance (so the name can never
  collide with a declared group), reusing the semaphore/waiter-rebind
  machinery verbatim. Ordering is load-bearing: execute acquires the GROUP
  slot first, THEN the global slot (group-then-global everywhere — no
  lock-order cycles, and global slots are never consumed by runs parked on
  a group queue), both before the container starts, so a globally-queued
  run stays `pending` with the watchdog unarmed and cancellation honored.
  Precedence: persisted dashboard override (overrides.json
  `global_run_limit`, PUT|DELETE /concurrency-global/limit — a dedicated
  literal path so it can't collide with a group name) >
  WEBHOOK_RUNNER_MAX_CONCURRENT_RUNS (set-but-invalid FAILS startup) >
  the built-in default 64. The cap is a CEILING over the groups, never a
  replacement — group limits keep gating under it. SCOPE: hook runs only
  (deliveries, scheduled fires, /spawn); manager instances, image builds,
  and `webhook-runner test` containers are deliberately outside it. A
  queued run's waiting_on is kind "group" with key "global"
  (concurrency.GlobalWaitKey — display-only; the dashboard words it "the
  global run cap"). Related: every hook-run container is stamped with the
  `io.webhook-runner.run` label and serve REAPS labeled leftovers at boot
  (`runner.SweepOrphanContainers` — a server hard-killed mid-drain
  orphans its containers on the daemon, each holding a bridge IP forever;
  safe because the runstore's bbolt flock guarantees no concurrent serve
  process once Open succeeds, and manager/test containers are never
  labeled). Don't gate managers "for consistency": one persistent
  container per manager is bounded by declaration, and capping them could
  deadlock a manager behind its own spawned workers.
- Hooks AND concurrency groups AND schedules reload together through one
  closure (`buildLoadAndApply` in cli/serve.go), used by both the
  admin/webhook reload and the filesystem watcher. The watcher is now
  `hooks.WatchFunc` (takes an `onChange` callback; `hooks.Watch` is a thin
  back-compat wrapper) and also fires on `concurrency.json` edits. Don't
  reintroduce a second, separate reload path — the registry, the
  `concurrency.Manager`, and the `scheduler.Scheduler` must update
  atomically together or a hook can be registered before its group exists
  (or scheduled after it's been dropped).
- Operator overrides (`internal/overrides`) are the kill switch — exactly
  the "big red switch" for a runaway hook (a describe retry storm, a sweep
  flooding PRs): flip it on the dashboard instead of merging a config PR or
  deleting a repo. They are **operational state in the data dir**
  (`<data-dir>/overrides.json`, atomic temp+rename writes; a persist
  failure rolls the in-memory flip back and surfaces as a 500 + an
  `override.write_failed` event — same loud-write rule as kv), NOT
  hooks-repo config. The hook switch is TRI-STATE: hook.json's `enable`
  field (absent = true) is only the DEFAULT position, and the store
  persists an EXPLICIT per-hook enable/disable override (`hook_enable` in
  overrides.json; the legacy `disabled_hooks` set is still read — as
  explicit disables — AND written for binary downgrades) that outranks the
  default in both directions, so enabling an `"enable": false` hook
  sticks. Effective state = override-if-any, else the default; a hook that
  failed to LOAD counts as default-enabled (`Server.effectiveDisabled` /
  `Store.HookDisabled(id, defaultEnabled)` — every consumer goes through
  these, never a raw read). GET /attention drops entries of effectively
  disabled hooks at READ time (never deleted — re-enabling resurfaces
  them), and hook.disabled/hook.enabled/hooks.reloaded also dirty the
  "attention" stream section so the banner count tracks flips. The
  dashboard renders the whole thing as ONE slider switch per hook (hooks
  table Status column + the app page title row — `hookSwitch` in
  dashboard.js; no separate state pill, no Enable/Disable button).
  The disable gate lives **at dispatch, not load**: a
  disabled hook stays loaded/registered (image state, config, run history
  intact) and `handleTrigger` rejects deliveries with a distinct 503 +
  `hook.disabled_rejected` event, while `buildScheduleFire` skips its
  scheduled runs (`schedule.skipped`, reason "disabled by operator");
  run *cancellation* is deliberately not gated. Reloads **re-apply**
  overrides, never silently wipe them: the trigger/schedule gates read the
  store at dispatch time, and `concurrency.Manager` keeps limit overrides
  in an internal map that `Update` re-applies atomically (the semaphore
  swap is token-safe because releases capture their channel — the same
  invariant as reload). The store is opened in `runServe` BEFORE the first
  load and seeds the manager, so boot state reflects persisted overrides.
  An override whose hook/group vanishes on a reload is kept inert and
  announced ONCE per orphaning via `override.orphaned`
  (`announceOrphanedOverrides` dedups across reloads); it re-applies if the
  target returns. Concurrency overrides must be >= 1 — a 0 limit is
  rejected everywhere (store, manager, HTTP) because it would deadlock
  queued runs; disabling the hooks is the way to stop them entirely.
- The scheduler (`internal/scheduler`) fires hooks declaring a `schedule`
  (a Go duration on `Hook`, validated in `hook.validate`) on a timer. Like
  `concurrency.Manager` it is a **pure** component: it owns only timing and
  takes a `Fire func(hookID)` callback, so it imports neither the runner nor
  the registry/tracker and is tested with an injected clock. `cli/serve.go`
  wires `Fire` to look the hook up in the registry, apply
  **skip-if-already-running** via `tracker.HasActive` (overlap protection —
  a long sweep must not stack on itself; this, not a concurrency group, is
  the built-in guard), and call `rn.Start(context.Background(), …)` with a
  synthetic schedule payload — so a scheduled run is a normal run (tracked,
  group-gated, KV-enabled, on the dashboard) and emits
  `schedule.fired`/`schedule.skipped` events. **Missed-tick policy:**
  fire-immediately on first registration (startup, a newly added hook, or a
  changed interval), preserve an unchanged schedule's next-fire across an
  unrelated reload, and after a long pause fire once (no backlog burst) —
  see `scheduler.Update`/`fireDue`. `schedule` is a new hook.json field, so
  `Parse`'s `DisallowUnknownFields` means old binaries reject it: same
  deploy-first rule as `concurrency_group`/`state`.
- Run history persists (`internal/runstore`): a single bbolt file at
  `<data-dir>/runs.db` (pure Go, no CGO). A run is written **exactly once, at
  terminal status**, through the tracker's OnFinish seam
  (`runs.Tracker.SetOnFinish`, wired in cli/serve.go — Finish's once-guard is
  what makes the write exactly-once, and Finish fires on *every* runner path,
  so the runner needed no changes). Nothing is ever rehydrated into the
  tracker: the store is read-side only, merged behind the live tracker by
  `/runs`, `/runs/{id}` (fallback for evicted TERMINAL runs — the tracker
  never evicts active ones, see the stream bullet), and `/hooks/{id}` stats
  (deduped by run ID, live wins). A run in flight during a restart never
  completed and exists nowhere afterwards — that's by design, don't "fix" it.
  Retention is time-based (`WEBHOOK_RUNNER_RUN_RETENTION`, default 48h, the
  primary knob) with a per-hook count cap
  (`WEBHOOK_RUNNER_RUN_RETENTION_MAX`, default 200000) as a coarse disk
  safety net; like kv, expiry is lazy on reads AND swept in the background —
  keep both. Keys are time-ordered (`<zero-padded-start-nanos>-<run-id>`,
  where the nanos are the QUEUED/accepted time — leave the key format
  alone), so GC and newest-first reads are single cursor walks — including
  the `ListAllBefore`/`ListByHookBefore` variants behind `/runs?before=`
  paging (Seek to the cursor instant's bare nanos prefix, walk Prev:
  strictly-older, same retention break); the per-hook
  index *value* carries `"<status> <finished-nanos> <startedat-nanos>"`
  (third field = processing start, `0` = never started) so `SummariesByHook`
  (the stats path) never deserializes metadata blobs — don't change one side
  of that format without the other, and keep `splitSummary` accepting the
  legacy two-field `"<status> <finished-nanos>"` form: pre-upgrade rows read
  back with a zero StartedAt, their duration falls back to Finished−Started
  (queued-inclusive), and they are excluded from the wait stats. The
  deferred `runStore.Close()` runs after
  `rn.Wait()`, so every in-flight run records its terminal state before the
  DB closes — keep that ordering.
