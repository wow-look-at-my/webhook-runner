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
- **GitHub commit status integration**: optional per-hook; posts
  `pending` on start and `success`/`failure`/`error` on exit.
- **Sync and async invocation**: every hook returns a unique 128-bit
  run ID; clients can poll `GET /runs/{id}` or use `?wait=true` to block
  on the response.
- **Concurrency**: no global queue, each request fires its own container.
- **Dashboard**: read-only HTML view at `/`.
- **Static binary, alpine runtime image** with `docker-cli` for shelling
  out — no Docker SDK dependency.

## Quick start

```sh
# Build
go-toolchain
./webhook-runner ./examples/hooks

# Or with the bundled image
docker build -t webhook-runner .
docker run --rm \
  -p 9000:9000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v $PWD/examples/hooks:/hooks:ro \
  webhook-runner /hooks
```

Then trigger a hook:

```sh
curl -X POST -d '{"foo":"bar"}' http://localhost:9000/hook/deploy-frontend
# { "run_id": "abqkr2f6mfjrtgsihpnz5rdgye" }

curl http://localhost:9000/runs/abqkr2f6mfjrtgsihpnz5rdgye
```

## HTTP API

| Method | Path                | Purpose                                    |
|--------|---------------------|--------------------------------------------|
| GET    | `/health`           | Liveness probe (200).                      |
| GET    | `/hooks`            | List loaded hooks (id + description).      |
| POST   | `/hook/{id}`        | Trigger a hook. Body becomes `HOOK_PAYLOAD_FILE`. |
| GET    | `/runs`             | Recent runs across all hooks.              |
| GET    | `/runs/{id}`        | Status + retained output for one run.      |
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

A JSON schema is published at
`https://wow-look-at-my.github.io/webhook-runner/hook.schema.json`.

```jsonc
{
  "description": "Deploy frontend on push to main",

  // REQUIRED.
  "image": "alpine:3.20",
  "command": ["sh", "-c", "echo deploying $HOOK_PAYLOAD_FILE"],

  // --- Authentication (pick at most one) ---

  // API key (recommended). Checked against the X-API-Key header.
  "api_key": "your-secret-key",
  "api_key_header": "X-API-Key",        // default

  // Ed25519 public key. Base64 or hex encoded. Sender signs the
  // body with the corresponding private key.
  "public_key": "<base64-ed25519-pubkey>",
  "signature_header": "X-Signature-Ed25519",  // default

  // Legacy HMAC-SHA256. Prefer api_key or public_key for new hooks.
  "secret": "whsec_abc123",

  // --- Optional fields ---

  "networks": ["frontend"],
  "volumes": ["/var/certs:/certs:ro"],
  "env": { "DEPLOY_TARGET": "production" },
  "user": "1000:1000",
  "workdir": "/app",
  "timeout": "10m",
  "extra_docker_args": ["--cap-add=NET_ADMIN"],
  "synchronous": false,

  "github_status": {
    "enabled": true,
    "context": "webhook-runner/deploy-frontend",
    "target_url": "https://hooks.example.com/runs/{{.RunID}}"
  }
}
```

## Server configuration

| Variable                      | Default   | Notes                                           |
|-------------------------------|-----------|-------------------------------------------------|
| `WEBHOOK_RUNNER_HOOKS_DIR`    | (none)    | Hooks directory. Also accepted as positional arg. |
| `WEBHOOK_RUNNER_ADDR`         | `:9000`   | Listen address.                                 |
| `WEBHOOK_RUNNER_GITHUB_TOKEN` | (none)    | Required only if any hook uses `github_status`. |
| `WEBHOOK_RUNNER_LOG_FORMAT`   | `text`    | Or `json`.                                      |

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
