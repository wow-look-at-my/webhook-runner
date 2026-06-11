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
internal/runner/           docker run dispatch + output streaming + image build/status
internal/runs/             in-memory run tracker (bounded)
internal/events/           in-memory activity feed (bounded ring; nil-recorder safe)
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
- **`$schema` is required.** Every `hook.json` must declare a `$schema`
  field (the `Hook.Schema` field); `Hook.validate` rejects a hook without
  one. The matching property lives in `schema/hook.schema.json`, which is
  published to GitHub Pages and is what the `$schema` URL points at. Keep
  the Go model, the JSON schema, and the example/e2e fixtures in sync.

## Architecture: dual ports

The server listens on two ports:

- **Hook port** (`:9000`): `POST /hook/{id}`, `POST /hook/{id}/cancel/{run}`,
  `GET /health`, `POST /_reload`. Public-facing, exposed via Cloudflare Tunnel.
- **Admin port** (`:9001`): dashboard, `/hooks`, `/runs`,
  `/runs/{id}/cancel`, `/reload`, `/events` (activity feed), `/images`
  (per-hook image state). Internal, behind Cloudflare Zero Trust.
  The dashboard's one-time webhook-setup instructions live in a
  collapsed `<details>`; the page is about live state (hooks, images,
  runs, activity). The `events.Recorder` is a nil-safe bounded ring fed
  by the server (push webhooks, reloads, load errors) and the runner
  (image builds, run lifecycle) — memory only, like run history.

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
