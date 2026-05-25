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
- **Concurrency**: no global queue, each request fires its own container.
- **Dashboard**: read-only HTML view at `/` on the admin port.
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
| POST   | `/_reload`          | Pull hooks repo and reload (HMAC auth, requires `WEBHOOK_RUNNER_HOOKS_REPO_SECRET`). |

### Admin port (`:9001`)

| Method | Path                | Purpose                                    |
|--------|---------------------|--------------------------------------------|
| GET    | `/health`           | Liveness probe (200).                      |
| GET    | `/hooks`            | List loaded hooks (id + description).      |
| POST   | `/hook/{id}`        | Trigger a hook (also available here).      |
| GET    | `/runs`             | Recent runs across all hooks.              |
| GET    | `/runs/{id}`        | Status + retained output for one run.      |
| POST   | `/reload`           | Pull hooks repo and reload (no auth — admin port is trusted). |
| GET    | `/`                 | Dashboard.                                 |

### Sync vs async

By default, `POST /hook/{id}` returns `202 Accepted` with `{"run_id": "..."}`
as soon as the container has been spawned. To block until the run finishes,
add `?wait=true`. Combine with `?timeout=30s` to cap how long the server
holds the connection (the run continues in the background if the sync
timeout elapses first).

A hook can opt into sync-by-default by setting `"synchronous": true`
in `hook.json`. The query parameter still wins.

## hook.json reference

The full schema is published at
`https://wow-look-at-my.github.io/webhook-runner/hook.schema.json`.
See `examples/hooks/` for working examples.

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
| `WEBHOOK_RUNNER_LOG_FORMAT`       | `text`                       | Or `json`.                                                   |

## Inside the container

Every container started by webhook-runner has these environment variables
set automatically:

| Variable             | Contents                                                |
|----------------------|---------------------------------------------------------|
| `HOOK_PAYLOAD_FILE`  | Path to a file containing the raw request body.         |
| `HOOK_HEADERS_FILE`  | Path to a JSON file `{"X-Header": ["value"], ...}`.     |
| `HOOK_ID`            | The hook ID (folder name).                              |
| `HOOK_RUN_ID`        | The 128-bit run ID, base32 encoded (26 chars).          |

Both files are bind-mounted read-only at `/var/run/webhook-runner/`.

Additionally, any `env` values in `hook.json` that use `$VAR` or `${VAR}`
syntax are expanded from the server's environment at load time. The same
expansion applies to `api_key`, `secret`, and `public_key` fields, keeping
secrets out of the hooks repository:

```jsonc
{
  "env": { "GITHUB_TOKEN": "$WEBHOOK_RUNNER_GITHUB_TOKEN" },
  "secret": "$MY_WEBHOOK_SECRET"
}
```

## Subcommands

- `webhook-runner [hooks-dir]` — start the server.
- `webhook-runner validate <hooks-dir>` — load and validate every hook
  without starting the server. Exits non-zero on validation errors.
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
- No CGO. The binary is `go build -o webhook-runner ./cmd/webhook-runner`
  with `CGO_ENABLED=0`.
- No persistence: run history is in memory only and bounded
  per-hook (50 most recent) and per-run (last 500 lines of output).
