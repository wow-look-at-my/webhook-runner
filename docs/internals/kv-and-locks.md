# Gotchas: the KV store, cooperative locks, contention, and pinning

The disk-backed per-hook KV store, run-owned locks released by the finish seam, try/block/steal contention, and lock pinning.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- The per-hook KV store (`internal/kv`, the state socket) also persists to
  disk, under `WEBHOOK_RUNNER_DATA_DIR` (default: the hooks-dir parent, same
  place as the deploy key and `runs.db`) as one `kv/<namespace>.json` per
  hook plus a `state-secret` file. Writes are atomic (temp+rename) and a persist failure
  rolls the in-memory mutation back, so memory never diverges from disk —
  don't "optimize" by keeping an in-memory-only value on write failure or you
  break the survives-a-restart guarantee. A rolled-back write is loud
  end-to-end: the store returns the error, the state API surfaces it as a
  500 (body carries the reason), and the server logs it and records a
  `kv.write_failed` event on the activity feed (`Server.writeKVError` in
  internal/server/state.go) — never a quiet degrade. (The sweeper's persist
  failures are log-only inside `internal/kv` — it has no events.Recorder,
  and an expired-entry cleanup failing to flush is invisible to reads
  either way.) TTL is enforced lazily on read AND
  by a background sweeper (`StartSweeper`/`Close`); keep both. The store is
  bounded per item (64 KiB/value, 256 namespaces by default — zero-valued
  `kv.Config` fields fall back to these in `kv.New`) but has NO key-count
  cap: total growth is deliberately uncapped in-process (operator directive)
  and bounded at the container level (a memory cap; swap-side only where the
  host kernel does cgroup swap accounting) instead — don't
  reintroduce a cap, usage detection, or eviction in its place.
- A hook opts into the store with `state: true`. `state` is a new hook.json
  field, so `Parse`'s `DisallowUnknownFields` means old binaries reject it —
  same deploy-first rule as `concurrency_group`. ONLY for state hooks, the
  runner bind-mounts the KV socket + the proxy shim, sets the shim as the
  container `--entrypoint`, and injects `HOOK_KV_SOCKET`, `HOOK_KV_URL`
  (`http://localhost:9002`), and `HOOK_KV_TOKEN` (all `ReservedEnvKey`). The
  token is a stateless HMAC over the hook ID AND the run ID
  (`kv.Token(ns, runID)`/`VerifyToken` returning both) — namespace == hook
  ID, minted per run, nothing to store or expire. The run identity in the
  token is what binds cooperative locks to their holding run; the retired
  two-part (namespace-only) format no longer verifies, which is fine
  because tokens never outlive their run.
- Cooperative locks (`internal/kv/lock.go`, `POST /kv/{key}/acquire` /
  `/release`) are **owned by run instances, not by client-managed tokens**:
  the state token carries the run ID, acquire/release are atomic under the
  lock table's own mutex (the compare-and-set/compare-and-delete a hook
  could never build from GET+PUT), and the **primary** release mechanism is
  the run tracker's OnFinish seam — `server.RunFinishCallback` (wired in
  cli/serve.go; it lives in internal/server, beside the lock handlers, so
  the cli package stays test-free) calls `kv.ReleaseRunLocks(runID)`
  BEFORE the runstore write, so a run
  that ends for ANY reason (success, error, timeout kill, cancel — Finish
  fires exactly once on every terminal path) drops all its locks even if
  the history write fails; leftovers surface as a `lock.released_on_finish`
  event. The TTL is a SECONDARY backstop only (default `kv.DefaultLockTTL`
  15m, explicit `ttl_seconds` 1..3600) against a release-path bug — never
  the liveness story — and a CONTENDED acquire mutates nothing (in
  particular it never restamps the holder's expiry, so contenders can't
  keep a dead lock alive). **Expiry frees nothing by itself** — it is
  ENFORCED against the holder; see the TTL-enforcement bullet below. The
  table is **in-memory on purpose**: no run
  survives a server restart, so a restart correctly starts lock-free —
  don't "fix" that by persisting locks. Locks are NOT entries: they never
  appear in GET/PUT/DELETE/list, the namespace files, or the admin KV
  views; the same key string can hold a value and a lock independently.
  Same-run re-acquire is idempotent (refreshes the backstop, keeps
  acquiredAt); cross-run release is refused server-side (409). Deploy-first
  rule as usual: hooks that call acquire/release need this runner deployed
  first — older runners 404 the routes (hooks should treat 404/405 as
  "primitive unavailable" and degrade, not wedge).
- Lock contention is first-class, never anonymous (the try/block/steal
  layer over the bullet above; `takeLockLocked` in internal/kv/lock.go is
  the ONE compare-and-set acquire and steal share):
  (1) **Try**: a contended acquire 409s with `held_by`
  ({run_id, hook_id, acquired_at, expires_at} — hook_id == the lock's
  namespace) so a contender can display, keep waiting on, or steal from a
  NAMED holder; the contended path still mutates nothing.
  (2) **Block**: acquire with `{"block": true}` (+ optional
  `block_timeout_seconds` 1..600, default 600 — the /wait cap) HOLDS the
  request, retrying every `lockRetryInterval` (250ms, tightened to the
  /wait touch cadence for tiny timeouts) until taken / timed out (409 +
  held_by) / the run ends. Fairness is deliberately best-effort — NO FIFO
  queue, waiters just poll — which keeps the lock table free of waiter
  state and lets a steal trivially beat every blocked waiter (they keep
  polling against the new holder). While blocked, the run's watchdog is
  fed (a blocked acquire is a declared wait) and its `waiting_on` names
  the holder, re-stamped when the lock changes hands mid-block.
  (3) **Steal** (`POST /kv/{key}/steal` — a separate route, NOT an acquire
  flag, so the destructive intent is unmistakable): atomically TRANSFERS
  the lock to the caller under the table mutex, then the server cancels
  the displaced run via the tracker (`RequestCancelWithReason`, riding the
  existing docker-kill cancel path; the reason — `cancelled: lock "k"
  stolen by run X` — becomes the victim's terminal error, visible in run
  history). Transfer-not-release is the race-safety invariant: after a
  steal the entry is owned by the thief, so the victim's finish-seam
  `ReleaseRunLocks` frees its OTHER locks but skips the stolen one; a
  holder that finished FIRST just makes steal a plain acquire (no error,
  no cancel, no `stolen_from` in the response). Namespace scoping means a
  run can only ever steal from — and thus cancel — runs of its OWN hook.
  Events: `lock.waiting` once per blocking acquire that actually waits,
  `lock.stolen` on displacement. Dashboard: the blocked run's row shows
  "waiting on lock K held by RUN (HOOK)" (from `waiting_on`), and holders
  carry a derived `waiters` list ("N waiting on this run's locks") —
  computed by `server.attachWaiters` from live runs' `waiting_on` at READ
  time, never stored; don't add waiter state to the lock table.
- **TTL enforcement — kill, confirm, THEN hand over** (operator ruling,
  verbatim: *"never have a TTL on a mutex, that doesn't make sense. Or, if
  you want to have a mutex TTL, you need to force kill the thing that's
  holding it when the time is up. ONCE THAT FORCE KILL COMPLETES AND THAT
  JOB IS CERTAIN TO BE DEAD, then the mutex would be freed automatically
  due to the ending job, and the TTL has been enforced."*). An expired
  lock must never read as FREE at acquire time: that is liveness-blind, so
  any hold outliving its TTL silently becomes two holders. The store keeps
  the entry (`kv.ErrLockExpired` + the holder's info, mutating nothing) and
  `Server.enforceLockTTL` does the enforcing: cancel the holder
  (`RequestCancelWithReason`, the same docker-kill path steal uses), poll
  until that run is TERMINAL, and take the lock its finish seam then
  freed. `takeLock` is the single seam every acquire path uses (immediate
  + both blocking loops), so none of them can disagree. The refusals are
  the point — the lock is never handed over on a guess:
  holder still dying past `lockKillTimeout` (30s) → 409; holder is a live
  MANAGER INSTANCE (supervised, restart-on-exit — not this endpoint's to
  kill) → 409; no run tracker wired → 409. The one no-kill path is a
  holder already CONFIRMED gone (the release-path bug the backstop exists
  for): `kv.ReapExpiredLock` drops that exact expired shell — refusing if
  the entry changed hands, is unexpired, or names a different holder — and
  the acquire proceeds. Events: `lock.ttl_enforced` (holder cancelled),
  `lock.ttl_reaped` (dead holder's shell reclaimed). The sweeper follows
  the same rule via `kv.SetRunLiveness` (wired in cli/serve.go to the run
  tracker + `managers.AnyCurrentInstance`): it reaps an expired lock only
  when the holder is certainly dead, and reaps NOTHING when no oracle is
  wired — an unwired store cannot tell "dead" from "slow". A steal still
  displaces an expired holder (pinned or not): a steal IS the kill path.
- Lock pinning (`internal/kv/lock.go` `pinned` + `internal/server/state.go`
  pin/unpin routes): a lock HOLDER can flip its lock not-stealable
  (`POST /kv/{key}/pin`) and back (`/unpin`), or take-and-pin atomically
  (`{"pinned":true}` on acquire — honored mid-blocking-retry too). A
  steal of a live pinned lock mutates NOTHING: 409 with
  `held_by.pinned:true` + a `lock.steal_refused` event, and the refused
  contender's correct fallback is a blocking acquire (which still wins on
  release — latest-event-wins survives, it just waits out the pinned
  critical section instead of interrupting it). Owner-only in both
  directions (409 otherwise), idempotent, `ErrLockPinned` maps to 409.
  THE INVARIANT: a pin NEVER outlives its run — the finish-seam
  `ReleaseRunLocks` and the TTL backstop apply to pinned locks unchanged
  (pinning restricts STEALING, never releasing; past the backstop a pinned
  lock is stealable again, and a plain acquire enforces the TTL against it
  like any other), and a re-acquire by the same run preserves an existing
  pin. The single-instance manager lease
  is deliberately NOT built on an always-pinned lock — it stays the
  separate kernel-flock layer (operator ruling; composition argument in
  docs/manager-entity-design.md 10b).
