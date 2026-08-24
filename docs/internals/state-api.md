# The state API (the Unix-socket KV surface)

Everything a `state: true` hook or a manager reaches over the state socket:
the KV store, the run-owned locks, declared waits, titles, spawn, backlogs and
the manager inbox. Extracted VERBATIM from `CLAUDE.md` when that file reached
the 40,000-character instruction-file budget; nothing here was condensed.

- **State KV API** — served on a **Unix socket** (NOT a TCP port), default
  `$TMPDIR/whr-state.sock`: `GET/PUT/DELETE /kv/{key}`, `GET /kv` (list),
  `POST /kv/{key}/incr`, the run-owned cooperative locks
  `POST /kv/{key}/acquire` (holder-identified 409 on contention; opt-in
  `{"block": true}` holds the request until acquired) /
  `POST /kv/{key}/release` / `POST /kv/{key}/steal` (transfer + cancel the
  holder — see the lock bullet under "Things easy to get wrong"), and the
  first-class declared sleep `POST /wait` (`{"seconds": 1..600, "reason":
  "..."}`, both required — blocks server-side, shows `waiting Ns: reason`
  on the run's dashboard row, counts as activity for the idle `timeout`;
  see the wait bullet under "Things easy to get wrong"), the shim-only
  instrumentation report `POST /phase/container-entry` (no body; the server
  stamps its receive time — the one lifecycle mark the host cannot see, and
  what separates docker's container-create cost from the hook runtime's
  cold start; docs/internals/run-phases.md), the friendly
  run-title override `POST /title` (`{"title":"..."}`, trimmed, 1..200
  chars — names the calling run mid-flight, replacing any run_title
  template title; see the run-title bullet under "Things easy to get
  wrong"), the pin toggle `POST /kv/{key}/pin` / `POST /kv/{key}/unpin`
  (owner-only, idempotent — a pinned lock refuses steals with 409 +
  `held_by.pinned:true`; acquire also takes `{"pinned":true}` for an
  atomic take-and-pin; see the pinning bullet under "Things easy to get
  wrong"), the spawn primitive `POST /spawn`
  (`{"hook","count":1..100,"payload":<JSON object ≤256KiB>}` + optional
  `"event"` — a MANAGER starts runs of ANOTHER hook through the
  runner itself, authorized deny-by-default by its own manager.json
  `spawn_targets` manifest; see the spawn bullet under "Things easy to get
  wrong"), the durable BATCH BACKLOGS `POST /backlog/{name}/push` (a set
  union that keeps order — re-push the whole candidate set every tick) /
  `POST /backlog/{name}/take` (`{"count":1..1000}`; REMOVES a slice,
  at-most-once, no leases) / `GET /backlog/{name}` / `GET /backlogs` (depths
  only) — WHAT IS LEFT for a run that can only afford part of the work, the
  sibling of internal/queue's WHEN-to-run scheduler (see
  docs/internals/backlogs.md), and — for MANAGERS only — the inbox long-poll
  `POST /inbox/next` (`{"wait_seconds":1..600}`; 200 = one event, 204 =
  none in time, 409 = superseded instance; calling again settles the
  previous event as processed — see the managers bullet under "Things
  easy to get wrong"; hooks 404 here). Hooks don't touch the socket directly: the runner
  injects a tiny proxy shim (webhook-runner's own binary, see `internal/kvproxy`)
  as the container entrypoint, so the hook reaches the API at a plain
  `http://localhost:9002` URL (`HOOK_KV_URL`) with any HTTP client — no
  networking, no `--unix-socket`. Each request is authenticated by the per-hook
  bearer token the runner injects, and the namespace is derived from that
  token, never from the URL — so a hook can only ever reach its own data.
  Backed by `internal/kv` (disk-backed under the data dir; see below). The
  `Server` struct exposes `StateHandler()` (served on the socket listener)
  alongside `HookHandler()`/`AdminHandler()`.
  The dashboard's one-time webhook-setup instructions live in a
  collapsed `<details>`; the page is about live state (hooks, images,
  runs, activity). The `events.Recorder` is a nil-safe bounded ring fed
  by the server (push webhooks, reloads, load errors, rejected hook
  requests: `hook.unknown` / `hook.denied` / `hook.misconfigured` — the
  last one names an unresolvable `${NAME}` api_key reference, logged to
  the feed but never to the 401 body) and the runner (image builds, run
  lifecycle)
  — memory only (run history, by contrast, persists completed runs via
  `internal/runstore`; see below). Rejections are events on purpose:
  the dashboard must be able to answer "did you receive anything?" —
  which is also why both feeds pass `?exclude=run` and show ONLY what has
  no run (the runs table owns run lifecycle). Every listing filters
  BEFORE the cap.

