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
internal/server/dashboard/ embedded HTML dashboard (read views + the operator kill-switch controls); ts/ holds the dashboard's module TypeScript that dashboard.go's `//go:generate sh generate-timeline.sh` compiles via ts0 into the committed assets/timeline.js — the page's ONLY ES module, so a new module script goes in ts/, never hand-written into index.html. The generate (same directory, cwd = the package dir) curls a PINNED ts0 build from buildhost + the component's published `.d.ts` pair from js-snippets' Pages into the committed `ts/js-snippets/`, then runs `node ts0.cjs build` — stock Node 22+, no npm/npx, no git auth; run it as `go-toolchain --generate <hash>` (bare `go-toolchain` prints the hash; ci.yml's `generate:` input carries the same one). ci.yml's freshness gate (`git diff --exit-code -- internal/server/dashboard/assets/ internal/server/dashboard/ts/js-snippets/`) FAILS on any drift, so the bundle and the fetched declarations can't go stale and an upstream component API change turns CI red instead of drifting — THREE js-snippets components are NOT in this repo and are imported by the browser at runtime from js-snippets' buildhost library site: <timeline-view> (the runs chart; types via the interim shim ts/js-snippets-timeline.d.ts, to be superseded by the generate-fetched real `.d.ts` pair), <activity-feed> (BOTH Activity feeds — the overview page and the per-hook section — owning their table, kind badges and filter bar) and <data-table> (EVERY table on the page — all eleven: runs ×2, hooks, managers, images, attention, kv ×2, concurrency ×2, reload commits — owning rows, sorting, chips, empty states and expandable detail. There are ZERO hand-rolled `<table>`s left, and adding one is a regression: the component is where table behavior lives. The per-hook runs status facet is declared `local: false` because that filter is applied SERVER-SIDE before the row cap, so the component must never re-apply it; the concurrency tables and the per-hook KV browser use `detailFor` for their drill-downs, KV's asynchronously since a key's value is fetched on expand). All three are loaded by ts/timeline.ts and fed by dashboard.js, which passes cell CSS into the shadow root via the element's styleText. Fix component bugs upstream in js-snippets, never here; testjs/ is the node-run client harness proving the push-first section feed (authored in TypeScript, run DIRECTLY via `node --test`'s native type-stripping — no build step; CI pins Node with actions/setup-node). the convention is to author/commit `.ts` source, not generated `.mjs` (gitignored via `*.mjs`; a genuine edge-case `.mjs` can be `git add -f`'d). The one deliberately-committed generated artifact is the dashboard adapter's `assets/timeline.js` bundle — a `.js` (not caught by the `*.mjs` rule), embedded via go:embed and regenerated via the go:generate
internal/hooks/            hook.json + manager.json models, loader, registry, watcher, git repo
internal/managers/         the manager entity's runtime: unbounded inbox (checkout/settle handles) + supervisor (flock lease, flat restarts, output ring, attention seam)
internal/reloadgate/       hooks-repo reload CI gate: /_reload event handling (push records, status switches), last-good persistence, admin-force bypass
internal/concurrency/      named concurrency groups (central concurrency.json) + semaphore manager (+ operator limit overrides)
internal/overrides/        operator kill switch: disabled hooks + concurrency limit overrides, persisted to <data-dir>/overrides.json
internal/scheduler/        per-hook "schedule" interval timer (pure timing; Fire callback dispatches the run)
internal/jsonc/            shared JSONC comment-stripping (hook.json + concurrency.json)
internal/runner/           docker run dispatch + output streaming + image build/status; containerargs.go is the ONE place `docker run` argv is assembled, for all three container starts (hook run, manager instance, hook tests)
internal/runs/             in-memory run tracker (bounded) + the OnFinish persistence seam
internal/runstore/         bbolt-backed persistent completed-run history (48h retention, GC sweeper)
internal/events/           in-memory activity feed (bounded ring; nil-recorder safe)
internal/attention/        aggregated ACTIVE misconfigurations (the needs-attention surface: GET /attention + the dashboard's red banner; nil-aggregator safe)
internal/spool/            durable park for deliveries arriving during shutdown drain (replayed by the next process)
internal/kv/               disk-backed per-hook KV store (state socket) + HMAC namespace tokens
internal/backlog/          durable per-hook batch backlogs (drain-a-slice; behind /backlog)
internal/kvproxy/          TCP->Unix proxy shim injected into state hooks (plain localhost URL)
internal/githubstatus/     GitHub commit status API client (credential resolved per call, so secret-server can supply it)
schema/                    JSON schemas for hook.json + manager.json + concurrency.json — published to buildhost sites (.github/workflows/schemas.yml) AND go:embed'd (embed.go) so the loader enforces the same contract at runtime. hook.schema.json + manager.schema.json are GENERATED from src/ (src/common.json holds the 24 shared property constraints ONCE; each overlay adds its own properties and the per-entity prose) — regenerate with `go test ./schema -update`; a drifted checkout fails TestGeneratedSchemasMatchSources
e2e/                       end-to-end test (shell script, requires Docker)
dats/                      black-box CLI-contract tests (.dats YAML, org dats runner — see "CLI contract tests" below)
examples/hooks/            sample hook configs
docs/                      the depth CLAUDE.md points at (internals/, design docs)
```

## Conventions

- **Always use `go-toolchain`** from the repo root. Don't run bare `go build`,
  `go test`, or `go mod tidy`. Export
  `GOPRIVATE=github.com/wow-look-at-my/secret-server` first: that dependency
  (the published secret-server client) is a PRIVATE module, and a checksum
  database can never contain one, so without it every `go mod tidy` dies on
  `verifying module: ... 404`. ci.yml sets it workflow-wide.
- **The secret-server client is IMPORTED, never reimplemented.** Its contract
  has edges (a 200 body is secret material; 200 `{}` is a configuration answer;
  a failed fetch must not be cached) that this repo got to discover by writing
  its own copy — which now lives in secret-server as
  `github.com/wow-look-at-my/secret-server/client`.
- **No CGO.** `CGO_ENABLED=0` is enforced by the Dockerfile build stage.
- **No Docker SDK.** Shell out to `docker` via `os/exec`.
- **A set is `go-containers/set.Set`,** never `map[K]bool` or
  `map[K]struct{}`. go-toolchain's vet analyzer FAILS the build on the
  first form and warns on the second, and there is no per-line exemption.
  A `map[K]bool` whose false values carry meaning is a real map — keep it,
  and keep its literals from being all-true.
- **A workflow comment is ONE line.** `go-toolchain@v1` embeds
  `wow-look-at-my/actions@yaml-comment-block`, which fails CI on any run of
  more than one `#` line in a workflow (a blank line does not split a run).
  One line is enough for the fact a next editor breaks without; the rest is
  deletion, never a doc holding the evicted prose -- that was tried here and
  reverted. The action's `exclude` input is for deliberate fixtures; using it
  on this repo's own workflows would be gate-weakening, so do not.
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
  Keep the Go model, the JSON schema, and the example/e2e fixtures in sync
  — editing the schema means editing `schema/src/`, never the generated
  `*.schema.json` (see the shared-base bullet under "Things easy to get
  wrong").

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

**The suites are SANDBOXED and a file cannot opt out.** dats sandboxes
commands BY DEFAULT (bubblewrap, falling back to docker); a file-level
`sandbox:` block only NARROWS, and `sandbox: false` is a parse error that
stops the suite loading at all. Only `--no-sandbox` on the run disables it,
which is the caller's decision, not the suite's. Both files declare
`network: false` and nothing else — these tests are offline by
construction. The binaries they exec stay reachable because go-toolchain's
dats phase stages them under `build/`, inside the module root, which is the
one host path a sandboxed command can read.

**dats needs a sandbox backend, so both jobs that run it are GitHub-hosted**
— `dats`, and `test` via go-toolchain's dats phase. They used to pin
`vars.CI_RUNNER_DIND`; that fleet cannot serve a job at all while `dind`
grants no privilege (docs/internals/nested-containers.md), and an unservable
pin is a queue, not a runner. `ubuntu-latest` costs paid minutes on a private
repo, which self-hosted-runners.md otherwise forbids — operator instruction,
for the Docker-dependent jobs only. The slim `wow-linux` fleet can supply
bubblewrap too, via `seccomp.userns` alone, so the jobs that need no daemon
stay on it.

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
- **State KV API** -- served on a **Unix socket** (NOT a TCP port), default `$TMPDIR/whr-state.sock`, reached by a `state: true` hook at a plain `http://localhost:9002` (`HOOK_KV_URL`) through the injected proxy shim. The bearer token DERIVES the namespace, never the URL, so a hook can only ever reach its own data. KV get/put/delete/list/incr, run-owned locks (try/block/steal/pin), the declared sleep `POST /wait`, the mid-flight `POST /title`, the manifest-authorized `POST /spawn`, the durable batch backlogs, and -- managers only -- the `POST /inbox/next` long-poll. Every endpoint, its body and its semantics: docs/internals/state-api.md.

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
API (credential: `WEBHOOK_RUNNER_GITHUB_TOKEN`, else
`PRIVATE_ORG_REPO_READ` fetched from secret-server with the
`WEBHOOK_RUNNER_SECRET_SERVER_TOKEN` machine token — hand-provisioning
the variable is what silently did not happen) — switching only on green, so a
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
- **Fail closed, everywhere.** An undeclared concurrency group, a non-compiling `skip_if` regex, a malformed `run_title`, a mixed hook layout, zero hooks loaded, `settings` that do not match the hook's own `settings.schema.json` — each is a load/validation error that DROPS the hook (or fails the run) rather than running it unbounded.
- **The published schema is enforced at LOAD, by the CI validator itself.** `internal/hooks/schemacheck.go` compiles the EMBEDDED `schema/*.schema.json` (never a fetch — a reload must not depend on the network) and validates every manifest through `wow-look-at-my/json-validator`, the same implementation the hooks repo runs in CI. It runs AFTER the Go checks, whose messages are more actionable where they overlap; what it adds is everything a struct cannot express (enums, patterns, formats, minimums), which was previously checked in CI and nowhere else.
- **A hook's own config is `settings`, never `env`, and never hook.json itself.** One JSON object validated at load against the `settings.schema.json` the hook ships, handed to the container as a read-only `$HOOK_SETTINGS_FILE`. A hook reading its manifest at run time is reading the runner's surface, not its configuration.
- **An operator can override any declared setting from the dashboard, per FIELD.** Stored as entity id + JSON Pointer (`internal/overrides/settings.go`), merged into the manifest at load, re-validated against the same schema, and applied to the next run. Sparse on purpose: a whole-document snapshot would silently freeze every field the operator never touched, so later manifest edits would deploy and do nothing. A rejected override degrades PER ENTITY (serve the manifest values, loudly) and never refuses the tree — unlike a manifest, an override is unreviewed and the tree can move underneath it, so one stale pin must not take the fleet down. The form is generated from the schema, never described server-side.
- **Settings may REFERENCE other values — and the two kinds resolve at different times, deliberately.** `${settings:a.b[2].c}` names another value in the same document: it depends on nothing outside the manifest, so it resolves at LOAD, before the schema runs — the schema validates real values and a typo'd path is a load error. A whole-string reference keeps the referenced value's TYPE (a referenced number stays a number). `${env:NAME}` names a runner-host variable (entity secrets first, then host env): it cannot resolve at validation time (`validate` in CI must never read the runner's environment), so it resolves as the container starts, and an unresolvable one FAILS THE RUN — never an empty string, which is how a hook comes up looking healthy with no credential. The consequence for schema authors is stated in `internal/hooks/settingsref.go`: a field meant to hold `${env:...}` is validated at load against the reference TEXT, so its schema must permit both forms. There is no hidden exemption for referenced fields.
- **`env` is GONE, and how it went is the pattern for every manifest field.** A field cannot be added or dropped in one step across two repos that deploy independently: a runner rejecting `env` cannot serve the fleet that declares it, and a fleet declaring `settings` cannot be served by a runner that predates it. Either way something is unservable at some instant, and no rollback fixes it — the tree never changed, the binary did. So both were accepted for one release, `env` still injecting exactly as it always had (a deprecation that quietly stops working is worse than the flag day), with every use named in the log, the activity feed, `validate` output, and the needs-attention surface — that list WAS the migration's to-do list. It emptied on 2026-08-01, and only then did the field's removal merge; `fleet-compat` is what made the ordering mechanical rather than remembered. `env` is now an unknown field: a load error naming it.

  Retiring the NEXT field should be less ceremonial than this one was. Expand/contract is still the right shape — two independently-deployed artifacts genuinely cannot change a shared contract atomically — but the reason it used to require a red PR and a merge-order runbook is gone: a mismatch is now a failed rollout or a held tree, both loud and both recoverable, so the ordering is an optimization rather than a tightrope. What has NOT changed: accept old and new for one release, name every use of the old field on the surfaces an operator reads, and remove it only once that list is empty.
- **The two manifest schemas share ONE base — never hand-copy a property between them.** They declare 24 of the same properties, and when those were two hand-maintained copies they drifted: the manager's `timeout` lost the hook's duration `pattern` (so `"banana"` validated), `api_key_header` and `enable` lost their defaults. Shared CONSTRAINTS now live once in `schema/src/common.json` and reach each published document as one `$defs.common` block it `allOf`-`$ref`s; prose stays per-entity (a manager's concurrency slot is held for the instance's whole lifetime, its `run_title` placeholders always resolve empty), so each overlay supplies its own description and a shared key with none is a generate error. `go test ./schema -update` regenerates.
- **`unevaluatedProperties: false`, NEVER `additionalProperties: false`, in a composed schema.** additionalProperties does not compose through `allOf`: the `$ref`'d common block is evaluated on its own, sees the entity's `schedule`/`spawn_targets`, and rejects a valid manifest — measured, not theorized. `unevaluatedProperties` is the 2020-12 keyword that accounts for what sibling subschemas matched. The generator applies it and refuses an overlay that reintroduces additionalProperties.
- **`devices` exposes a HOST device node, not container-private storage — an audited grant like `dind`.** `--device` flags rendered by `containerSpec.args()`, wired identically to `volumes` in `runner.go`/`managersession.go`/`tests.go`. Unlike `volumes` it reaches something the OUTER kernel owns, so treat every entry the same weight as `dind`/`seccomp.userns`: only for trusted, operator-curated entities.
- **A manifest may not carry a shell program.** `command` and `script.args` are rejected at load (and by the published schema) when an element contains `$(`, a backtick, `<(` or `>(`. A nested command in JSON is double-escaped, throws away the inner exit status, and can never be linted or run outside the runner. Put it in a `.sh` next to the manifest and call that. Plain `$VAR` references stay legal.
- **The fleet is served ALL-OR-NOTHING, and that is what makes a contract change safe.** A load in which ANY entity fails applies NOTHING (`internal/cli.buildLoadAndApply`). It used to apply the partial set, which fails OPEN in exactly the situation that produces fleet-wide load errors — a binary and a tree that disagree about a manifest field: every entity using it silently stopped serving, with a "hooks reloaded" line to match and no rollback available. Both deploy directions are now closed. **Binary moved** → the startup load errors and serve exits non-zero naming the entities, so a bad image is a FAILED, VISIBLE deploy and the previous container keeps serving. **Tree moved** → the gate resets the working tree to the commit that was serving, re-applies THAT, keeps its old serving record, and never persists the refused sha (`internal/reloadgate.applyOrRollbackLocked`) — the deploy is HELD, not half-applied. Being strict is only safe because a load error cannot reach a gated deploy by the normal path: the reload gate requires the hooks repo's CI green, and that CI runs `validate` through this same loader. Pinned by `internal/cli/loadapply_refuse_test.go` and `internal/reloadgate/refuse_git_test.go`.
- **`fleet-compat` reports the deploy ORDER; it no longer has to be red to keep you safe — ci.yml.** It validates this binary against webhooks **master** (the deployed tree) AND the webhooks branch matching this branch's name (the tree this change is paired with — the same convention webhooks CI uses in reverse). It FAILS only when nothing the change ships with can be served, i.e. there is no order in which it becomes deployable. When master alone fails, that is a `DEPLOY ORDER` notice, because the all-or-nothing rule above makes the wrong order survivable: the worst case is a failed rollout or a held tree, both loud, neither destructive. Before that rule existed this job was the only thing between a contract change and a silent fleet-wide outage, so it had to red every coordinated pair until its partner merged — which meant shipping red PRs with a merge-order runbook attached. Don't reintroduce that: the runtime owns the safety, CI owns the telling.
- **New hook.json fields are deploy-first — but no longer dangerously so.** `Parse` uses `DisallowUnknownFields`, so an older binary REJECTS a hook using a newer field. Deploy webhook-runner first; if you don't, the all-or-nothing rule above turns it into a held tree (the gate rolls back and keeps serving), not a fleet missing entities.
- Hooks, concurrency groups, schedules and managers reload together through ONE closure (`buildLoadAndApply`). Never add a second reload path.
- **ONE builder assembles every `docker run` argv** (`internal/runner/containerargs.go`). A hook run, a manager instance and a hook's tests are three lifecycles, not three kinds of container: they differ by FIELDS on a `containerSpec`, never by hand-built slices. This is what makes a fleet-wide property hold instead of being re-checked three times — mirror routing reaches every container because the builder injects it, and no container joins another PID namespace because there is no field to ask for one. **`internal/runner/bannedflags_test.go` fails the build on `--pid`, `--privileged` and `systempaths=unconfined`** — each hands a hook a piece of the host, and docker's defaults already withhold all three, so every ban costs nothing until someone reaches for the first search hit. `--pid` would put the host's process table in the container's `/proc`, which is what makes dats' read-only `/proc` bind safe on the slim fleet; the other two are host root by another name. **`dind: true` is therefore a MOUNT and nothing else** (`internal/runner/dind.go`): a nested daemon started as root cannot come up in an unprivileged container, and neither can any other nested container runtime — the kernel refuses runc a fresh procfs while docker's ten locked `/proc` submounts are visible. Measured, with the reproduction: docs/internals/nested-containers.md.
- **Filter BEFORE the cap, in every listing.** `max`/limit bounds what is RETURNED, never what is EXAMINED (`/runs?exclude=`, `/events?exclude=`+`?hook=`). Page-then-filter blanks a surface on exactly the busy hooks it exists for: a burst of excluded entries fills the page, the filter empties it, and the panel reports "nothing here" while the matches sit just behind them.
- **An image tag is a content hash, so anything derived from it is cacheable.** `imageCommand`'s `docker inspect` is memoized per (tag, command) — it used to be a full CLI + daemon round trip on every state-hook run, between slot acquisition and container launch. Never cache an inspect FAILURE: that is a daemon condition, not a property of the tag.
- **A phase mark that is missing means UNKNOWN, never zero.** Container overhead is measured, not estimated (`internal/runs` phase marks) — but only a hook whose container reports from the inside yields an EXACT boot figure; every other hook gets an upper bound that also contains its runtime's cold start. Never let the two meet in one number.
- **GitHub does not re-send a failed delivery.** A draining server therefore PARKS deliveries (`internal/spool`) and answers 202 — never 503 "the sender will retry". Shutdown order is load-bearing and is itself a test (`internal/cli/shutdown.go`, `shutdown_test.go`): EVERY listener stays up across the drain — the hook port so deliveries spool, the state socket because draining runs still use it, and the admin port because a drain measured in whole CI jobs is exactly when an operator needs the dashboard.

Read before changing any of these areas:

- [docs/internals/hooks-images-and-reload.md](docs/internals/hooks-images-and-reload.md) -- the CI-gated reload, cancellation, per-hook `settings` + their schema, secrets refs, the two tree layouts, image immutability, the containerized-TMPDIR hazard, hook tests, `dind`, `script`.
- [docs/internals/runs-concurrency-and-overrides.md](docs/internals/runs-concurrency-and-overrides.md) -- the activity-based timeout, `skip_if`, `run_title`, concurrency groups, the global run cap, the operator kill switch, the scheduler, the run store.
- [docs/internals/streaming-and-attention.md](docs/internals/streaming-and-attention.md) -- the SSE hub's never-block invariant, the five section-signal seams, the needs-attention surface.
- [docs/internals/delivery-durability.md](docs/internals/delivery-durability.md) -- deploy windows: `/restart-ready`, the delivery spool and its replay, the shutdown ordering, and the port-down gap none of it covers.
  The admin mux also serves `/.well-known/docker-updater/{health,pre-update}` as ALIASES of `/health` and `/restart-ready` --
  the paths docker-updater discovers by itself. The image EXPOSEs both ports (metadata only, publishes nothing), and discovery
  picks a port itself only from an image declaring exactly one -- so deploy with `docker-updater.well-known.port: "9001"`; the
  older `docker-updater.pre-check.url` still wins where set, and marks the container "nonstandard" for as long as it is.
- [docs/internals/settings-editor.md](docs/internals/settings-editor.md) -- operator settings overrides: the sparse per-field shape, the four rules that make one safe, why a rejected override must not refuse the tree, the one-shape API, and the schema-to-control table the dashboard form is generated from.
- [docs/internals/kv-and-locks.md](docs/internals/kv-and-locks.md) -- the KV store, run-owned locks, try/block/steal, pinning.
- [docs/internals/backlogs.md](docs/internals/backlogs.md) -- the batch-backlog primitive: push-as-set-union, take-removes, depths, how it differs from internal/queue, and why a hook must never build a cursor instead.
- [docs/internals/managers-and-gateway.md](docs/internals/managers-and-gateway.md) -- managers (an instance is NOT a run), the push-fed admin surface, and unconditional github-state-mirror routing.
- [docs/internals/waits-and-spawn.md](docs/internals/waits-and-spawn.md) -- declared waits and the manifest-authorized spawn primitive.
- [docs/internals/run-phases.md](docs/internals/run-phases.md) -- lifecycle phase marks: what each one means, the exact-vs-bounded boot rule, the shim's in-container report.
- [docs/internals/shim-and-timeline.md](docs/internals/shim-and-timeline.md) -- the state-socket proxy shim and the dashboard timeline adapter.
- [docs/manager-entity-design.md](docs/manager-entity-design.md) -- the manager entity design, as built.
