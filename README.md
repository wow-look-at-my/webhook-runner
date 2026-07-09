# webhook-runner

A self-contained Go HTTP server that executes incoming webhooks inside
disposable Docker containers. Each webhook is described by a `hook.json`
file in its own folder; the server watches the directory and hot-reloads
hooks without restart.

## Features

- **Folder-per-hook config**, parsed with JSONC-style comments.
- **Three authentication methods**: API key (recommended), Ed25519 public
  key signatures, and legacy HMAC-SHA256. At most one per hook.
- **Disposable containers**: every run is `docker run --rm ...` with the
  request body and headers bind-mounted as files.
- **Hot reload**: filesystem watch picks up new, modified, and removed
  `hook.json` files immediately.
- **Separate hook and admin ports**: the hook port (`:9000`) handles
  incoming webhooks and can be exposed publicly; the admin port (`:9001`)
  serves the dashboard, hook list, and run history and should be placed
  behind authentication (e.g. Cloudflare Zero Trust).
- **Stateful hooks (KV store)**: a hook can opt into a small persistent
  key/value store with `"state": true` — making webhook-runner almost a
  simple serverless platform. The hook reaches it at a plain
  `http://localhost:9002` URL (`HOOK_KV_URL`) with a scoped `HOOK_KV_TOKEN` —
  any HTTP client, no networking (webhook-runner injects a proxy shim that
  bridges that port to an internal Unix socket). get/put/delete/list, atomic
  increment, and per-key TTL. Data is disk-backed (survives restarts),
  bounded, and isolated per hook.
- **Git-backed hooks**: point at a Git repository with
  `WEBHOOK_RUNNER_HOOKS_REPO` and the server clones it on startup.
  Configure a GitHub push webhook to `POST /_reload` to auto-pull on push.
- **GitHub commit status integration**: optional per-hook; posts
  `pending` on start and `success`/`failure`/`error` on exit.
- **Sync and async invocation**: every hook returns a unique 128-bit
  run ID; clients can poll `GET /runs/{id}` or use `?wait=true` to block
  on the response.
- **Cancellation**: `POST /hook/{id}/cancel/{run}` kills an in-flight
  run's container (authenticated like the hook itself), so async callers
  can supersede stale work.
- **Activity-based timeout**: `timeout` kills a run only after it has
  produced no output for that long (any output byte resets the clock) —
  so long-but-chatty work survives while hung work is reaped. See
  [Timeouts](#timeouts).
- **Immutable hook code**: every hook ships a `Dockerfile` next to its
  `hook.json` and runs an image webhook-runner builds from the hook
  directory, tagged by content hash — code is baked in, a hooks-repo pull
  can't change an in-flight run, and runs are plain
  `docker run --rm <image>`.
- **Hooks ship their own tests**: a `tests` array in `hook.json` declares
  test commands; `webhook-runner test <hooks-dir>` runs each one in the
  hook's built image, so CI never hardcodes per-hook test invocations.
- **Secrets without plaintext**: `env` values and `api_key` may reference
  secrets as `${NAME}`, resolved from a per-hook sops-encrypted file
  committed to the hooks repo (`secrets.sops.env`) or from the runner
  host's environment.
- **Scheduled hooks**: a `"schedule"` interval (a Go duration) fires a hook
  on a timer through the same run pipeline as an HTTP trigger, with
  skip-if-already-running overlap protection and fire-on-startup. See
  [Scheduled hooks](#scheduled-hooks).
- **Concurrency**: no global queue, each request fires its own container.
- **Dashboard**: read-only HTML view at `/` on the admin port showing the
  full internal state — loaded hooks, per-hook image status (built /
  will-build-next-run, images on disk), recent runs, and a live activity
  feed (GitHub push webhooks received, git pulls, reloads, load errors,
  image builds, run lifecycle — and rejected requests: unknown hook ids,
  denied auth, unresolvable `${NAME}` references in `api_key`/`env`, so
  "did you receive anything?" always has an answer). One-time setup
  instructions stay collapsed. Opening a run shows its output with a
  per-line timestamp column (the raw view) or per-turn times (the
  conversation view), plus a **Copy log** button that puts the whole
  timestamped log on the clipboard. Every hook also has its own
  drill-down page (`/#hook={id}`, linked from the hooks list) with its
  config summary, run stats, image state, runs, activity slice, and —
  for `state: true` hooks — a **State (KV)** section listing the hook's
  stored keys (size, TTL remaining) where clicking a key shows its
  stored value (pretty-printed when it's JSON) — a per-"app" view,
  where an app is one hook for now. Run timing is split
  into queue wait and processing time: the runs table shows when a run
  was queued, how long it **Waited** for its concurrency slot, and a
  **Duration** that covers container time only, and the stats keep
  avg/max wait separate from avg/max duration.
- **Persistent run history**: completed runs are written once, at their
  terminal status, to a single bbolt file under the data dir; `/runs`,
  `/runs/{id}`, and the drill-down stats serve the live tracker merged
  with that history, so runs and their output survive restarts.
  Time-based retention (default 48h, `WEBHOOK_RUNNER_RUN_RETENTION`)
  with a per-hook count cap as a disk safety net
  (`WEBHOOK_RUNNER_RUN_RETENTION_MAX`). Runs still in flight during a
  restart never completed and are not in the store.
- **Static binary, alpine runtime image** with `docker-cli` and `git`
  for shelling out — no Docker SDK dependency.

## Quick start

```sh
# Build
go-toolchain
./webhook-runner ./examples/hooks

# Or with the bundled image
docker build -t webhook-runner .
docker run --rm \
  -p 9000:9000 -p 9001:9001 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v $PWD/examples/hooks:/hooks:ro \
  webhook-runner /hooks

# Or with a private Git-backed hooks repo
# On first run, webhook-runner generates an SSH deploy key and prints
# the public key to logs. Add it to your repo's deploy keys, then restart.
docker run --rm \
  -p 9000:9000 -p 9001:9001 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v webhook-runner-data:/var/lib/webhook-runner \
  -e WEBHOOK_RUNNER_HOOKS_REPO=git@github.com:you/your-private-hooks.git \
  -e WEBHOOK_RUNNER_HOOKS_REPO_SECRET=your-webhook-secret \
  webhook-runner
```

Then trigger a hook:

```sh
curl -X POST -d '{"foo":"bar"}' http://localhost:9000/hook/deploy-frontend
# { "run_id": "abqkr2f6mfjrtgsihpnz5rdgye" }

# Runs and dashboard are on the admin port
curl http://localhost:9001/runs/abqkr2f6mfjrtgsihpnz5rdgye
```

## HTTP API

The server listens on two TCP ports. The **hook port** (default `:9000`) should
be publicly accessible (e.g. via a Cloudflare Tunnel). The **admin port**
(default `:9001`) should be behind authentication (e.g. Cloudflare Zero Trust).
The per-hook KV store is reached by hooks at a plain `http://localhost:9002`
URL (backed by an internal Unix socket; see below), not a public port.

### Hook port (`:9000`)

| Method | Path                | Purpose                                    |
|--------|---------------------|--------------------------------------------|
| GET    | `/health`           | Liveness probe (200). Body carries the build version: `{"status":"ok","version":"..."}`. |
| GET    | `/version`          | Build identity: `{"version","revision","time"}` — the same string `webhook-runner version` prints, plus the VCS commit/time when the build has them. |
| POST   | `/hook/{id}`        | Trigger a hook. Body becomes `HOOK_PAYLOAD_FILE`. |
| POST   | `/hook/{id}/cancel/{run}` | Cancel an in-flight run of this hook (same auth as triggering it). |
| POST   | `/_reload`          | Pull hooks repo and reload (HMAC auth, requires `WEBHOOK_RUNNER_HOOKS_REPO_SECRET`). |

### Admin port (`:9001`)

| Method | Path                | Purpose                                    |
|--------|---------------------|--------------------------------------------|
| GET    | `/health`           | Liveness probe (200). Body carries the build version. |
| GET    | `/version`          | Build identity (same shape as on the hook port). Shown in the dashboard footer. |
| GET    | `/hooks`            | List loaded hooks (id + description).      |
| GET    | `/hooks/{id}`       | One hook's drill-down: a value-free config summary (schedule, concurrency group, state on/off, timeout, whether an api_key is configured as a boolean, env var *names* — never key material or env values), its image state, its KV namespace stats, and run stats (counts by status, success rate, avg/max **processing** duration and avg/max **queue wait** — kept separate, see `/runs` — plus last run) over the live window merged with the persisted run history (`stats.retention` names the window, e.g. `48h`). |
| POST   | `/hook/{id}`        | Trigger a hook (also available here).      |
| POST   | `/hook/{id}/cancel/{run}` | Cancel a run (also available here).  |
| GET    | `/runs`             | Runs across all hooks, newest-first: live (active + recent) merged with the persisted completed history, deduped by run ID; `?hook={id}` narrows to one hook, `?max=` caps the page (default 100). Each run carries `started` (when it was accepted/queued) and, separately, `started_at` (when its container actually launched — absent while pending, or if it never started), so queue wait (`started`→`started_at`) and processing time (`started_at`→`finished`) never blur together. |
| GET    | `/runs/{id}`        | Status + retained output for one run — served from the live tracker, falling back to the persisted history for runs evicted from it or finished before a restart. |
| POST   | `/runs/{id}/cancel` | Cancel any run (no auth — admin port is trusted). |
| POST   | `/reload`           | Pull hooks repo and reload (no auth — admin port is trusted). |
| GET    | `/events`           | Activity feed: GitHub push webhooks, git pulls, hook (re)loads and load errors, image builds, run lifecycle (including `run.queued` when a run waits for a concurrency slot), rejected requests (`hook.unknown`, `hook.denied`, `hook.misconfigured`) and unresolved env references (`env.unresolved`). Newest first; `?max=` caps it, `?hook={id}` narrows to one hook's slice. |
| GET    | `/images`           | Per-hook image state: the tag the current content resolves to, whether it's built (false = next run builds it), and every `whr-hook/*` image on disk. |
| GET    | `/concurrency`      | Live state of every declared concurrency group: its `limit`, how many runs are `active`, and how many are `waiting` (queued) behind it. |
| GET    | `/kv`               | Read-only state-store stats: per-namespace key count and byte total (no values at this level; shape unchanged for existing consumers). |
| GET    | `/kv/{namespace}`   | List one namespace's keys (namespace == hook ID): per key its name, value size in bytes, and — when a TTL is set — `expires_at` (absolute) plus `ttl_seconds` (remaining); both absent for keys without a TTL. Sorted by key; `?prefix=` filters. Unknown/empty namespaces list as empty. |
| GET    | `/kv/{namespace}/{key}` | Read one entry: the metadata above **plus the stored value** — `value_base64` always, `value_utf8` additionally when the bytes are valid UTF-8. `404` when absent **or expired** (the same lazy-expiry rule the state API applies). The key is one path segment: URL-encode it (`%2F` for `/`, `%23` for `#`). |
| GET    | `/`                 | Dashboard; `/#hook={id}` opens a hook's drill-down page. |

The dashboard's static assets are content-addressed: the served index.html
references `/dashboard.<hash>.css|.js` (hash of the embedded bytes), which are
cacheable forever (`Cache-Control: immutable` + ETag) — a new build changes
the URLs. `/`, the bare `/dashboard.css|.js` paths, and any stale-hash URL
(404) are `no-cache`, so an edge cache (e.g. Cloudflare, which caches
`.css`/`.js` by extension when the origin sends no cache headers) can never
pair a new index.html with stale assets after a deploy.

### State KV API (`http://localhost:9002` in state hooks)

The per-hook KV store, consumed by hook containers at a plain
`http://localhost:9002` URL (`HOOK_KV_URL`). There is no network and no public
port: webhook-runner serves the API on an internal Unix socket and injects a
tiny proxy shim as the hook's entrypoint that bridges `localhost:9002` to it
(see *Stateful hooks* below). Every request carries the bearer token the runner
injects as `HOOK_KV_TOKEN` (`Authorization: Bearer <token>`); the namespace is
derived from the token, never from the URL, so a hook can only ever reach its
own data.

| Method | Path             | Purpose                                                        |
|--------|------------------|----------------------------------------------------------------|
| GET    | `/kv/{key}`      | Read a value (`200` raw bytes, `404` if absent/expired).       |
| PUT    | `/kv/{key}`      | Store a value (body = raw bytes). Optional TTL via `X-KV-TTL: <seconds>` header or `?ttl=<seconds>`. `204` on success, `413` over the value cap. |
| DELETE | `/kv/{key}`      | Remove a key (idempotent `204`).                               |
| GET    | `/kv`            | List the caller's keys: `{"keys":[...]}` (sorted, non-expired). |
| POST   | `/kv/{key}/incr` | Atomically add to an integer counter. Optional body `{"delta":N}` (default `+1`) and TTL as for PUT. Returns `{"value":<int64>}`; `409` if the existing value isn't an integer. |

### Sync vs async

By default, `POST /hook/{id}` returns `202 Accepted` with `{"run_id": "..."}`
as soon as the container has been spawned. To block until the run finishes,
add `?wait=true`. Combine with `?timeout=30s` to cap how long the server
holds the connection (the run continues in the background if the sync
timeout elapses first).

A hook can opt into sync-by-default by setting `"synchronous": true`
in `hook.json`. The query parameter still wins.

### Cancelling a run

`POST /hook/{id}/cancel/{run}` asks the runner to kill an in-flight run's
container. It is authenticated exactly like triggering the hook (same
api_key / signature), and a hook's credentials can only cancel that hook's
own runs. Responses:

- `202` `{"run_id": "...", "status": "cancelling"}` — cancel requested; the
  run reaches status `cancelled` once the container is actually gone.
- `409` — the run already finished (body carries its final status).
- `404` — unknown hook or run (including runs belonging to another hook).

This is what lets a fire-and-forget caller supersede stale work: kick off a
run, remember the `run_id` from the 202, and cancel it if a newer event
makes its result obsolete (e.g. pr-minder cancelling an in-flight
PR-describe run when new commits arrive).

## Stateful hooks (KV store)

Hook containers are disposable and have no memory of their own. Set
`"state": true` in a hook's `hook.json` to give it a small persistent
key/value store — enough to count invocations, dedupe events, cache a token,
or carry anything else across runs. This is what makes webhook-runner almost
a simple serverless platform.

When a state hook runs, the runner injects two environment variables (and a
proxy shim as the container entrypoint that makes `localhost:9002` work):

- `HOOK_KV_URL` — base URL of the KV API (`http://localhost:9002`).
- `HOOK_KV_TOKEN` — a bearer token scoped to **this hook's** namespace.

The hook just makes plain HTTP calls with any client:

```sh
# Increment a counter and read the running total (atomic, race-free)
curl -fsS -XPOST -H "Authorization: Bearer $HOOK_KV_TOKEN" "$HOOK_KV_URL/kv/hits/incr"
# {"value":1}

# Put a value with a 1-hour TTL, then read it back
curl -fsS -XPUT -H "Authorization: Bearer $HOOK_KV_TOKEN" -H "X-KV-TTL: 3600" \
  --data-binary @- "$HOOK_KV_URL/kv/last-seen" <<<"$COMMIT_SHA"
curl -fsS -H "Authorization: Bearer $HOOK_KV_TOKEN" "$HOOK_KV_URL/kv/last-seen"
```

It's an ordinary HTTP endpoint, so any language's standard client works — no
Unix-socket support needed. (Under the hood, webhook-runner serves the API on
an internal Unix socket and sets its own binary as the hook's entrypoint to
proxy `localhost:9002` to it, then runs the hook's real command.)

Properties:

- **Durable**: data is written to `<data-dir>/kv/<hook-id>.json` (atomic
  temp+rename) and survives server restarts.
- **Isolated**: the namespace comes from the verified token, never the URL —
  a hook can only ever read and write its own data.
- **Internal**: no network and no published port — the API is an internal Unix
  socket reached only through the injected localhost proxy.
- **Bounded**: per-value size, keys-per-hook (default 5000,
  `WEBHOOK_RUNNER_KV_MAX_KEYS` overrides), and namespace-count caps keep a
  runaway hook from exhausting disk (oversize writes get `413`).
- **TTL**: any `PUT`/`incr` may set a per-key expiry (`X-KV-TTL` seconds or
  `?ttl=`); expired keys disappear from reads and are swept from disk.

See the State KV API table above for the full endpoint list. On the admin
port, `GET /kv` shows per-hook key counts and byte totals, and the inspection
routes `GET /kv/{namespace}` / `GET /kv/{namespace}/{key}` list a hook's keys
(with sizes and TTLs) and read stored **values** — also surfaced on the
dashboard as a *State (KV)* section on each state hook's page (click a key to
view its value). Exposing values on the admin port is deliberate: whatever a
hook stores becomes readable there, so keep that port operator-only (Zero
Trust — the same trust the un-authenticated `/reload` and run cancellation
already assume). Values never appear on the public hook port, and
`/hooks/{id}` stays value-free.

> **Deploy-first:** `state` is a newer `hook.json` field, so deploy a
> webhook-runner build that understands it before any hook sets `"state":
> true` (older binaries reject unknown fields). The KV socket and the proxy
> shim live under `TMPDIR`, which must be host-shared when the server runs in
> a container — the same requirement payload files already have; see *Running
> the server in a container*.

## hook.json reference

The full schema is published at
`https://wow-look-at-my.github.io/webhook-runner/hook.schema.json`.
See `examples/hooks/` for working examples.

Every `hook.json` **must** declare a `$schema` field pointing at that URL
(alongside the required `Dockerfile` that bakes the hook's code into its
image):

```json
{
  "$schema": "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json",
  "command": ["sh", "-c", "echo hi"]
}
```

This is required: the server and `webhook-runner validate` both reject a
hook whose `hook.json` is missing `$schema`. Declaring it lets editors and
CI (e.g. [json-validator](https://github.com/wow-look-at-my/json-validator))
validate the file against the published schema.

Two values support `${NAME}` secret references:

- `env` values — expanded when the container starts. Unresolvable names
  expand to `""` with a logged warning.
- `api_key` — expanded on every request. If the reference is unresolvable,
  the hook **fails closed** (every request is rejected with 401).

`${NAME}` resolves against the hook's decrypted `secrets.sops.env` first
(see below), then the runner host's environment — so a secret can start
life as a host env var and move into the repo without touching hook.json.
Only the braced `${NAME}` form is expanded; a bare `$NAME` passes through
untouched. Expansion never happens at load/validate time, so CI validation
needs neither the production environment nor any decryption keys.

## Timeouts

One per-hook knob, `timeout` (default `5m`), and it is **activity-based,
not wall-clock**: the run is killed only once its container has produced
**no output** (stdout or stderr) for that long. Any output byte resets the
clock, so a hook that keeps logging progress runs as long as it needs —
there is **no absolute processing ceiling** — while one that has gone
silent is reaped within `timeout` of its last output. The kill goes through
`docker kill`, ends the run with status `timeout`, and carries the error
message `timed out after <d> (no output)` (in `/runs/{id}` and the
activity feed).

This semantics exists because wall-clock ceilings kill healthy work: a
47-part map-reduce hook that logged every ≤45s was cut down mid-progress
by its 15-minute total timeout, while a genuinely hung run and a healthy
long run are indistinguishable by wall clock alone. Silence is the actual
failure signal — so `"timeout": "15m"` now means "kill it after 15 minutes
of no activity", which lets the long healthy run finish and still reaps a
hung one promptly.

The clock arms **at container launch**: never while the run is queued
behind a [concurrency group](#concurrency-groups), decrypting secrets, or
building its image — a queued run cannot time out.

For synchronous requests (`?wait=true` or `"synchronous": true`), the
hook's `timeout` value also serves as the default bound on how long the
server holds the HTTP response open. That hold is necessarily wall-clock —
a response can't wait on activity — and it bounds only the **response**,
never the run: a run that outlives it degrades to the async `202` and
keeps running in the background.

## Concurrency groups

By default a hook runs with unbounded concurrency: every accepted request
spawns its container immediately. When a burst arrives — or several hooks
share one scarce backend (a single local model server, a rate-limited API)
— that means many containers competing at once, and, worse, **every run's
timeout clock is live from container launch** while a run stuck waiting for
the backend produces no output — so runs that are really just *waiting* can
time out before they ever do work.

Concurrency groups fix both. A group is a named slot pool with a limit; at
most `limit` runs in the group execute at once and the rest **queue**
(staying `pending`, not `running`). A queued run's `timeout` clock does not
start until it actually begins processing — queue time is never counted.

Unlike GitHub Actions' free-form `concurrency:` expression, the set of valid
groups is **declared centrally** so names can't drift: put a
`concurrency.json` at the hooks root (next to the hook folders), and each
hook opts in by name. Referencing a group that isn't declared is a
load/validation error — the hook won't load.

```jsonc
// concurrency.json (at the hooks root)
{
  "$schema": "https://wow-look-at-my.github.io/webhook-runner/concurrency.schema.json",
  "groups": {
    // Serialize everything that hits the single local model server.
    "ollama-local": { "description": "shared local model server", "limit": 1 }
  }
}
```

```jsonc
// some-hook/hook.json
{
  "$schema": "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json",
  "concurrency_group": "ollama-local"
}
```

`limit` defaults to `1` (full serialization) and must be `>= 1`. Multiple
hooks may share a group — the limit applies across all of them, so two
different hooks that both call the same backend take turns. Omit
`concurrency_group` for unbounded concurrency. Watch live utilization on the
admin port's `/concurrency` endpoint, and a `run.queued` event appears in
the activity feed whenever a run has to wait. Queue time is also reported
separately from processing time everywhere run timing shows up — the
dashboard's Waited/Duration columns, `started`/`started_at` on `/runs`, and
the per-hook avg/max wait vs duration stats — so a run stuck behind a busy
group never reads as a slow run.

The schema is published at
`https://wow-look-at-my.github.io/webhook-runner/concurrency.schema.json`.

## Scheduled hooks

A hook can fire itself on a timer, not just on an HTTP `POST`. Add a
`schedule` (a Go duration) to its `hook.json`:

```json
{
  "$schema": "https://wow-look-at-my.github.io/webhook-runner/hook.schema.json",
  "description": "fleet reconcile sweep",
  "schedule": "5m",
  "timeout": "10m"
}
```

A scheduled run is dispatched through the **same pipeline** as an
HTTP-triggered one — it is tracked, shown on the dashboard and `/runs`, gated
by any `concurrency_group`, and gets the KV store when `state` is set. It
receives a synthetic payload in `$HOOK_PAYLOAD_FILE`:

```json
{ "trigger": "schedule", "hook": "<id>", "time": "<RFC3339>" }
```

and an `X-Webhook-Runner-Schedule` request header, so the code can tell a
timer fire from an HTTP caller.

- **Overlap protection.** The scheduler **skips** a tick whenever a previous
  run of the same hook is still in flight (pending or running), so a sweep
  that runs longer than its interval never stacks copies of itself. A skip is
  recorded as a `schedule.skipped` event; a fire as `schedule.fired`. For a
  hook that should *serialize* rather than skip, add a `concurrency_group`
  too.
- **Startup / missed ticks.** On startup — and whenever a schedule is newly
  added or its interval changes — the hook fires **immediately**, then every
  interval thereafter. An unrelated hooks reload preserves an unchanged
  schedule's next-fire time (no spurious re-fire). After a long pause
  (a restart or deploy), the hook fires once and resumes one interval out
  rather than bursting a backlog of missed ticks. Fire-immediately-on-start
  is deliberate: a redeploy is exactly when a catch-up sweep is wanted, and
  scheduled work is expected to be idempotent.

`schedule` is a Go duration string (e.g. `"30s"`, `"5m"`, `"1h"`); the
effective resolution is one second. Like `concurrency_group` and `state`, it
is a newer field, so an older `webhook-runner` binary rejects a `hook.json`
that sets it — deploy a runner that supports `schedule` before merging a hook
that uses it.

## Encrypted secrets in the hooks repo (sops)

A hook directory may contain `secrets.sops.env` — a
[sops](https://github.com/getsops/sops)-encrypted **dotenv** file. At
container start webhook-runner decrypts it (by shelling out to the `sops`
binary) and:

- **injects every entry** into the container environment (an explicit
  `env` entry in hook.json wins on conflict; reserved `HOOK_*` keys are
  skipped with a warning), and
- makes the entries resolvable by `${NAME}` references in `env` values
  and `api_key`.

Decryption failures are loud: a run fails with status `error` before the
container starts, and an `api_key` backed by an undecryptable file rejects
all requests. Results are cached per file (invalidated by mtime/size), so
steady-state requests don't re-exec sops; a `git pull` of the hooks repo
picks up rotated values automatically.

Setup with [age](https://github.com/FiloSottile/age) (any sops keysource
works — age, KMS, PGP, Vault):

```sh
# once, on the runner host
age-keygen -o /etc/webhook-runner/age.key        # note the public key
# export SOPS_AGE_KEY_FILE=/etc/webhook-runner/age.key in the service env

# in the hooks repo, per hook
cat > my-hook/secrets.sops.env <<EOF
MY_HOOK_API_KEY=super-secret
EOF
sops --encrypt --age <public-key> --input-type dotenv --output-type dotenv \
  --in-place my-hook/secrets.sops.env
```

`validate` ignores secrets files entirely, and the `sops` binary is only
required on the runner host (override its path with
`WEBHOOK_RUNNER_SOPS_BIN`).

## Hook tests

A hook can declare test commands for its scripts in `hook.json`:

```jsonc
{
  // image/command come from this hook's Dockerfile
  "tests": [["node", "--test", "handler.test.ts"]]
}
```

`webhook-runner test <hooks-dir>` runs every declared test command in a
fresh container of the hook's image. For Dockerfile hooks it builds the
image first (the same content-hash tag a live run uses), so tests exercise
exactly the baked code — copy test files into the image alongside the code
and set `WORKDIR` so relative paths like `handler.test.ts` resolve. Hooks
without a `tests` array are skipped; the command exits non-zero if any
hook fails to load, any build fails, or any test command fails.

Tests get no payload, no `hook.json` env, and no secrets: they must be
self-contained (start their own mock servers, set their own env). That is
what lets a hooks repo's CI run them with nothing but Docker — the test
commands live next to the code they test instead of being hardcoded into a
workflow. Each command is capped by `--timeout` (default 10m, independent
of the hook's run `timeout`); `--hook <id>` filters to specific hooks.

## Server configuration

| Variable                          | Default                      | Notes                                                        |
|-----------------------------------|------------------------------|--------------------------------------------------------------|
| `WEBHOOK_RUNNER_HOOKS_DIR`        | (none)                       | Hooks directory. Also accepted as positional arg.             |
| `WEBHOOK_RUNNER_HOOKS_REPO`       | (none)                       | Git URL to clone hooks from. SSH URLs recommended for private repos. |
| `WEBHOOK_RUNNER_HOOKS_BRANCH`     | (repo default)               | Branch to track when using `HOOKS_REPO`.                     |
| `WEBHOOK_RUNNER_HOOKS_REPO_SECRET`| (none)                       | HMAC-SHA256 secret for `POST /_reload` on the hook port.     |
| `WEBHOOK_RUNNER_HOOK_BASE_URL`    | (none)                       | Public base URL of the hook port (e.g. `https://hooks.example.com`). Shown in the dashboard setup instructions. |
| `WEBHOOK_RUNNER_ADDR`             | `:9000`                      | Hook port listen address.                                    |
| `WEBHOOK_RUNNER_ADMIN_ADDR`       | `:9001`                      | Admin port listen address.                                   |
| `WEBHOOK_RUNNER_DATA_DIR`         | (hooks-dir parent)           | Directory for KV state (`kv/<namespace>.json`), the token `state-secret`, and the run history (`runs.db`). Defaults alongside the hooks clone + deploy key. |
| `WEBHOOK_RUNNER_STATE_SOCKET`     | `$TMPDIR/whr-state.sock`     | Path of the KV API's internal Unix socket (the proxy shim bridges `localhost:9002` to it). Must stay in a host-shared dir (defaults under `TMPDIR`, which already is). |
| `WEBHOOK_RUNNER_STATE_SECRET`     | (generated + persisted)      | HMAC secret signing per-hook KV tokens. Set it to share one secret across replicas; otherwise it's generated and saved to `<data-dir>/state-secret`. |
| `WEBHOOK_RUNNER_KV_MAX_KEYS`      | `5000`                       | Max keys in one hook's KV namespace. Positive integer; unset or invalid falls back to the default. |
| `WEBHOOK_RUNNER_RUN_RETENTION`    | `48h`                        | How long completed runs are kept in the persistent run history (`<data-dir>/runs.db`). Go duration; the primary retention knob. |
| `WEBHOOK_RUNNER_RUN_RETENTION_MAX`| `200000`                     | Max persisted runs per hook — a coarse disk safety net behind the time-based retention (the GC sweep prunes oldest-first). |
| `WEBHOOK_RUNNER_GITHUB_TOKEN`     | (none)                       | Required only if any hook uses `github_status`.               |
| `WEBHOOK_RUNNER_SOPS_BIN`         | `sops`                       | sops binary used to decrypt `secrets.sops.env` files. Key material is plain sops config on the service env (e.g. `SOPS_AGE_KEY_FILE`). |
| `WEBHOOK_RUNNER_LOG_FORMAT`       | `text`                       | Or `json`.                                                   |
| `TMPDIR`                          | `/tmp`                       | Where per-run payload/header files AND the KV socket + proxy shim live before being bind-mounted into hook containers. Must be host-shared when the server itself runs in a container (below). |

### Running the server in a container

The server shells out to the **host's** docker daemon (mount
`/var/run/docker.sock`), and per-run payload/header files **plus the KV socket
and proxy shim** are bind-mounted into hook containers **by host path**. A temp
dir private to the server's container doesn't exist on the host, so docker
silently creates a *directory* at the mount source and every run fails reading
its payload (`EISDIR`); the KV socket/shim would likewise be unreachable. The
server detects this topology at startup and records a `server.misconfigured`
event on the dashboard unless `TMPDIR` is set.

Share the temp dir with the host at the **same absolute path**, and declare
it via `TMPDIR`:

```yaml
services:
  webhook-runner:
    image: ghcr.io/wow-look-at-my/webhook-runner:latest
    environment:
      - TMPDIR=/var/lib/webhook-runner/tmp
      # KV state lives here — keep it on a persistent volume.
      - WEBHOOK_RUNNER_DATA_DIR=/var/lib/webhook-runner
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /var/lib/webhook-runner/tmp:/var/lib/webhook-runner/tmp
      - webhook-runner-data:/var/lib/webhook-runner
```

State hooks need **no** extra config here: the KV socket is created under
`TMPDIR` (already host-shared above), so the hook containers the server
launches bind-mount it by the same host path and reach the KV API directly —
no network, no published port. This works identically whether the server runs
in a container or directly on the host.

(Sharing `/tmp:/tmp` also works — then declare `TMPDIR=/tmp` to silence the
startup warning.) The hooks dir needs no sharing: image builds stream their
context over the docker socket instead of resolving host paths.

## Inside the container

Every container started by webhook-runner has these environment variables
set automatically:

| Variable             | Contents                                                |
|----------------------|---------------------------------------------------------|
| `HOOK_PAYLOAD_FILE`  | Path to a file containing the raw request body.         |
| `HOOK_HEADERS_FILE`  | Path to a JSON file `{"X-Header": ["value"], ...}`.     |
| `HOOK_ID`            | The hook ID (folder name).                              |
| `HOOK_RUN_ID`        | The 128-bit run ID, base32 encoded (26 chars).          |
| `HOOK_KV_URL`        | Base URL of the KV API (`http://localhost:9002`) — use it with any HTTP client. **Only for `state: true` hooks.** |
| `HOOK_KV_TOKEN`      | Bearer token scoped to this hook's KV namespace. **Only for `state: true` hooks.** |
| `HOOK_KV_SOCKET`     | Internal: path the injected proxy shim bridges `HOOK_KV_URL` to. Hooks normally use `HOOK_KV_URL`. **Only for `state: true` hooks.** |

Both files are bind-mounted read-only under `/var/run/webhook-runner/` —
per-run *data*, never code. Hook code is immutable per run: it is baked
into the hook's built image (see below).

## Hook images (Dockerfile)

Every hook directory contains a `Dockerfile` next to its `hook.json` —
there is no other way to supply code. The hook runs an image
webhook-runner builds locally from the hook directory (the build
context), tagged `whr-hook/<id>:<content-hash>`:

```
my-hook/
  hook.json       # "command" optional (the image's CMD runs by default)
  Dockerfile      # FROM node:24-alpine / WORKDIR /app / COPY handler.ts . / CMD ["node", "handler.ts"]
  handler.ts
```

The content-hash tag is what makes runs immutable and rebuilds automatic:

- The image is built lazily on the hook's next run (or `test`) whenever no
  image exists for the directory's current content — after a hooks-repo
  pull, the first run rebuilds; an unchanged hook reuses the cached image.
- In-flight runs keep the image they started with; a concurrent
  `POST /_reload` + `git pull` cannot change what they execute.
- Superseded builds are deleted after a successful new build (best-effort;
  images backing still-running containers are skipped).
- A failed build fails the run with status `error` before any container
  starts; build output is streamed to the server log.

No registry is involved: the hooks repo stays the single source of truth,
and the runner host turns it into immutable local images.

## Subcommands

- `webhook-runner [hooks-dir]` — start the server.
- `webhook-runner validate <hooks-dir>` — load and validate every hook
  without starting the server. Exits non-zero on validation errors.
- `webhook-runner test <hooks-dir>` — run every hook's declared `tests`
  commands in its image (see "Hook tests"). Requires Docker. Exits
  non-zero on load errors or test failures.
- `webhook-runner version` — print build version.

## Building

This project uses [`go-toolchain`](https://github.com/wow-look-at-my/go-toolchain);
running it from the repo root handles `go mod tidy`, tests, coverage,
and a binary build.

```sh
go-toolchain
```

## Notes

- The runtime image is plain `alpine` plus `docker-cli`. The Docker
  SDK is intentionally not used — webhook-runner shells out to `docker run`
  exactly as you would on the command line.
- The image defines a `HEALTHCHECK` that probes `GET /health` on the hook
  port (derived from `WEBHOOK_RUNNER_ADDR`), so `docker ps` reports health
  and deploy tooling like docker-updater can gate updates on it.
- No CGO. The binary is `go build -o webhook-runner ./cmd/webhook-runner`
  with `CGO_ENABLED=0`.
- Run history persists: completed runs (metadata + captured output) are
  written to a single bbolt file (`<data-dir>/runs.db`) the moment they
  finish and survive restarts — `WEBHOOK_RUNNER_RUN_RETENTION` (default
  48h) is the primary retention knob, with a per-hook count cap
  (`WEBHOOK_RUNNER_RUN_RETENTION_MAX`, default 200000) as a disk safety
  net. The live tracker stays in memory and bounded (50 most recent per
  hook, last 2000 lines of output per run); a run still in flight during
  a restart never completed and is not in the store.
