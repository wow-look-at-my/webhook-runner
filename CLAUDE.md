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
internal/server/dashboard/ embedded HTML dashboard (read views + the operator kill-switch controls); ts/ holds the dashboard's module TypeScript that ts0 compiles into the committed assets/timeline.js — the page's ONLY ES module, so a new module script goes in ts/, never hand-written into index.html. Regenerate with the prebuilt ts0 (`curl -fSL 'https://dl.pazer.build/ts0?v=10&os=linux&arch=amd64' -o /tmp/ts0.cjs` then `cd internal/server/dashboard && node /tmp/ts0.cjs build` — stock Node, no npm); ci.yml's `dashboard-assets` job runs the same build and FAILS if the committed bundle drifted from ts/ — THREE js-snippets components are NOT in this repo and are imported by the browser at runtime from js-snippets' buildhost library site: <timeline-view> (the runs chart; types via the interim shim ts/js-snippets-timeline.d.ts), <activity-feed> (BOTH Activity feeds — the overview page and the per-hook section — owning their table, kind badges and filter bar) and <data-table> (EVERY table on the page — all eleven: runs ×2, hooks, managers, images, attention, kv ×2, concurrency ×2, reload commits — owning rows, sorting, chips, empty states and expandable detail. There are ZERO hand-rolled `<table>`s left, and adding one is a regression: the component is where table behavior lives. The per-hook runs status facet is declared `local: false` because that filter is applied SERVER-SIDE before the row cap, so the component must never re-apply it; the concurrency tables and the per-hook KV browser use `detailFor` for their drill-downs, KV's asynchronously since a key's value is fetched on expand). All three are loaded by ts/timeline.ts and fed by dashboard.js, which passes cell CSS into the shadow root via the element's styleText. Fix component bugs upstream in js-snippets, never here; testjs/ is the node-run client harness proving the push-first section feed (authored in TypeScript, run DIRECTLY via `node --test`'s native type-stripping — no build step; CI pins Node with actions/setup-node). the convention is to author/commit `.ts` source, not generated `.mjs` (gitignored via `*.mjs`; a genuine edge-case `.mjs` can be `git add -f`'d). The one deliberately-committed generated artifact is the dashboard adapter's `assets/timeline.js` bundle — a `.js` (not caught by the `*.mjs` rule), embedded via go:embed and regenerated via ts0
internal/hooks/            hook.json + manager.json models, loader, registry, watcher, git repo
internal/managers/         the manager entity's runtime: bounded inbox (checkout/settle handles) + supervisor (flock lease, flat restarts, output ring, attention seam)
internal/reloadgate/       hooks-repo reload CI gate: /_reload event handling (push records, status switches), last-good persistence, admin-force bypass
internal/concurrency/      named concurrency groups (central concurrency.json) + semaphore manager (+ operator limit overrides)
internal/overrides/        operator kill switch: disabled hooks + concurrency limit overrides, persisted to <data-dir>/overrides.json
internal/scheduler/        per-hook "schedule" interval timer (pure timing; Fire callback dispatches the run)
internal/jsonc/            shared JSONC comment-stripping (hook.json + concurrency.json)
internal/runner/           docker run dispatch + output streaming + image build/status
internal/runs/             in-memory run tracker (bounded) + the OnFinish persistence seam
internal/runstore/         bbolt-backed persistent completed-run history (48h retention, GC sweeper)
internal/events/           in-memory activity feed (bounded ring; nil-recorder safe)
internal/attention/        aggregated ACTIVE misconfigurations (the needs-attention surface: GET /attention + the dashboard's red banner; nil-aggregator safe)
internal/spool/            durable park for deliveries arriving during shutdown drain (replayed by the next process)
internal/kv/               disk-backed per-hook KV store (state socket) + HMAC namespace tokens
internal/backlog/          durable per-hook batch backlogs (drain-a-slice; behind /backlog)
internal/kvproxy/          TCP->Unix proxy shim injected into state hooks (plain localhost URL)
internal/githubstatus/     GitHub commit status API client
schema/                    JSON schemas for hook.json + manager.json + concurrency.json (published to buildhost sites — .github/workflows/schemas.yml)
e2e/                       end-to-end test (shell script, requires Docker)
dats/                      black-box CLI-contract tests (.dats YAML, org dats runner — see "CLI contract tests" below)
examples/hooks/            sample hook configs
docs/                      the depth CLAUDE.md points at (internals/, design docs)
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
  one — presence only, never a specific URL. The matching property lives in
  `schema/hook.schema.json`, published to buildhost sites on every master
  push (`.github/workflows/schemas.yml` — replaced the GitHub Pages deploy,
  which died on the org's Actions artifact-storage quota 2026-07-17;
  operator directive 2026-07-19: use buildhost). Canonical URL:
  `https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json`
  (a public site branch under the private repo's private buildhost project,
  via the publish action's `public: true`). The legacy
  `https://wow-look-at-my.github.io/webhook-runner/` URLs keep serving
  their frozen 2026-07-15 content and stay valid in deployed hook.jsons.
  Keep the Go model, the JSON schema, and the example/e2e fixtures in sync.

## CLI contract tests (dats/)

`dats/*.dats` are black-box tests of the CLI's contract — exit codes,
stdout/stderr, messages — run by the org's
[dats](https://github.com/wow-look-at-my/dats) test runner against the REAL
built binary (unlike `internal/cli/commands_test.go`, which drives cobra
in-process). They are deliberately docker-free, offline, and secret-free so
they pass on a bare runner: `validate`'s full gate contract (plus a drift
gate that `validate examples/hooks` stays green), `test`'s docker-free
paths, and version/help/argument/flag errors — the authoritative case list
is the `desc:` lines in `dats/*.dats`. `serve`, real `test` runs, and the
dashboard need Docker/network and stay in `e2e/`.

**The suites declare `sandbox: false`, and must keep doing so.** dats
SANDBOXES commands BY DEFAULT (bubblewrap, falling back to docker), and its
bwrap sandbox gives a command a FRESH /tmp — while go-toolchain's dats phase
stages the binaries the suites exec under an `os.MkdirTemp` there. Inside the
sandbox that path does not exist, so every test exits 127 (all 23, the moment
dats v49 turned sandboxing on). Nothing here needs isolating: these are
docker-free, offline, secret-free tests of our own freshly built CLI. The
opt-out also means the suites need NO sandbox backend at all — dats probes
lazily — so the dind pinning below is now belt-and-braces rather than load-bearing.

**dats itself is not runner-free** (the ruling that put these jobs on dind):
without the opt-out it fails a run outright when neither backend is usable. The slim
`wow-linux` fleet can supply neither — docker is deleted from that image by
design, and bubblewrap needs an unprivileged user namespace a stock container
is refused — so **both jobs that run dats (`dats`, and `test` via
go-toolchain's dats phase) use `vars.CI_RUNNER_DIND`**, where bubblewrap is
installed and measured working. Operator ruling 2026-07-26; the measurements,
and the alternative that was rejected (granting the slim fleet
`seccomp=unconfined` + `CAP_SYS_ADMIN`), are in the webhooks repo's
`src/hooks/gha-runner/CLAUDE.md`. Moving either job back to `CI_RUNNER` fails
it at the dats phase, not in the suite.

Every suite command execs the binary as
`"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner"` — NEVER a bare
PATH lookup. go-toolchain itself runs these suites as its **dats phase**
after every build (go-toolchain#330): it stages throwaway binary copies
under `$GO_TOOLCHAIN_DATS_BUILD_DIR` and does NOT put them on PATH, so a
bare `webhook-runner` in a `cmd` exits 127 there (the 2026-07-21 CI
breakage). The `:-build` fallback keeps standalone runs working from the
repo root. Note dats runs each `cmd` with `bash -c`, so the expansion
needs no `sh -c` wrapper.

Run locally from the repo root (a plain `go-toolchain` already runs the
suites via its dats phase; to run them standalone, build first so
`build/webhook-runner` exists and install dats per README's "CLI contract
tests (dats)" section):

    go-toolchain
    dats test dats

ci.yml's `dats` job runs the standalone invocation against the `test`
job's `go-build` hand-off — local and CI are identical by design.

Facts to keep in mind when adding cases (dats' own docs are authoritative
for the general format — `docs/file-format.md` in the dats repo; the
parser is strict and `dats syntax dats` checks without running):

- Tests are SANDBOXED but not chdir'd: `inputs.files` (map of relative
  path -> content) materialize under a per-test temp dir, the command runs
  with cwd = the invocation cwd, and `{inputs.<path>}` in `cmd` expands to
  a fixture's ABSOLUTE path. There is no directory placeholder, so a
  fixture tree's root is recovered as
  `"$(dirname "{inputs.<hook>/hook.json}")/.."` — the suite's standard
  anchor pattern. Every case is self-contained; never share fixtures on
  disk.
- Stream assertions: LIST entries are substring-contains; MAP entries are
  0-based line numbers matched as REGEXES (escape `(`/`[`; the two forms
  really do differ — an unescaped `(s)` in a map entry silently changes
  meaning).
- dats runs ALL tests, by design — no filtering/skip/only mechanisms
  exist, and none should be added or emulated.
- NEVER write a case that invokes bare `webhook-runner <word>`: the root
  command's `[hooks-dir]` positional means any unrecognized word STARTS
  THE SERVER (binds :9000/:9001) instead of erroring. Tests near the
  serve path must error before binding (and carry a `timeout:` hang
  guard, e.g. `30s`).
- Assert only observed behavior — run the built binary by hand first and
  copy the exact exit code/message — and pin the minimal DISCRIMINATING
  substring, not remediation prose or valid-value rosters (those churn
  on compatible changes).

## Architecture: two ports + a state socket

The server listens on two TCP ports plus a Unix socket:

- **Hook port** (`:9000`): `POST /hook/{id}`, `POST /hook/{id}/cancel/{run}`,
  `GET /health` (body carries the build version), `GET /version` (build
  identity: version + VCS revision/time — the same string the `version`
  command prints, plumbed from cli via `server.Options.Version` — plus
  `hooks_tree`, the reload gate's served-tree state: `state` is
  `serving` (`serving_sha` + `verified`), `held` (adds `pending_sha`,
  `pending_state`, rendered `reason`), `unknown` (gate tracking, no
  serving commit recorded — `serving_sha` omitted, never an ambiguous
  empty string), or `untracked` (`mode` names the gate-off/no-repo
  mode). Wired via the nil-safe `Options.TreeState` (serve sets it to
  `reloadgate.Gate.TreeState`, a pure under-mutex snapshot — no git, no
  GitHub calls); exposing the private hooks repo's deployed commit sha
  on this PUBLIC port is a deliberate, operator-requested trade),
  `POST /_reload`. Public-facing, exposed via Cloudflare Tunnel.
- **Admin port** (`:9001`): the dashboard plus the operator API — hook/run/manager reads and drill-downs, the activity feed, `/attention`, `/concurrency`, the KV views, the reload panel, and the operator kill switches (disable a hook, override a concurrency limit, disable/restart a manager). `/runs/stream` is the SSE live tail that also multiplexes section-invalidation signals, so the dashboard never polls while it is up — with ONE deliberate exception: an open run modal on a non-terminal run polls `/runs/{id}` every 3s, because deltas are output-stripped and a merely-logging run emits none. Internal, behind Cloudflare Zero Trust.
  - **A manager instance is NOT a run**: its logs live under `/managers/{id}`, never in `/runs`.
  - `/kv/{namespace}/{key}` deliberately EXPOSES stored values (operator request; the admin port is operator-only). The hook port and `/hooks/{id}` stay value-free.
  - [docs/internals/admin-api.md](docs/internals/admin-api.md) -- every endpoint, the per-hook app page, and the realtime swimlane timeline.
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
  lifecycle, `env.unresolved` when an env reference expands to nothing)
  — memory only (run history, by contrast, persists completed runs via
  `internal/runstore`; see below). Rejections are events on purpose:
  the dashboard must be able to answer "did you receive anything?" —
  which is also why both feeds pass `?exclude=run` and show ONLY what has
  no run (the runs table owns run lifecycle). Every listing filters
  BEFORE the cap.

The `Server` struct has `HookHandler()` and `AdminHandler()` returning
separate `http.Handler`s. Tests use the `hook(s)` and `admin(s)` helpers.

## Hooks repo integration

When `WEBHOOK_RUNNER_HOOKS_REPO` is set, the server clones the repo on
startup (shallow, single-branch) into `WEBHOOK_RUNNER_HOOKS_DIR` (default
`/var/lib/webhook-runner/hooks`). `POST /_reload` on the hook port
accepts the repo's GitHub webhook — push AND status events (HMAC-SHA256
via `WEBHOOK_RUNNER_HOOKS_REPO_SECRET`). Reloads are CI-GATED by default
(`internal/reloadgate`; see the gate bullet under "Things easy to get
wrong"): a push only fetches + records the new tip as pending, and the
tree switches when a `status` event reports the gating context
(`all-builds`, override via `WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT`; set it
EMPTY to disable the gate and restore the legacy
any-signed-POST-pulls-and-reloads flow). An hourly reconciliation poll
(`WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL`, default `1h`, `0` disables)
backstops missed status webhooks: it fetches the tip and, when it
differs from what is serving, reads its gating status from the GitHub
API (via `WEBHOOK_RUNNER_GITHUB_TOKEN`) — switching only on green, so a
missed webhook costs at most ~one interval of latency instead of
freezing deploys. The admin port's `POST /reload`
is the operator's deliberate gate bypass: fetch + reset to the remote tip,
recorded verified, no auth.

For private repos, use an SSH URL (`git@github.com:...`). On first
startup, the server auto-generates an Ed25519 deploy key and logs the
public key. Add it to the repo's deploy keys on GitHub, then restart.
The key persists at `<hooks-dir>/../id_ed25519`.

The companion repo is `wow-look-at-my/webhooks`.

## Things easy to get wrong

The long-form gotchas moved to `docs/internals/` -- unchanged, each
authoritative for its area. What stays here is the short list that bites
most often, plus where to read the rest.

- `runner.execute` deliberately uses `exec.Command` (not `CommandContext`) and kills the container by name on timeout: if Go SIGKILLs the docker CLI, the container can survive. Async runs use `context.Background()`, NOT the request context (the client disconnects right after the 202).
- Run IDs are 16 random bytes, base32-lowercased to 26 chars — anything building container names from them must keep the `a-z2-7` alphabet in mind.
- **A lock TTL is ENFORCED, never assumed.** An expired lock is still the holder's: the contender's acquire kills that run, waits for it to be certainly dead, and takes the lock the finish seam freed — or is refused. Expiry alone frees nothing, in the store or the sweeper.
- **A backlog belongs in the runner, never in a hook-side cursor.** A hook run is one container, so "work I did not get to" has to outlive it. Two primitives, two questions: `internal/queue` decides WHEN to start a run; `internal/backlog` holds WHAT IS LEFT for a run that already exists (push is a set union, take removes a slice, depth is observable).
- **Fail closed, everywhere.** An undeclared concurrency group, a non-compiling `skip_if` regex, a malformed `run_title`, a mixed hook layout, zero hooks loaded — each is a load/validation error that DROPS the hook (or fails the run) rather than running it unbounded.
- **New hook.json fields are deploy-first.** `Parse` uses `DisallowUnknownFields`, so an older binary REJECTS a hook using a newer field. Deploy webhook-runner before merging hooks that rely on one.
- Hooks, concurrency groups, schedules and managers reload together through ONE closure (`buildLoadAndApply`). Never add a second reload path.
- **Filter BEFORE the cap, in every listing.** `max`/limit bounds what is RETURNED, never what is EXAMINED (`/runs?exclude=`, `/events?exclude=`+`?hook=`). Page-then-filter blanks a surface on exactly the busy hooks it exists for: a burst of excluded entries fills the page, the filter empties it, and the panel reports "nothing here" while the matches sit just behind them.
- **An image tag is a content hash, so anything derived from it is cacheable.** `imageCommand`'s `docker inspect` is memoized per (tag, command) — it used to be a full CLI + daemon round trip on every state-hook run, between slot acquisition and container launch. Never cache an inspect FAILURE: that is a daemon condition, not a property of the tag.
- **A phase mark that is missing means UNKNOWN, never zero.** Container overhead is measured, not estimated (`internal/runs` phase marks) — but only a hook whose container reports from the inside yields an EXACT boot figure; every other hook gets an upper bound that also contains its runtime's cold start. Never let the two meet in one number.
- **GitHub does not re-send a failed delivery.** A draining server therefore PARKS deliveries (`internal/spool`) and answers 202 — never 503 "the sender will retry". Shutdown order is load-bearing: the hook port and state socket stay up across `rn.Wait()`.

Read before changing any of these areas:

- [docs/internals/hooks-images-and-reload.md](docs/internals/hooks-images-and-reload.md) -- the CI-gated reload, cancellation, secrets/env refs, the two tree layouts, image immutability, the containerized-TMPDIR hazard, hook tests, `dind`, `script`.
- [docs/internals/runs-concurrency-and-overrides.md](docs/internals/runs-concurrency-and-overrides.md) -- the activity-based timeout, `skip_if`, `run_title`, concurrency groups, the global run cap, the operator kill switch, the scheduler, the run store.
- [docs/internals/streaming-and-attention.md](docs/internals/streaming-and-attention.md) -- the SSE hub's never-block invariant, the five section-signal seams, the needs-attention surface.
- [docs/internals/delivery-durability.md](docs/internals/delivery-durability.md) -- deploy windows: `/restart-ready`, the delivery spool and its replay, the shutdown ordering, and the port-down gap none of it covers.
- [docs/internals/kv-and-locks.md](docs/internals/kv-and-locks.md) -- the KV store, run-owned locks, try/block/steal, pinning.
- [docs/internals/backlogs.md](docs/internals/backlogs.md) -- the batch-backlog primitive: push-as-set-union, take-removes, depths, how it differs from internal/queue, and why a hook must never build a cursor instead.
- [docs/internals/managers-and-gateway.md](docs/internals/managers-and-gateway.md) -- managers (an instance is NOT a run), the push-fed admin surface, and unconditional github-state-mirror routing.
- [docs/internals/waits-and-spawn.md](docs/internals/waits-and-spawn.md) -- declared waits and the manifest-authorized spawn primitive.
- [docs/internals/run-phases.md](docs/internals/run-phases.md) -- lifecycle phase marks: what each one means, the exact-vs-bounded boot rule, the shim's in-container report.
- [docs/internals/shim-and-timeline.md](docs/internals/shim-and-timeline.md) -- the state-socket proxy shim and the dashboard timeline adapter.
- [docs/manager-entity-design.md](docs/manager-entity-design.md) -- the manager entity design, as built.
