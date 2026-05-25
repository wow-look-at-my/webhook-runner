# webhook-runner — notes for Claude

## What this is

A self-contained Go HTTP server that executes incoming webhooks inside
disposable Docker containers. Each hook is described by a `hook.json` file
in its own folder under a single hooks directory. Hook definitions can
come from a local directory or be cloned from a Git repository.

## Project layout

```
cmd/webhook-runner/        binary entry point (calls into internal/cli)
internal/cli/              cobra commands (root = run server, validate, version)
internal/server/           HTTP handlers + routing (two muxes: hook + admin)
internal/server/dashboard/ embedded read-only HTML dashboard
internal/hooks/            hook.json model, loader, registry, watcher, git repo
internal/runner/           docker run dispatch + output streaming
internal/runs/             in-memory run tracker (bounded)
internal/githubstatus/     GitHub commit status API client
schema/                    JSON schema for hook.json (published to GitHub Pages)
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

## Architecture: dual ports

The server listens on two ports:

- **Hook port** (`:9000`): `POST /hook/{id}`, `GET /health`, `POST /_reload`.
  Public-facing, exposed via Cloudflare Tunnel.
- **Admin port** (`:9001`): dashboard, `/hooks`, `/runs`, `/reload`.
  Internal, behind Cloudflare Zero Trust.

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

## Env var expansion

Hook `env` values and auth fields (`api_key`, `secret`, `public_key`)
support `$VAR` / `${VAR}` syntax. These are expanded from the server's
environment at hook load time via `Hook.Resolve()` / `ResolveAll()`.
Expansion is NOT done during validation (`webhook-runner validate`), so
`$`-prefixed placeholders survive CI. Validation also skips format
checks on `$`-prefixed `public_key` values.

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
