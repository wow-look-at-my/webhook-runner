# Gotchas: the reload gate, hook layouts, images, and hook.json fields

The CI-gated hooks-repo reload, cancellation, per-hook settings, secrets and their refs, the two hook-tree layouts, image immutability, the containerized-TMPDIR hazard, hook tests, and the dind/script fields.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- The hooks-repo reload CI gate (`internal/reloadgate`) moves the tree
  along THREE paths — the former "event-driven only, no polling, ever"
  doctrine was superseded by explicit operator order (2026-07-17): a
  missed status webhook must never freeze deploys indefinitely, so the
  reconciliation poll below is deliberate, not a regression.
  (1) The HMAC-verified `status` event — the PRIMARY, low-latency switch
  authority. A `push` NEVER moves the tree (it fetches + records the tip
  pending, loudly: `reload.held` + the "reload"-source attention
  entries). The ordering rule for a green: the sha must be in the
  freshly-fetched recent history (`fetchDepth` 100) AND not older than
  the serving sha — stale/out-of-order greens are `ignored_stale`, never
  applied.
  (2) The hourly reconciliation POLL (`reloadgate.Poller` →
  `Gate.Reconcile`; `WEBHOOK_RUNNER_RELOAD_POLL_INTERVAL`, default `1h`,
  `0` disables, unparseable/negative fails startup) — the fallback that
  bounds a missed webhook's cost to one interval: one pass at startup
  (catching a green missed while down), then one per interval. Each pass
  fetches the remote tip; tip == serving is a quiet no-op with NO API
  call; a newer tip has the gating context's state read from the combined
  commit status (`githubstatus.ContextState` — reusing the
  GitHub credential (`WEBHOOK_RUNNER_GITHUB_TOKEN`, else `PRIVATE_ORG_REPO_READ`
  read from secret-server with `WEBHOOK_RUNNER_SECRET_SERVER_TOKEN`);
  owner/repo derived from the
  hooks-repo URL, both SSH and https forms) and switches ONLY on an
  affirmative green, through the exact same trySwitch ordering path as
  (1) — never a forked copy. Red/pending/no-status-yet hold via the same
  `reload.held`/`held_red` bookkeeping; an UNREADABLE status (no token,
  API error, underivable URL) falls back to a RECORDED VERDICT and, absent
  one, holds BLIND — `reload.poll_blind` + the `KeyReloadPoll` attention
  entry, one event per distinct problem, and it is impossible for the poll
  to switch to a tip that is not affirmatively green.
  RECORDED VERDICTS (`internal/reloadgate/verdicts.go`): every terminal
  gating status the gate accepts is written to a bounded, TTL'd sha->state
  map in the same state file (`verdicts`, additive/omitempty), INCLUDING
  greens the ordering rule then refuses — a delivered verdict is a
  verified fact about that sha, and discarding it is what left the poll
  buying it back from an API it may have no credential for (the 2026-07-25
  rollback: the gate had applied `802df44`'s green, rolled back, then held
  blind asking GitHub about it). `readGatingState` asks the API FIRST and
  its answer always wins — the poll exists to catch what the webhook
  missed, so a stale record must never mask a fresher red — with the
  record standing in only when the API cannot answer at all. The store
  answers "is this sha green?", never "should the tree switch to it?":
  trySwitch's recent-history + not-older-than-serving checks still gate
  every apply, so a record is an input to that rule, not a bypass.
  Repeat ticks over an unchanged verdict are quiet. The poll makes
  the repo webhook's Statuses-event checkbox a latency optimization, not
  a correctness requirement. GATED MODE ONLY: with the gate disabled the
  poller never starts (one log line; legacy stays timerless).
  (3) Admin `POST /reload` — the DELIBERATE operator bypass (Force: reset
  to tip, recorded verified, `reload.forced`).
  MANUAL PICK (the dashboard's reload panel, `POST /reload/switch`):
  `Gate.ManualSwitch(ref, override)` rides the SAME Force-style apply
  path (`forceApplyLocked` — Force generalized to a target sha; ONE
  switch mechanism, zero forks), deliberately WITHOUT trySwitch's
  staleness ordering so rollback to an OLDER commit works and a wedged
  gate (CI unreadable) stays overridable. Informed override is
  SERVER-enforced: a pick whose gating CI state is not affirmatively
  green ("unknown" counts as not green) or whose tree lacks `src/hooks`
  answers 409 naming every reason + `requires_override:true` and moves
  NOTHING (`reload.switch_refused`); only an explicit `override:true`
  switches — loudly, `reload.forced` naming each overridden reason. A
  green+src pick records `reload.switched` (verified). Pending
  bookkeeping stays consistent: picking the pending commit or the tip
  clears the hold; a rollback elsewhere KEEPS a hold for a different
  commit visible. The automatic paths (1)/(2) are byte-for-byte
  unchanged — and note a rollback away from a GREEN tip lasts only until
  the next green delivery/poll re-switches to it (inherent: the gate
  converges on the newest green; pin by reverting the commit instead).
  The last-good sha persists in `<data-dir>/reload-gate.json`
  (temp+rename; a persist failure is loud but never blocks the reload)
  and is restored at boot BEFORE the watcher's initial scan — gate mode
  never pulls-to-tip on startup (`hooks.OpenRepo`, vs legacy
  `CloneRepo`'s pull-on-open), and `Startup` never calls apply (the
  watcher's initial scan does the first load). Setting
  `WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT` to an EMPTY string disables the
  gate (exact legacy behavior everywhere, including startup); unset means
  `all-builds`. Operator setup: the hooks repo's webhook should send
  `status` events in addition to `push` (same URL/secret) for low-latency
  switches, and that credential needs read access to the
  hooks repo's commit statuses or the poll fallback holds blind.
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
- **Every manifest is schema-validated at load** (`internal/hooks/schemacheck.go`).
  The loader compiles the EMBEDDED `schema/hook.schema.json` /
  `schema/manager.schema.json` (package `schema`, `go:embed`) and validates each
  hook.json/manager.json through **`wow-look-at-my/json-validator`** — the same
  library the hooks repo's CI runs, so "passes CI" and "loads at runtime" are one
  statement rather than two implementations that drift. Details that matter:
  the schema is embedded, NEVER fetched (a reload cannot depend on the network,
  and the binary can only honestly enforce the contract it carries — that is
  what deploy-the-runner-first means); the gate runs AFTER the Go decode and
  `validate()`, because their messages are the better ones where they overlap
  ("invalid schedule 5 minutes" beats a pattern mismatch), while the schema adds
  what a struct cannot express — enums, patterns, `format`, minimums, required
  combinations; format assertions are ON (json-validator's deliberate deviation
  from the 2020-12 default), so a `$schema` or `target_url` that is not a URI is
  a load error; JSONC is handled by the same `jsonc.ToJSON` path as the CLI. A
  failure DROPS the entity like any other load error.
- **Per-hook settings** (`internal/hooks/settings.go`): a hook's OWN
  configuration is one `settings` object in its manifest, an arbitrary JSON
  shape the runner never interprets, and it MUST ship a `settings.schema.json`
  next to the manifest describing what it accepts. The runner validates one
  against the other AT LOAD and DROPS the hook on a mismatch — a hook is never
  started with configuration its own schema calls wrong, and "unconfigured"
  (a required property absent) is a load error rather than a hook that starts
  and no-ops. Declaring `settings` with no schema is refused: config with no
  contract is the state this replaced. The document reaches the container as a
  read-only mount at `$HOOK_SETTINGS_FILE` (`{}` when none is declared, so a
  hook reading its config has no missing-file branch), written PER RUN — the
  image content hash covers the hook's source, so config baked into the image
  could only change by rebuilding it; a settings edit takes effect on the next
  run. This replaced hook.json's `env` block, which mixed hook-private config
  into the runner's own parsed keys, forced every value to be a string, and was
  validated by nobody. The admin API exposes the top-level KEY NAMES only.
- `${NAME}` references in hook.json (`api_key`) are expanded
  at run/request time via `hooks.ExpandEnvRefs`, never at load time —
  `validate` in CI must pass without the production environment or keys.
  Resolution order: the hook's decrypted `secrets.sops.env` first, then
  the host environment. An `api_key` whose reference is unresolvable
  fails closed (401 for everyone).
- Per-hook sops secrets (`hooks.SecretsLoader`, `secrets.sops.env`)
  decrypt by exec'ing the `sops` binary (`WEBHOOK_RUNNER_SOPS_BIN`
  overrides; key material like `SOPS_AGE_KEY_FILE` is plain sops config
  on the service env), cached per file by mtime+size. The runtime image
  (`Dockerfile`, alpine) bundles `sops` (and `age`) so the server can run
  this exec in-container; the age *identity* is mounted at runtime via
  `SOPS_AGE_KEY_FILE`, never baked in. The decrypt runs host-side in the
  server process — the hook container only ever receives the plaintext
  values as env vars, so hook images need nothing sops-related. Decrypted
  entries are injected into the container env (a secret that would shadow a
  key the runner sets itself is skipped with a warning). Note the split: sops
  entries are SECRETS delivered as environment; a hook's CONFIG is `settings`
  and never an env var.
  Decrypt failures fail the run (status `error`) before the container
  starts — never run a secrets-bearing hook without its secrets. The e2e
  fixture key at `e2e/age-test-key.txt` is intentionally committed.
- Hooks trees have TWO layouts (internal/hooks/layout.go), detected by ONE
  rule — `<root>/src/hooks/` exists ⇒ src layout, else legacy — applied
  identically in serve/validate/test because they all load through
  hooks.LoadDir/LoadLayout. Under the src layout: hooks at
  src/hooks/<id>/, shared code in one or more SHARED DIRS — every
  immediate child of src/ that is not hooks/ or managers/ (src/sdk/ is
  the generic-infrastructure one; a tree may carry others, e.g.
  src/actions-runner/ for one domain's shared code), imported relatively
  (../../sdk/...), concurrency.json at
  cfg/concurrency.json — repo-root cfg/, deliberately OUTSIDE src/
  (concurrency config is repo-wide config, not source) — read by
  concurrency.LoadFile at the layout-resolved path (Load(root) is the
  legacy-only shorthand), and the
  docker build runs with CONTEXT src/ + the hook's own Dockerfile via -f
  (tree-mirror COPY convention: `COPY sdk/ /app/sdk/` +
  `COPY hooks/<id>/ /app/hooks/<id>/` + `WORKDIR /app/hooks/<id>` so the
  same relative import resolves in-repo and in-image). Layouts are NEVER
  mixed — a root-level hook dir under the src layout is a HARD ERROR
  (IgnoredLegacyDirError, one per offending dir, naming it): NOT loaded,
  and loud enough to fail `validate` (non-zero exit, message "mixed hook
  layout: top-level hook directory <dir> is not allowed when src/hooks/
  exists ...") and every `serve` reload (logged + recorded as
  hook.load_error) — NEVER a silent skip, so a stray top-level hook left
  by an incomplete move to the src layout turns CI RED instead of quietly
  vanishing from the fleet. SCOPED to MIXED layouts ONLY: the guard
  (`findIgnoredLegacyDirs`) fires solely when `src/hooks/` exists, so a
  pure-legacy tree with no `src/hooks/` sibling — e.g. this repo's own
  `examples/hooks/` and `e2e/hooks/` fixtures — is never scanned for it
  and stays 100% valid. Content hashing:
  legacy stays BYTE-IDENTICAL to the historical algorithm (golden-hash
  test — never change it, or every deployed hook re-tags on upgrade); the
  src layout hashes src/hooks/<id>/ AND EVERY shared dir (src-relative
  path + mode + bytes, never sibling entities), so a shared-code edit
  re-tags every src-layout entity while hook A's edit never re-tags hook
  B; the COPY surface is therefore the shared dirs + own hook dir ONLY (a
  sibling entity's dir builds fine but never re-tags — undefined
  staleness, document don't debug). Hashing EVERY shared dir, not just
  src/sdk, is what lets a tree organize shared code by what it IS instead
  of piling everything into src/sdk to be covered: a shared dir outside
  the hash is worse than no sharing, since editing it re-tags nothing and
  every consumer silently keeps running the old copy. ZERO hooks loaded is a LOUD, typed failure
  (ZeroHooksError) in BOTH layouts: validate exits non-zero, serve logs +
  records it via the normal load-error event path every reload — the
  guard that stops a premature repo restructure from taking the fleet
  offline behind green CI. Layout detection re-runs on EVERY reload (a
  hooks-repo pull can restructure the tree); the watcher additionally
  watches src/, src/hooks/*, and root cfg/ under the src layout (not the
  shared dirs — shared-code edits matter at image-build time, not reload
  time).
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
  payload, NO `settings`, and NO secrets — they must be
  self-contained, which is what lets a hooks repo's CI run them without
  production keys. The per-command timeout (`--timeout`, default 10m) is
  deliberately independent of the hook's run `timeout` (sized for
  production work, not unit tests). The Dockerfile requirement, missing
  `image` field, and `tests` all need a runner binary with these
  semantics — `Parse` uses `DisallowUnknownFields` and old binaries
  demand `image`/`command` — so deploy webhook-runner before merging
  hooks that rely on them.
- `dind: true` (hook.json, a plain opt-in bool like `state`) maps to
  EXACTLY two docker-run flags — `--privileged` and
  `--mount type=volume,dst=/var/lib/docker` — injected on BOTH the
  live-run path (`runner.execute`, before the image)
  AND the `webhook-runner test` path (`runner.runOneTest`, before the
  image); that run/test parity is load-bearing so a dind hook's declared
  `tests` can start a nested daemon under `webhook-runner test`. The
  anonymous /var/lib/docker volume is REQUIRED, not decorative: an inner
  daemon's overlay2 storage can't stack on the outer container's overlay
  rootfs, so it needs a real volume — and `--rm` (always passed)
  auto-removes it, so inner storage never leaks between runs. The host's
  docker daemon is NEVER exposed (no host socket mount); the nested daemon
  is a throwaway. `--privileged` is host-root-equivalent, so this is an
  AUDITED capability — enable it only for trusted, operator-curated hooks.
  It is deliberately first-class rather than raw docker args: `dind` covers
  both paths with one greppable boolean, and there is no general
  raw-docker-args escape hatch at all, so every privilege a container gets
  is a named, reviewable field. New hook.json field ⇒ same
  deploy-first rule as `state`/`schedule` (old binaries reject it via
  DisallowUnknownFields).
- **`seccomp.userns` and the `/proc` masking — the opt-in that does NOT
  exist.** `seccomp: { userns: true }` runs the container under docker's
  default profile plus an ungated allow for
  `unshare`/`clone`/`clone3`/`setns` and `mount`/`umount2`/`pivot_root`
  (`internal/runner/seccomp.go`), so a hook can build an unprivileged user
  namespace and furnish it. That is the whole grant: no capability, no
  `--privileged`, every other syscall keeps the default policy.

  A container refuses bubblewrap for a SECOND, independent reason, and there
  is deliberately no opt-in for it. Docker masks parts of `/proc` (a size-0
  tmpfs over `/proc/acpi` and friends, a `/dev/null` bind over `/proc/kcore`
  and friends, read-only self-binds of `/proc/sys` and `/proc/sysrq-trigger`),
  and inside a user namespace the kernel refuses a fresh procfs while anything
  obscures the one already visible (`mount_too_revealing`, `fs/namespace.c`),
  so bwrap's `--proc` fails with `Can't mount proc on /newroot/proc: Operation
  not permitted`. Measured: the tmpfs alone, the `/dev/null` bind alone, and
  the read-only bind alone each reproduce it exactly.

  `--security-opt systempaths=unconfined` clears the masks and therefore looks
  like the missing half of this opt-in. **It shipped for one afternoon and was
  removed by operator ruling: a hook may not be able to disrupt the host.**
  Every write-capable path it uncovers is root-owned — `/proc/sysrq-trigger` is
  `0200`, `/proc/sys/*` and `/proc/irq/*/smp_affinity` are `0644` — and a hook
  container runs as root, so one write to `/proc/sysrq-trigger` reboots the
  host. `/proc/sys/kernel/sysrq` does not gate that: `write_sysrq_trigger`
  calls `__handle_sysrq(c, false)`, and the kernel's comment there reads
  "Should we check for enabled operations (/proc/sysrq-trigger should not)".
  It is not an escape primitive (the classic escapes need `CAP_SYS_ADMIN` or a
  host bind), but a reboot is disruption enough. `-o subset=pid` is not a way
  round it either: mainline exempts restricted procfs variants from the
  visibility rule, no shipping kernel has that branch (`v6.18` does not), and
  measured on 6.18 both variants are refused under masking.

  The masking is not the runner's problem to solve, because the SANDBOX can
  solve it without any grant: dats binds the container's existing `/proc`
  read-only where the kernel refuses a private one, keeping `--unshare-pid`
  and every containment property, and announcing the reduction. See
  [dats' docs/sandbox-masked-proc.md](https://github.com/wow-look-at-my/dats/blob/master/docs/sandbox-masked-proc.md).
  If a future sandbox needs a fresh procfs in a container, fix it there.
- `script` (hook.json) is parse-time sugar for `command`:
  `Hook.resolveScript` derives `<interpreter> <file> [args…]` (bash,
  pwsh, node, or tsx), resolving the file with `EvalSymlinks` and
  rejecting anything outside the hook directory. It sets nothing else —
  no image, no mounts: the script is baked into the hook's image like
  all code, so the interpreter must be installed in that image (the
  webhooks repo's `Dockerfile.common` base ships bash/node/tsx). An
  explicit `command` wins over `script`. New hook.json field ⇒ same
  deploy-first rule as `state`/`schedule`/`concurrency_group`.
- **A manifest may not carry a shell program.** `command` and
  `script.args` are rejected at load when any element contains `$(`, a
  backtick, `<(` or `>(` — command, arithmetic or process substitution
  (`internal/hooks/shellsafe.go`, and the same rule as a `not.pattern` in
  both published schemas, so CI and the loader agree). What a nested
  command in JSON costs: it is escaped twice (once for JSON, once for the
  shell) so nobody can read it; its exit status vanishes into the outer
  string, so a failed `sed`/`curl` silently becomes an empty argument; and
  it can never be run, linted, or tested outside the runner. Put the
  program in a `.sh` next to the manifest, COPY it into the image, and
  call that (`"command": ["sh", "run.sh"]`, or the `script` field) — a
  real file gets `set -eu`, shellcheck, and a stack trace. Plain `$VAR` /
  `${VAR}` references stay legal: `$HOOK_PAYLOAD_FILE` and friends are the
  point, and there is nothing nested to hide in them.
