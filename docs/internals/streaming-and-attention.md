# Gotchas: the run live tail and the needs-attention surface

The SSE stream hub and its never-block invariant, the five section-signal seams, and the aggregated needs-attention entries with their clear rules.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- The run live tail (`internal/server/streamhub.go`, GET `/runs/stream` on
  the admin port) hangs off `runs.Tracker.SetOnChange` — a nil-safe
  notification seam like `events.Recorder`, invoked synchronously on the
  MUTATING goroutine (runner dispatch, state API, cancel handlers) after
  every observable run mutation, with an output-stripped snapshot. Two
  invariants: (1) the hub's publish NEVER blocks — bounded per-client
  buffered channels, non-blocking sends, and a client whose buffer is full
  is dropped on the spot (channel closed → its handler returns → its
  EventSource reconnects and resyncs from the connect snapshot; that
  drop-and-resync IS the slow-client semantics, so never "fix" it with a
  blocking send or an unbounded buffer — it would let one wedged browser
  tab apply backpressure to run execution). (2) the handler subscribes
  BEFORE reading the snapshot, so no mutation can fall between snapshot
  and stream — anything landing in that window is buffered and delivered
  after (clients merge by run id, so the duplicate is harmless). The
  terminal notification fires AFTER the OnFinish seam (run store write
  first) and BEFORE `close(done)` — `Run.Finish` settles every terminal
  side effect (onTerminal, onFinish, notifyChange) and closes the done
  channel last, so `<-run.Done()` is a sufficient barrier by itself and no
  caller needs a second one. See "Terminal ordering" in
  `docs/internals/runs-concurrency-and-overrides.md`. At shutdown
  `srv.CloseStreams()` runs before the admin server's
  `Shutdown` — Shutdown drains in-flight handlers, and stream handlers
  only return when their subscription closes or their client hangs up.
  `streamHeartbeat` is a package var so tests can shrink it.
  Every `hb` write carries the ACTIVE (non-terminal) run-id set —
  `{"active":[...]}` via `runs.Tracker.ActiveIDs` (sorted; `[]` when
  idle, NEVER null — the empty array is a real "nothing is active"
  verdict clients act on) — the timeline's truth-reconcile beat, purely
  additive (pre-payload clients read hb as bare liveness). The
  /runs-shaped reads pair with it: `mergedRuns` (runlist.go) caps
  TERMINAL rows only on cursorless windows — every active run is ALWAYS
  included however small `?max=` is, and `?live=1` serves exactly the
  active set — while `?before=` cursor pages keep the legacy newest-max
  TOTAL cap on purpose (the paging walk advances its cursor from each
  page's oldest row; an uncapped ancient active row would make it skip
  terminal history). The tracker's per-hook trim (`runs.Tracker.New`,
  internal/runs/tracker.go) evicts oldest TERMINAL runs only: an active
  run is current truth and is never evicted — the per-hook list may
  exceed MaxRunsPerHook while that many runs are genuinely active, and
  it shrinks back as they finish. Don't reintroduce a status-blind trim:
  it made GET /runs/{id} 404 for still-running runs during floods (the
  runstore fallback is terminal-only) and cut live runs out of windows.
  The SAME connection multiplexes the dashboard's section-invalidation
  push (`event: changed`, `{"sections":[...]}` — "changed → refetch
  once", never payloads): per-subscriber it is a bounded dirty SET + a
  1-slot wake channel, NOT the delta queue, so signal storms coalesce
  into one drain and signals can never overflow/drop/block anyone — only
  run deltas drop a slow client, and a reconnecting client refetches
  every section on open so no signal is load-bearing. Five seams feed
  `streamHub.signal`, wired in `server.New`: (1) the tracker OnChange
  wrapper also dirties "concurrency" (group active/waiting/holders move
  exactly with run lifecycle/waiting_on — a deliberate superset); (2)
  `events.Recorder.SetOnRecord` → `sectionsForEvent(kind)` (every event
  dirties "events"; hooks.reloaded → hooks+images+concurrency,
  hook.load_error/disabled/enabled → hooks, image.* → images,
  concurrency.* → concurrency; rejection noise like hook.denied
  deliberately does NOT dirty the roster) — the same callback also feeds
  each event to `attention.Aggregator.ObserveEvent` (the event-derived
  entry seam; unrecognized kinds no-op); (3) `kv.Store.SetOnMutate`
  (successful entry mutations + reclaiming sweeps; locks are not entries
  and never signal; a lazily-expired entry only signals at its sweep, so
  /kv views can lag expiry by ≤1 sweep interval); (4)
  `attention.Aggregator.SetOnChange` → "attention" (fired only on REAL
  set changes — an identical re-derivation on a quiet reload signals
  nothing); (5) `managers.Supervisor.SetOnChange` → "managers" (instance
  OUTPUT lines, inbox depth/stamps, and supervision state transitions —
  none of which record an activity event, so before this seam the
  Managers page and the `#manager=<id>` drill-down only moved on the
  occasional lifecycle event and F5 was the operator's refresh button;
  output is deliberately UNTHROTTLED — the hub's dirty set and the
  client's 1s coalescing absorb a chatty instance, whereas a throttle
  here could only lose the last line, i.e. the stale tail itself). All
  five callbacks run
  synchronously on mutating goroutines under their owners' mutexes —
  keep them trivial (the hub only flips bounded dirty bits), never let
  them call back into their owner. Client side: timeline.ts re-publishes
  `changed` as `whr:sections-changed` (and dashboard.js's /hooks fetch
  flows back as `whr:hooks-data` — timeline never fetches /hooks itself);
  dashboard.js's section feed refetches named sections with leading-edge
  + 1s trailing coalescing, single-flight, bounded fetches
  (AbortSignal.timeout), full-refresh on every stream (re)open, and a
  fixed 5s full-refresh fallback ONLY while the stream is down — zero
  polling while it is live (proven by the node harness in
  internal/server/dashboard/testjs/, run by CI). The "attention" section
  is the one granular section the app view keeps (everything else folds
  into one "app" token): the red banner renders on BOTH views, so its
  signal refetches /attention wherever the operator is.
- The needs-attention surface (`internal/attention`, `GET /attention`,
  the dashboard's red banner + "Needs attention" panel) is the
  PERSISTENT view of ACTIVE misconfigurations — the activity feed
  announces them and scrolls on; the aggregator holds the current set
  until each problem RESOLVES (no acknowledgement anywhere). Entry
  identity is (source, hook, key); `since` is when the problem first
  became active — preserved across re-derivations while it persists
  (message may be reworded in place), reset on clear+recur. Sources and
  their CLEAR rules (every rule can actually fire — don't add a source
  without one):
  - `load` (hook dropped at load/validation: parse error, missing
    Dockerfile/$schema, malformed skip_if/run_title, undeclared
    concurrency_group, IgnoredLegacyDirError, unreadable tree,
    unparseable concurrency.json) and `zero-hooks`
    (hooks.ZeroHooksError): STATE-derived — `buildLoadAndApply` re-derives
    them from the retained load errors on EVERY reload (the loader's
    per-hook failures are typed `hooks.HookLoadError` for attribution),
    so they clear on the first reload where the hook loads / any hook
    loads / the dir is removed.
  - `secrets` (unresolvable `${NAME}` api_key/env references — including
    an api_key that expands to empty — and sops decrypt failures):
    STATE-derived by `attention.ApplyServeProbe`, a static probe run per
    reload over the LOADED hooks with the request/run paths' exact
    resolution order (secrets.sops.env first, then host env). Clears when
    the reference resolves / the file decrypts / the hook goes away.
    STRICTLY serve-path-only (called from buildLoadAndApply): `validate`
    must stay environment-independent — never call the probe from a CLI
    path. A hook whose sops decrypt fails gets ONE `sops` entry and no
    per-reference entries (auth/runs fail on the decrypt first).
  - `server` (the containerized-without-TMPDIR hazard,
    runner.WarnIfContainerized's verdict): BOOT-scoped — computed once at
    startup, and a running process's env can't change, so it CANNOT clear
    without a restart (documented in the entry; a restart with TMPDIR set
    boots without it).
  - `event` (event-derived, via `RegisterStandardEventRules` — THE SEAM
    for hook-emitted signals): recognized activity-event kinds feed
    entries through per-kind RuleFuncs registered on the aggregator;
    server.New's OnRecord wiring hands every recorded event to
    `ObserveEvent`. Today: `hook.misconfigured` (request-time api_key
    denial, recorded by auth.go) → entry keyed `api_key`, cleared by the
    next reload whose probe finds the hook's api_key resolvable, or the
    hook leaving the loaded set. RESERVED for the fleet's silent-fail
    audit: `hook.reported_misconfigured` (one entry per distinct message,
    keyed `reported:<message>`) paired with `hook.reported_healthy`
    (clears ALL of that hook's reported entries); reported entries also
    clear when the hook leaves the loaded set — but deliberately NOT on a
    later successful run (a run can succeed while the feature it should
    exercise stays inert). New hook-emitted classes plug in by recording
    a recognized kind + registering a rule — no redesign.
  - `schedule` (`attention.CheckStaleSchedules`): a hook declaring a
    `schedule` interval whose last SUCCESSFUL run (over its tracked
    history, `internal/runs.Tracker.ListByHook`) is older than
    max(interval*3, 15m) — or that has never once succeeded, once enough
    time has passed for that to be meaningful. TIME-derived, not
    reload-derived: a dedicated ticker in cli/serve.go re-runs the check
    every minute (independent of any reload) and `ReplaceSource`s the
    result, because staleness is a function of elapsed time. This is the
    source that watches whether a hook's own reliability backstop is
    still backstopping anything — a scheduled tick that silently stops
    succeeding (a broken credential, a refusing dependency) looks, from
    the outside, identical to a hook with nothing to do, and every other
    source here only covers load-time or request-time defects. Clears
    the moment a run of that hook succeeds.
  Everything is IN-MEMORY (the events/requestLog stance): a restart
  re-derives the state sources at the boot load (their `since` resets to
  boot) and loses event-derived entries until their events recur. Entries
  are VALUE-FREE — name the hook, the `${NAME}` reference, the file;
  never a resolved secret value. The aggregator is nil-receiver-safe
  everywhere (the events.Recorder convention).
