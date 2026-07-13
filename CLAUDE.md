# webhook-runner — notes for Claude

## What this is

A self-contained Go HTTP server that executes incoming webhooks inside
disposable Docker containers. Each hook is described by a `hook.json` file
in its own folder under a single hooks directory. Hook definitions can
come from a local directory or be cloned from a Git repository.

## Project layout

```
cmd/webhook-runner/        binary entry point (calls into internal/cli)
internal/cli/              cobra commands (root = run server, validate, test, version)
internal/server/           HTTP handlers + routing (two muxes: hook + admin)
internal/server/dashboard/ embedded HTML dashboard (read views + the operator kill-switch controls); ts/ holds the runs-timeline adapter TypeScript that the go:generate one-liner in dashboard.go compiles via ts0 into the committed assets/timeline.js — the <timeline-view> component itself is NOT in this repo (the browser imports it at runtime from js-snippets' GitHub Pages; types = the component's real .d.ts pair, fetched from Pages by the same generate into the committed ts/js-snippets/)
internal/hooks/            hook.json model, loader, registry, watcher, git repo
internal/concurrency/      named concurrency groups (central concurrency.json) + semaphore manager (+ operator limit overrides)
internal/overrides/        operator kill switch: disabled hooks + concurrency limit overrides, persisted to <data-dir>/overrides.json
internal/scheduler/        per-hook "schedule" interval timer (pure timing; Fire callback dispatches the run)
internal/jsonc/            shared JSONC comment-stripping (hook.json + concurrency.json)
internal/runner/           docker run dispatch + output streaming + image build/status
internal/runs/             in-memory run tracker (bounded) + the OnFinish persistence seam
internal/runstore/         bbolt-backed persistent completed-run history (48h retention, GC sweeper)
internal/events/           in-memory activity feed (bounded ring; nil-recorder safe)
internal/kv/               disk-backed per-hook KV store (state socket) + HMAC namespace tokens
internal/kvproxy/          TCP->Unix proxy shim injected into state hooks (plain localhost URL)
internal/githubstatus/     GitHub commit status API client
schema/                    JSON schemas for hook.json + concurrency.json (published to GitHub Pages)
e2e/                       end-to-end test (shell script, requires Docker)
examples/hooks/            sample hook configs
```

## Conventions

- **Always use `go-toolchain`** from the repo root. Don't run bare `go build`,
  `go test`, or `go mod tidy`.
- **No CGO.** `CGO_ENABLED=0` is enforced by the Dockerfile build stage.
- **No Docker SDK.** Shell out to `docker` via `os/exec`.
- **Cobra subcommands** live one-per-file in `internal/cli/` and self-register
  via `init()`.
- **HTTP routing** uses Go 1.22+ `http.ServeMux` patterns (`POST /hook/{id}`).
  Don't add chi/gorilla/echo.
- **`$schema` is required.** Every `hook.json` must declare a `$schema`
  field (the `Hook.Schema` field); `Hook.validate` rejects a hook without
  one. The matching property lives in `schema/hook.schema.json`, which is
  published to GitHub Pages and is what the `$schema` URL points at. Keep
  the Go model, the JSON schema, and the example/e2e fixtures in sync.

## Architecture: two ports + a state socket

The server listens on two TCP ports plus a Unix socket:

- **Hook port** (`:9000`): `POST /hook/{id}`, `POST /hook/{id}/cancel/{run}`,
  `GET /health` (body carries the build version), `GET /version` (build
  identity: version + VCS revision/time — the same string the `version`
  command prints, plumbed from cli via `server.Options.Version`),
  `POST /_reload`. Public-facing, exposed via Cloudflare Tunnel.
- **Admin port** (`:9001`): dashboard, `/version` (build identity, same as
  the hook port's; the dashboard footer shows it), `/hooks`, `/hooks/{id}` (one hook's
  drill-down: value-free config summary — api_key as a boolean, env var
  names only, never any api_key/env/secret value, `skip_conditions` as a
  count — plus image state, KV namespace stats, and run stats over the live
  tracker window merged with the persisted run history; `stats.retention`
  labels that window and `stats.skipped` is the skip bucket — see the
  skip_if bullet under "Things easy to get wrong"),
  `/runs` (`?hook=` filters; live + persisted history, deduped by run ID,
  newest-first), `/runs/stream` (SSE live tail: `retry: 2000`, a connect `snapshot` shaped exactly like `/runs`, then one `run` event per lifecycle change + `hb` heartbeats ~10s; fed by the tracker's OnChange seam through a never-blocking hub — see "Things easy to get wrong"), `/runs/{id}/cancel`, `/reload`, `/events`
  (activity feed; `?hook=` filters on the `hook` field every hook-scoped
  event carries), `/images` (per-hook image state), the operator kill
  switch (`POST /hooks/{id}/disable|enable`,
  `PUT|DELETE /concurrency/{group}/limit` — see the overrides bullet under
  "Things easy to get wrong"), `/concurrency` (live per-group
  effective limit/declared/overridden/active/waiting), `/kv` (read-only state-store stats:
  per-namespace key count and bytes — shape unchanged, still value-free),
  `/kv/{namespace}` (one namespace's keys, sorted, `?prefix=` filters:
  name, size, and `expires_at` + remaining `ttl_seconds` when a TTL is
  set — the entry model tracks nothing else, so no created/updated
  stamps), and `/kv/{namespace}/{key}` (one entry **including its
  value**: `value_base64` always, `value_utf8` when the bytes are valid
  UTF-8; 404 on absent-or-expired via the same lazy-expiry rule as the
  state API). Exposing values on `/kv/{namespace}/{key}` is a
  **deliberate reversal** of the original "never values" stance, made at
  the operator's explicit request — the admin port is operator-only
  behind Zero Trust; the hook port and `/hooks/{id}` stay value-free
  (`/hooks/{id}`'s KV field remains the count/bytes summary). Internal,
  behind Cloudflare Zero Trust. The dashboard's `#hook={id}` fragment
  opens a per-hook "app" page built on those endpoints — an app is
  exactly one hook for now; grouping several hooks into one app is
  future work, which is why `/hooks/{id}` keeps a hook-scoped shape a
  grouping layer could aggregate. For `state: true` hooks that app page
  renders a "State (KV)" section: the key table (name, size, TTL
  remaining) with click-through to the stored value (pretty-printed when
  it parses as JSON, base64 for binary; text-node rendering, so stored
  bytes can't inject markup). The overview's PRIMARY runs view is a
  realtime swimlane timeline (`<timeline-view>`, canvas, one lane per
  hook, hue per hook): queue wait as a dim lead-in segment, declared
  waits/blocked locks/queued group acquires hatched (connectors from the
  waiter to every current holder; queued labels carry the position, holder
  labels a waiter-count badge — derived CLIENT-side by inverting
  waiting_on, because stream deltas never ship the server's waiters
  field), failures
  emphasized, cancelled hollow, instant runs as pips; wheel/drag
  pan + zoom, and panning into the past pages `/runs?before=` history
  down to retention (`/config`'s `run_retention` labels the boundary).
  A bar click opens the run modal, a lane-label click opens `#hook={id}`,
  and the old runs table stays behind a persisted "Show table" toggle.
  waiting_on/waiters and unknown statuses are feature-detected, so the
  timeline works against servers with or without first-class waits.
  Dashboard assets are content-addressed (`internal/server/
  dashboard` rewrites index.html to `/dashboard.<hash>.css|.js` +
  `/timeline.<hash>.js`, served
  immutable; `/` and the bare asset paths are no-cache, stale hashes 404)
  so an edge cache can never pair new HTML with stale assets.
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
  see the wait bullet under "Things easy to get wrong"), and the friendly
  run-title override `POST /title` (`{"title":"..."}`, trimmed, 1..200
  chars — names the calling run mid-flight, replacing any run_title
  template title; see the run-title bullet under "Things easy to get
  wrong"). Hooks don't touch the socket directly: the runner
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
  lifecycle, `env.unresolved` when an env reference expands to nothing)
  — memory only (run history, by contrast, persists completed runs via
  `internal/runstore`; see below). Rejections are events on purpose:
  the dashboard must be able to answer "did you receive anything?".

The `Server` struct has `HookHandler()` and `AdminHandler()` returning
separate `http.Handler`s. Tests use the `hook(s)` and `admin(s)` helpers.

## Hooks repo integration

When `WEBHOOK_RUNNER_HOOKS_REPO` is set, the server clones the repo on
startup (shallow, single-branch) into `WEBHOOK_RUNNER_HOOKS_DIR` (default
`/var/lib/webhook-runner/hooks`). `POST /_reload` on the hook port
accepts a GitHub push webhook (HMAC-SHA256 via `WEBHOOK_RUNNER_HOOKS_REPO_SECRET`)
and triggers `git fetch --depth=1` + `git reset --hard FETCH_HEAD` + reload.
The admin port's `POST /reload` does the same without auth.

For private repos, use an SSH URL (`git@github.com:...`). On first
startup, the server auto-generates an Ed25519 deploy key and logs the
public key. Add it to the repo's deploy keys on GitHub, then restart.
The key persists at `<hooks-dir>/../id_ed25519`.

The companion repo is `wow-look-at-my/webhooks`.

## Things easy to get wrong

- `runner.execute` deliberately uses `exec.Command` (not `CommandContext`)
  and kills the container by name on timeout. This is because if Go SIGKILLs
  the docker CLI process, the underlying container can survive briefly.
  We use `docker kill <name>` to stop the container reliably.
- Async webhook runs use `context.Background()`, NOT the request context.
  The HTTP request context is canceled the moment the client disconnects
  (which they do immediately after the 202), so anything tied to it would die.
- Run IDs are 16 random bytes, base32-lowercased to 26 chars. Anything that
  builds container names from them must keep that alphabet (`a-z2-7`) in mind.
- The watcher debounces events by 200ms; very rapid edits can collapse into
  a single reload.
- Cancellation is a *request*: `Run.RequestCancel()` closes a channel the
  runner's watcher goroutine selects on; the run only reaches the
  `cancelled` status once `docker kill <name>` has actually run and
  cmd.Wait returned. A cancel that races the container launch is covered
  twice — a pre-start check in `runner.execute`, and the watcher's select
  firing immediately on the already-closed channel.
- `${NAME}` references in hook.json (`env` values, `api_key`) are expanded
  at run/request time via `hooks.ExpandEnvRefs`, never at load time —
  `validate` in CI must pass without the production environment or keys.
  Resolution order: the hook's decrypted `secrets.sops.env` first, then
  the host environment. An `api_key` whose reference is unresolvable
  fails closed (401 for everyone).
- Per-hook sops secrets (`hooks.SecretsLoader`, `secrets.sops.env`)
  decrypt by exec'ing the `sops` binary (`WEBHOOK_RUNNER_SOPS_BIN`
  overrides; key material like `SOPS_AGE_KEY_FILE` is plain sops config
  on the service env), cached per file by mtime+size. Decrypted entries
  are also injected into the container env, with hook.json `env` winning
  on conflict (it's appended after, and docker keeps the last `-e`).
  Decrypt failures fail the run (status `error`) before the container
  starts — never run a secrets-bearing hook without its secrets. The e2e
  fixture key at `e2e/age-test-key.txt` is intentionally committed.
- Hooks trees have TWO layouts (internal/hooks/layout.go), detected by ONE
  rule — `<root>/src/hooks/` exists ⇒ src layout, else legacy — applied
  identically in serve/validate/test because they all load through
  hooks.LoadDir/LoadLayout. Under the src layout: hooks at
  src/hooks/<id>/, shared dependency-free code at src/sdk/ (imported
  relatively — ../../sdk/...), concurrency.json at
  src/config/concurrency.json (concurrency.LoadFile at the
  layout-resolved path; Load(root) is the legacy-only shorthand), and the
  docker build runs with CONTEXT src/ + the hook's own Dockerfile via -f
  (tree-mirror COPY convention: `COPY sdk/ /app/sdk/` +
  `COPY hooks/<id>/ /app/hooks/<id>/` + `WORKDIR /app/hooks/<id>` so the
  same relative import resolves in-repo and in-image). Layouts are NEVER
  mixed — root-level hook dirs under the src layout are skipped with a
  loud typed error (IgnoredLegacyDirError) naming each. Content hashing:
  legacy stays BYTE-IDENTICAL to the historical algorithm (golden-hash
  test — never change it, or every deployed hook re-tags on upgrade); the
  src layout hashes src/hooks/<id>/ AND src/sdk/ (src-relative path +
  mode + bytes, never sibling hooks), so an sdk edit re-tags every
  src-layout hook while hook A's edit never re-tags hook B; the COPY
  surface is therefore sdk/ + own hook dir ONLY (anything else in the
  context builds fine but never re-tags — undefined staleness, document
  don't debug). ZERO hooks loaded is a LOUD, typed failure
  (ZeroHooksError) in BOTH layouts: validate exits non-zero, serve logs +
  records it via the normal load-error event path every reload — the
  guard that stops a premature repo restructure from taking the fleet
  offline behind green CI. Layout detection re-runs on EVERY reload (a
  hooks-repo pull can restructure the tree); the watcher additionally
  watches src/, src/hooks/*, and src/config under the src layout (not
  src/sdk — sdk edits matter at image-build time, not reload time).
  SEQUENCING: the runner with this support deploys BEFORE the webhooks
  repo's src/ restructure lands — an old binary scanning a new tree loads
  zero hooks (now loud, still offline).
- Hook code is never mounted — it is immutable per run. Every hook ships
  a `Dockerfile` next to hook.json (the loader rejects hooks without one)
  and runs an image built lazily from the hook directory
  (`runner.EnsureImage`), tagged `whr-hook/<id>:<content-hash>`
  (`hooks.ContentHash`): a hooks-repo pull makes the *next* run rebuild,
  in-flight runs keep their image, superseded tags are best-effort
  deleted after a successful build, and a build failure fails the run
  (status `error`) before any container starts. There is no `image`
  field (the Dockerfile's FROM is the base); `command` optionally
  overrides the image's CMD. `validate` stays docker-free — builds
  happen only at run/test time. Only the per-run payload/headers files
  are mounted (data, not code).
- When the server itself runs in a container (the GHCR image + compose),
  per-run payload/header bind mounts resolve on the docker HOST — a temp
  dir private to the server's container doesn't exist there, docker
  creates a directory at the mount source, and every run fails with
  EISDIR reading its payload. `TMPDIR` must point at a dir bind-mounted
  from the host at the same absolute path; startup records a
  `server.misconfigured` event (and logs an error) when a container
  marker (/.dockerenv, /run/.containerenv) is present and TMPDIR is
  unset (`runner.WarnIfContainerized`, called from cli/serve.go — it
  lives in runner because the hazard is that package's mounting model).
  Image *builds* are immune — the docker CLI streams the build context
  over the socket.
- Hook test commands (hook.json `tests`, run by `webhook-runner test` via
  `runner.RunHookTests`) execute in the hook's built image (built first
  if needed), so tests exercise the exact baked bytes; copy test files
  into the image and set WORKDIR so relative paths resolve. Tests get NO
  payload, NO hook.json `env`, and NO secrets — they must be
  self-contained, which is what lets a hooks repo's CI run them without
  production keys. The per-command timeout (`--timeout`, default 10m) is
  deliberately independent of the hook's run `timeout` (sized for
  production work, not unit tests). The Dockerfile requirement, missing
  `image` field, and `tests` all need a runner binary with these
  semantics — `Parse` uses `DisallowUnknownFields` and old binaries
  demand `image`/`command` — so deploy webhook-runner before merging
  hooks that rely on them.
- `script` (hook.json) is parse-time sugar for `command`:
  `Hook.resolveScript` derives `<interpreter> <file> [args…]` (bash,
  pwsh, node, or tsx), resolving the file with `EvalSymlinks` and
  rejecting anything outside the hook directory. It sets nothing else —
  no image, no mounts: the script is baked into the hook's image like
  all code, so the interpreter must be installed in that image (the
  webhooks repo's `Dockerfile.common` base ships bash/node/tsx). An
  explicit `command` wins over `script`. New hook.json field ⇒ same
  deploy-first rule as `state`/`schedule`/`concurrency_group`.
- The run `timeout` is **activity-based, not wall-clock**: it kills a run
  only when the container has produced **no output** (stdout or stderr) for
  that long — "time out after N minutes of no activity". There is **no
  absolute processing ceiling anymore**: a run that keeps logging runs as
  long as it needs (the semantics exists because a healthy 47-part
  map-reduce run logging every ≤45s was killed by its 15m wall-clock
  `timeout`, while silence — not runtime — is the actual failure signal).
  Any output **byte** resets the clock: the runner wraps the pipe read side
  in a `touchReader` (internal/runner/watchdog.go), so even a long line
  without a newline counts. `DefaultTimeout` (5m) still applies when a hook
  omits `timeout` — now meaning 5 minutes of *silence*, so every hook keeps
  hang protection by default. The implementing `idleWatchdog` keeps the old
  arming rule: `Arm()` is called only after secrets decrypt, image build,
  and (crucially) the concurrency-group slot acquisition, once `cmd.Start`
  succeeded — a queued run stays `pending` with no clock ticking, and an
  unarmed watchdog never fires (that invariant is unit-tested; keep it).
  A timeout kill reuses the docker-kill-by-name path and ends the run as
  status `timeout` with error `timed out after <d> (no output)`; the
  `run.finished` event message carries that reason. `run.SetRunning()`
  (pending→running) still fires only once the container launches, so the
  dashboard shows queued runs as `pending` — and it stamps
  `RunState.StartedAt`, the queue-wait/processing split point (`started` in
  JSON stays the QUEUED/accepted instant for compatibility; waited =
  StartedAt−Started, duration = Finished−StartedAt, and a zero StartedAt
  means the run never started). The **sync hold** is the one place
  `hook.Timeout()` is still read as wall clock (`parseWaitParams` /
  `handleTrigger` in internal/server/handlers.go): a held HTTP response
  can't wait on activity, so the hold is a *response* bound, never a run
  bound — a chatty run legitimately outlives it and the response degrades
  to the async 202 while the run continues. (The short-lived `idle_timeout`
  field from #30 is REMOVED — `timeout` itself is the idle limit now, and
  `DisallowUnknownFields` means a hook.json still setting `idle_timeout`
  fails to load; nothing merged ever set it.) **Declared waits count as
  activity too**: while a state hook's `POST /wait` is in flight, the wait
  handler keeps touching the run's watchdog (see the wait bullet below), so
  an announced in-process sleep is never reaped as silence — only
  *undeclared* silence times out.
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
  token. Alongside the semaphores the Manager keeps ADVISORY queue
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
  hooks-repo config. The disable gate lives **at dispatch, not load**: a
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
  `/runs`, `/runs/{id}` (fallback for evicted runs), and `/hooks/{id}` stats
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
  first). At shutdown `srv.CloseStreams()` runs before the admin server's
  `Shutdown` — Shutdown drains in-flight handlers, and stream handlers
  only return when their subscription closes or their client hangs up.
  `streamHeartbeat` is a package var so tests can shrink it.
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
  bounded (64 KiB/value, 5000 keys/namespace, 256 namespaces by default —
  zero-valued `kv.Config` fields fall back to these in `kv.New`);
  `WEBHOOK_RUNNER_KV_MAX_KEYS` overrides the key cap, parsed in cli/serve.go
  like the other WEBHOOK_RUNNER_* env options.
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
  keep a dead lock alive). The table is **in-memory on purpose**: no run
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
- State hooks reach the KV API at a plain `http://localhost:9002` URL, NOT over
  networking — Docker has no native TCP→unix-socket forward, so webhook-runner
  runs the proxy itself. The KV server listens on a Unix socket at
  `$TMPDIR/whr-state.sock` (chmod 0666 so non-root hook users can connect; the
  bearer token, not file perms, is the real gate). For a state hook the runner
  sets the container entrypoint to webhook-runner's own binary (copied to
  `$TMPDIR/whr-shim` at startup by `copyExecutable`, bind-mounted in) invoked as
  the hidden `kv-forward` subcommand; that shim (`internal/kvproxy`) proxies
  `localhost:9002` → the bind-mounted socket, then execs the hook's real command
  (reconstructed from the image's entrypoint+cmd via `imageCommand`, or
  `hook.Command`). Both the socket and the shim MUST sit in the host-shared
  `TMPDIR` (same requirement as payload mounts) — so it works identically
  whether the server runs on the host or in a container. This was chosen after
  host-gateway and a shared Docker network proved more fragile; the shim keeps
  the hook's own networking intact (no netns sharing) and publishes no port.
  `WEBHOOK_RUNNER_STATE_SOCKET` overrides the socket path (must stay host-shared).
- The dashboard timeline splits in two: the **`<timeline-view>` component
  is consumed at RUNTIME from js-snippets' GitHub Pages** — the browser
  imports `https://wow-look-at-my.github.io/js-snippets/ui/timeline-view.js`
  (live at master head; the org's standard js-snippets consumption model,
  NEVER vendored copies) — while this repo ships only the runner-specific
  adapter. Component fixes deploy to this dashboard on js-snippets merge
  with no runner change; fix component bugs upstream in js-snippets, full
  stop. Consequences to keep straight: `assets/timeline.js` is a small
  ES-module adapter bundle whose component import passes through UNBUNDLED
  (ts0.json: esbuild `format: "esm"` + `external: ["https://*"]`) and is
  loaded via `<script type="module">` (after dashboard.js — modules defer,
  so its globals are always ready); the admin dashboard's chart therefore
  needs reach to wow-look-at-my.github.io at page load. A failed component
  fetch degrades softly and NEVER parks: the adapter module still runs,
  shows a "chart loading…" note in the Runs section, and retries the
  dynamic import on a FIXED 5s cadence forever (cache-busted `?retry=N`,
  because browsers can memoize a failed module fetch; no backoff, no
  attempt cap — see boot() in ts/timeline.ts), while dashboard.js's tables
  are untouched and the runs-table toggle keeps working. TypeScript types
  for the component come from the committed `ts/js-snippets/` — the
  component's REAL `.d.ts` pair (timeline-view + timeline-view-math),
  fetched VERBATIM from Pages at generate time; the adapter type-imports
  `./js-snippets/timeline-view.js` directly (type-only, erased — do NOT
  try an ambient `declare module '<url>'` bridge re-exporting the relative
  files, that's TS2439), and the runtime dynamic import of COMPONENT_URL
  needs no module declaration (its specifier is a widened string). The
  adapter is compiled by ts0 into the COMMITTED `assets/timeline.js`
  (go:embed needs it on a fresh clone; the bundle carries a DO-NOT-EDIT
  banner — never hand-edit it, edit ts/ and regenerate). Regeneration is
  the ONE-LINE `//go:generate` in dashboard.go: curl a PINNED ts0 build
  from buildhost (`?v=N`, never branch=latest) + the two `.d.ts` from
  Pages, then `node .cache/ts0.cjs build` (.cache/ is gitignored). It
  needs curl and Node 22+ — deliberately NO npm/npx and NO git auth (the
  previous npx pipeline and a Go-bootstrap rewrite were both scrapped for
  exactly that). Run it as `go-toolchain --generate <hash>` (bare
  `go-toolchain` prints the hash; ci.yml's `generate:` input carries the
  same one, with setup-node@v4/node 22 before the toolchain step and the
  freshness gate `git diff --exit-code -- internal/server/dashboard/assets/
  internal/server/dashboard/ts/js-snippets/` after — a stale bundle, stale
  fetched types, or upstream component API drift all fail CI). To bump the
  ts0 pin: change `?v=N` in the directive, then re-key the approval hash
  in ci.yml (any edit to the directive line — including the pin — or
  anything that shifts its file:line changes the hash; the bare run prints
  the new one).
