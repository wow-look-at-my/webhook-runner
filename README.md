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
  timestamped log on the clipboard.
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

The server listens on two ports. The **hook port** (default `:9000`) should
be publicly accessible (e.g. via a Cloudflare Tunnel). The **admin port**
(default `:9001`) should be behind authentication (e.g. Cloudflare Zero
Trust).

### Hook port (`:9000`)

| Method | Path                | Purpose                                    |
|--------|---------------------|--------------------------------------------|
| GET    | `/health`           | Liveness probe (200).                      |
| POST   | `/hook/{id}`        | Trigger a hook. Body becomes `HOOK_PAYLOAD_FILE`. |
| POST   | `/hook/{id}/cancel/{run}` | Cancel an in-flight run of this hook (same auth as triggering it). |
| POST   | `/_reload`          | Pull hooks repo and reload (HMAC auth, requires `WEBHOOK_RUNNER_HOOKS_REPO_SECRET`). |

### Admin port (`:9001`)

| Method | Path                | Purpose                                    |
|--------|---------------------|--------------------------------------------|
| GET    | `/health`           | Liveness probe (200).                      |
| GET    | `/hooks`            | List loaded hooks (id + description).      |
| POST   | `/hook/{id}`        | Trigger a hook (also available here).      |
| POST   | `/hook/{id}/cancel/{run}` | Cancel a run (also available here).  |
| GET    | `/runs`             | Recent runs across all hooks.              |
| GET    | `/runs/{id}`        | Status + retained output for one run.      |
| POST   | `/runs/{id}/cancel` | Cancel any run (no auth — admin port is trusted). |
| POST   | `/reload`           | Pull hooks repo and reload (no auth — admin port is trusted). |
| GET    | `/events`           | Activity feed: GitHub push webhooks, git pulls, hook (re)loads and load errors, image builds, run lifecycle (including `run.queued` when a run waits for a concurrency slot), rejected requests (`hook.unknown`, `hook.denied`, `hook.misconfigured`) and unresolved env references (`env.unresolved`). Newest first; `?max=` caps it. |
| GET    | `/images`           | Per-hook image state: the tag the current content resolves to, whether it's built (false = next run builds it), and every `whr-hook/*` image on disk. |
| GET    | `/concurrency`      | Live state of every declared concurrency group: its `limit`, how many runs are `active`, and how many are `waiting` (queued) behind it. |
| GET    | `/`                 | Dashboard.                                 |

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

## Concurrency groups

By default a hook runs with unbounded concurrency: every accepted request
spawns its container immediately. When a burst arrives — or several hooks
share one scarce backend (a single local model server, a rate-limited API)
— that means many containers competing at once, and, worse, **every run's
timeout starts counting the moment it is accepted**, so runs that are really
just *waiting* can time out before they ever do work.

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
the activity feed whenever a run has to wait.

The schema is published at
`https://wow-look-at-my.github.io/webhook-runner/concurrency.schema.json`.

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
| `WEBHOOK_RUNNER_GITHUB_TOKEN`     | (none)                       | Required only if any hook uses `github_status`.               |
| `WEBHOOK_RUNNER_SOPS_BIN`         | `sops`                       | sops binary used to decrypt `secrets.sops.env` files. Key material is plain sops config on the service env (e.g. `SOPS_AGE_KEY_FILE`). |
| `WEBHOOK_RUNNER_LOG_FORMAT`       | `text`                       | Or `json`.                                                   |
| `TMPDIR`                          | `/tmp`                       | Where per-run payload/header files are written before being bind-mounted into hook containers. Must be host-shared when the server itself runs in a container (below). |

### Running the server in a container

The server shells out to the **host's** docker daemon (mount
`/var/run/docker.sock`), and per-run payload/header files are bind-mounted
into hook containers **by host path**. A temp dir private to the server's
container doesn't exist on the host, so docker silently creates a
*directory* at the mount source and every run fails reading its payload
(`EISDIR`). The server detects this topology at startup and records a
`server.misconfigured` event on the dashboard unless `TMPDIR` is set.

Share the temp dir with the host at the **same absolute path**, and declare
it via `TMPDIR`:

```yaml
services:
  webhook-runner:
    image: ghcr.io/wow-look-at-my/webhook-runner:latest
    environment:
      - TMPDIR=/var/lib/webhook-runner/tmp
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /var/lib/webhook-runner/tmp:/var/lib/webhook-runner/tmp
```

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
- No persistence: run history is in memory only and bounded
  per-hook (50 most recent) and per-run (last 500 lines of output).
