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
internal/server/dashboard/ embedded read-only HTML dashboard
internal/hooks/            hook.json model, loader, registry, watcher, git repo
internal/concurrency/      named concurrency groups (central concurrency.json) + semaphore manager
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
  names only, never any api_key/env/secret value — plus image state, KV
  namespace stats, and run stats over the live tracker window merged with
  the persisted run history; `stats.retention` labels that window),
  `/runs` (`?hook=` filters; live + persisted history, deduped by run ID,
  newest-first), `/runs/{id}/cancel`, `/reload`, `/events`
  (activity feed; `?hook=` filters on the `hook` field every hook-scoped
  event carries), `/images` (per-hook image state), `/concurrency` (live
  per-group limit/active/waiting), `/kv` (read-only state-store stats:
  per-namespace key count and bytes, never values). Internal, behind
  Cloudflare Zero Trust. The dashboard's `#hook={id}` fragment opens a
  per-hook "app" page built on those endpoints — an app is exactly one
  hook for now; grouping several hooks into one app is future work, which
  is why `/hooks/{id}` keeps a hook-scoped shape a grouping layer could
  aggregate. Dashboard assets are content-addressed (`internal/server/
  dashboard` rewrites index.html to `/dashboard.<hash>.css|.js`, served
  immutable; `/` and the bare asset paths are no-cache, stale hashes 404)
  so an edge cache can never pair new HTML with stale assets.
- **State KV API** — served on a **Unix socket** (NOT a TCP port), default
  `$TMPDIR/whr-state.sock`: `GET/PUT/DELETE /kv/{key}`, `GET /kv` (list),
  `POST /kv/{key}/incr`. Hooks don't touch the socket directly: the runner
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
  webhook-runner before any hook sets it.
- Concurrency groups (`internal/concurrency`) are declared centrally in
  `concurrency.json` at the hooks root, NOT per-hook: a hook only references
  a group by name via `concurrency_group`, and referencing an undeclared
  group is a load/validation error (the hook is dropped, not run unbounded —
  fail closed). The `concurrency.Manager` holds one buffered-channel
  semaphore per group; `Acquire` captures the channel in its release closure
  so a reload that swaps a group's semaphore can't lose or double-count a
  token. `concurrency_group` is a new hook.json field (so `Parse`'s
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
  alone), so GC and newest-first reads are single cursor walks; the per-hook
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
- The per-hook KV store (`internal/kv`, the state socket) also persists to
  disk, under `WEBHOOK_RUNNER_DATA_DIR` (default: the hooks-dir parent, same
  place as the deploy key and `runs.db`) as one `kv/<namespace>.json` per
  hook plus a `state-secret` file. Writes are atomic (temp+rename) and a persist failure
  rolls the in-memory mutation back, so memory never diverges from disk —
  don't "optimize" by keeping an in-memory-only value on write failure or you
  break the survives-a-restart guarantee. TTL is enforced lazily on read AND
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
  token is a stateless HMAC over the hook ID (`kv.Token`/`VerifyToken`) —
  namespace == hook ID, minted per run, nothing to store or expire.
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
