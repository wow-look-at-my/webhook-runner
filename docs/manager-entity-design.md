# Managers: persistent, single-instance watchers as a first-class webhook-runner entity

Design document, updated to AS-BUILT for the webhook-runner PR (the
"manager entity" PR A) and carrying the operator's rulings on every open
question (section 17 records them). Operator instruction, verbatim:
"why is the coordinator implemented as a hook? I directly told you it needed
support from webhook-runner. Make persistent, single-instance
watchers/managers a first-class entity as you were instructed."

Operator ruling on shipping posture, verbatim: "When i ask you to build a
feature, that doesn't mean built it and find a new way to disable it. It
doesn't mean gate it behind defaulted-off configs. I asked you to add it
because i want to use it." Consequence, applied throughout: managers (and
the PR-C coordinator) ship ENABLED and working -- `enable` defaults true,
exactly like hooks; correctness comes from verifying BEFORE ship (draft ->
review -> merge), never from shipping disabled. Kill switches are
emergency controls defaulting ON.

Operator addendum 1, verbatim: "inject something into docker or whatever so
that api.github.com is blocked for the webhooks, forcing them to go through
github-state-mirror." Operator addendum 2, verbatim: "if there are bugs in
GSM then obviously fix them instead of just leaving tech debt there,
bypassing it in a hacky way, and causing more downstream failures." Both are
folded in as central decisions (sections 8-9).

Survey basis: webhook-runner @ origin/master 1608c29 (/spawn merged #95
a9dd040, tests/docs #97, schemas-to-buildhost #93), webhooks @ origin/master
1ac8ff0 (gha-coordinator v2 #158 merged dormant). The
wow-look-at-my/github-state-mirror repo is NOT in this session; every gsm
mechanism below that needs its source is explicitly marked
**[confirm against gsm source]**. All file:line references are to the two
in-session trees.

---

## 1. Problem statement

Three of the fleet's hooks are not webhooks. They are persistent,
single-instance reconcilers wearing the ephemeral per-delivery-container
costume, and each has grown compensating machinery to fake persistence and
single-instance-ness on a runtime that provides neither:

- **pr-minder**: per-SUBJECT run locks with acquire-or-STEAL choreography
  (its own concurrent runs cancelling each other to fake latest-event-wins
  -- the operator-cited "pr-minder stealing its own subject lock" race),
  delivery-gap replay, comment-unstick as a healing path, one cold Node
  container boot per PR event.
- **required-builds**: per-SHA lock steal + a `defer:` fallback counter, DO
  alarms emulated as KV records drained at the tail of EVERY handled event,
  settle windows implemented as declared in-run waits because no process
  outlives an event.
- **gha-coordinator (#158)**: a `reconcile` run-owned lock whose contended
  path exits 0 to collapse bursts, `dispatched:` anti-storm markers, an
  `active:` repo set persisted only because no memory survives a run, a
  dormancy ENV gate because the entity has no first-class enable, and a
  schedule field it cannot declare while dormant without clockwork
  container spam.

All of that compensates for the same two missing runtime properties: a
process that persists across events, and a guarantee that exactly one
instance exists. This design adds those properties to webhook-runner as a
first-class entity -- the **manager** -- and maps the three consumers onto
it. A second, coupled decision (operator addenda): GitHub access for the
whole fleet is consolidated through github-state-mirror as an ENFORCED
gateway, with gsm's known caching defects fixed at the root as part of this
work -- because a reconciling manager is only as sound as the ground truth
it reads.

## 2. Entity model (summary)

A **manager** is declared in the hooks tree at `src/managers/<id>/`
(`manager.json` + `Dockerfile` + code -- the exact hook layout, new
directory). The runner supervises ONE long-lived container per manager:

- **Persistent**: the container runs indefinitely; the runner restarts it on
  ANY exit after a FLAT delay, indefinitely -- no backoff, no give-up cap
  (constraint 1).
- **Single-instance**: exactly one instance fleet-wide, enforced by a
  kernel-arbitrated flock lease in the shared data dir plus deterministic
  container naming with orphan cleanup (section 5; constraint 2).
- **First-class instance identity**: each container instance carries an
  instance id (run-id alphabet, NOT a run -- operator ruling: instances
  never appear in the runs list, the timeline, or the runstore), against
  which the KV token, cooperative locks, `/wait`, `/title`, `/spawn`, and
  the activity watchdog all reuse their existing mechanisms (section 4).
  Spawned WORKER runs stay normal timeline runs.
- **Event-fed**: `POST /hook/<id>` deliveries for a manager are
  authenticated and skip_if-filtered exactly like hook deliveries, then
  pushed into a bounded in-memory inbox instead of booting a container; the
  live manager consumes them over the state socket via long-poll
  `POST /inbox/next` (section 6).
- **Reconciling**: the runner injects synthetic `tick` events into the inbox
  on the manager's declared `reconcile_interval` (coalesced; one immediately
  on session start), so a manager's main loop is one queue: deliveries are
  the fast path, ticks are the ground-truth floor. Event-only managers
  (no interval) are first-class too -- pr-minder's no-tick doctrine
  (section 7).
- **Worker-spawning**: managers call the existing `POST /spawn`; the
  `WEBHOOK_RUNNER_SPAWN_ALLOW` allowlist works verbatim with manager ids as
  parents (section 10).
- **gsm-fed**: a manager's GitHub reads ride the enforced gsm gateway like
  the rest of the fleet (sections 8-9); the gateway's cache is made sound
  enough to reconcile against, rather than routed around.

Hooks are untouched. A manager is not a hook variant -- it is a sibling
entity with its own declaration file, schema, loader path, and dashboard
surface. IDs share one namespace with hooks (collision = load error),
because the public endpoint, KV namespace, overrides store, and spawn
allowlist are all keyed by bare id -- which is precisely what makes
migration a `git mv` with full state continuity (section 13).

## 3. Decision: long-lived container, not in-process Go (constraint 4)

**Chosen: (a) a long-lived supervised container built from the same tree
layout** (`src/managers/<id>/` with manager.json + Dockerfile + code).

Rationale:

1. **The consumers are TypeScript.** pr-minder, required-builds, and
   gha-coordinator all exist as Node/TS codebases with in-image test
   suites. In-process Go would force three rewrites; the container form
   makes each migration a directory move plus a main-loop rewrite.
2. **Container-is-the-contract is the repo's constitution** (webhooks
   CLAUDE.md: "the container is the contract... whatever the Dockerfile
   installs is what runs"). Managers keep it: code baked in, never mounted;
   `whr-mgr/<id>:<content-hash>` images built by the same `EnsureImage`
   machinery; a tree switch re-tags and the supervisor replaces the running
   instance with the new image (section 11).
3. **Isolation and blast radius.** A wedged or leaking manager is killed by
   `docker kill` like any run; an in-process Go manager could take the
   runner down with it.
4. **The runtime support the operator ordered lives in the RUNNER** --
   supervision, lease, inbox, routing, gateway enforcement, observability
   are Go, in webhook-runner. Only the domain logic stays in the tree.

Rejected: (b) in-process Go (three rewrites, deploy coupling, blast
radius); (b') Go plugins / embedded interpreters (worse on every axis).

Content-hash semantics: `hooks.ContentHash` SDK-layout rules apply
unchanged -- hash `src/managers/<id>/` + `src/sdk/` (src-relative path +
mode + bytes). An sdk edit re-tags every manager AND every hook, as today;
manager A's edit never re-tags manager B. Build context is `src/` with the
manager's Dockerfile via `-f` (tree-mirror COPY convention:
`COPY sdk/ /app/sdk/` + `COPY managers/<id>/ /app/managers/<id>/`).

## 4. First-class instance identity: a manager instance is NOT a run

**Operator ruling** (reversing this design's original session-as-run
proposal): a manager instance must NOT be modeled as a run. It never
appears in the runs list, the timeline, or the runstore -- a
forever-running manager would permanently pollute the timeline (one
eternal bar per manager drowning the actual work), and run history is for
WORK ITEMS, not supervised daemons. The rejected alternative (register
each instance in the run tracker and inherit every run surface) is kept
here as the record of why: it bought token/lock/spawn reuse cheaply, but
at the cost of making the primary observability surface lie about what a
run is.

As built, a manager instance carries its own identity -- an **instance
id**, minted per session by the supervisor -- that deliberately shares the
run-id ALPHABET (26-char lowercase base32, `internal/managers.NewInstanceID`)
so every run-id-parameterized mechanism reuses verbatim WITHOUT the
instance being a run:

- **KV token**: `kv.Token(managerID, instanceID)` -- the existing
  three-part per-run HMAC, unchanged code; namespace == manager id, so
  migrated entities keep their KV data. Tokens die with their instance
  (the hook rule).
- **Cooperative locks (incl. pinning, 10b)**: held under the instance id.
  The finish-seam analog is the supervisor's `OnInstanceEnd` callback
  (wired in cli/serve.go to `kv.ReleaseRunLocks(instanceID)` + the
  `lock.released_on_finish` event), so an instance that ends for ANY
  reason drops its locks -- the same guarantee runs have, at the same
  seam shape.
- **/spawn parent check**: the server's `managerCaller` check accepts a
  token whose namespace is a declared manager AND whose instance id is
  the CURRENT one (supervisor-verified). A stale token from a dead
  instance 409s -- API-level single-instance enforcement, same property
  the session-as-run tracker check would have given.
- **/wait, /title**: manager branches in the state handlers -- /wait
  holds + touches the instance's watchdog (`TouchInstance`), /title names
  the instance panel (`SetInstanceTitle`). Same request shapes, no run
  bookkeeping.
- **Activity watchdog**: the runner's `idleWatchdog` reused against the
  instance (section 7c) -- armed/disarmed by inbox checkout state, not by
  run lifecycle.
- **Output**: the supervisor keeps a bounded per-manager output ring
  (`OutputTail`), surfaced on `GET /managers/{id}` and the dashboard's
  manager page -- NOT the run modal.
- **Spawned WORKER runs stay normal runs**: everything a manager
  dispatches via /spawn is a real tracked run with timeline bars, runstore
  history, group gating -- the manager/worker split is exactly the point.

Instance terminal outcomes (supervisor bookkeeping + events, never run
statuses): container exit -> restart after the flat delay (`failure` in
the manager's status + attention until an instance holds); watchdog kill
-> `timeout` outcome, same restart; operator disable, tree-switch replace,
runner shutdown, manager removal -> requested stops (graceful `docker
stop`), no restart (park/exit). Exit 0 restarts too -- a manager is not
supposed to exit.

Deltas from hook runs: no per-delivery payload mount (the session gets one
synthetic `{"trigger":"manager"}` payload file for path-compat; real input
is the inbox) and no per-delivery container boots. Everything else from
the hook feature set IS carried, manager-shaped (section 12) -- the
operator rejected v1 field cuts: "you can't lose existing functionality,
implement this properly."

## 5. Single-instance: the lease (constraint 2)

Two runner processes can briefly coexist under docker-updater rolling
updates, so single-instance MUST hold across processes and across crashes.

**Chosen mechanism: one kernel flock + deterministic container names +
orphan cleanup.**

1. **The lease file**: `<data-dir>/managers.lock`. The supervisor set (one
   goroutine group per process) takes `flock(LOCK_EX|LOCK_NB)` on it before
   starting ANY manager, polling FLAT (2s) until acquired. The data dir is
   the shared host bind mount both rolling containers see; flock on a bind
   mount arbitrates across containers on one host kernel.
2. **Why flock, not a heartbeat-TTL record**: the kernel releases a flock
   the instant its holder dies -- no heartbeat renewals, no TTL tuning, no
   clock skew, no false expiry when the holder stalls; takeover latency on
   crash is the poll interval, not a TTL. It is also the codebase's
   established cross-process arbitration pattern: `runstore.Open` already
   gates the whole process on a bbolt flock with fail-fast
   (internal/runstore/runstore.go:104-106), which means today's rolling
   overlap already resolves by the new process crash-looping on the runs.db
   flock until the old one drains and closes. `managers.lock` makes the
   manager guarantee EXPLICIT rather than an accident of runstore
   internals. The constraint asked for "a real leader lease (persisted,
   heartbeat-renewed, expiring)": flock is the same property implemented by
   the kernel -- persisted (a file), renewed (implicitly by process
   liveness), expiring (on process death, immediately). Proposed as the
   stronger form; CONFIRMED by operator ruling 2 (section 17).
3. **One lock for all managers**, not per-manager: managers never split
   across processes (nothing gained; handover much harder). The holder
   process supervises the whole set.
4. **The orphan problem**: containers belong to dockerd, not the runner
   process -- a CRASHED runner leaves its manager containers running while
   the kernel releases the flock. Deterministic names close the hole: the
   container is always `webhook-runner-mgr-<id>` (no run-id suffix), and
   the supervisor's acquire sequence is: take flock -> `docker rm -f
   webhook-runner-mgr-<id>` for every declared manager (kills any orphan;
   no-op when none) -> start fresh sessions. Docker's name uniqueness
   additionally makes two same-named manager containers impossible mid-race.
5. **Graceful handover (deploy)**: the OLD process's shutdown path stops
   every manager session (docker stop with grace, section 11) BEFORE its
   flock releases (process exit). The NEW process flat-polls the flock,
   acquires, cleans orphans, starts sessions. State survives handover
   because manager durable state lives in the KV store (disk-backed,
   namespace-scoped, process-agnostic files under `<data-dir>/kv/`) and in
   ground truth (GitHub via gsm); in-memory working sets are caches a fresh
   session rebuilds via its start-of-session reconcile (section 7). The
   manager CONTRACT states this: anything a manager cannot afford to lose
   goes in KV; memory is disposable.

Scope note: flock is host-scoped, which matches the deployment fact (one
host, one dockerd, docker-updater rolling). A multi-host future would swap
the lease implementation behind the same supervisor seam; out of scope.

## 6. Event routing (constraint 5)

`POST /hook/<id>` keeps working for manager ids -- same public endpoint,
same tunnel, same secrets. The dispatch order mirrors handleTrigger's
load-bearing sequence (internal/server/handlers.go:123-253):

1. **Registry lookup**: id resolves to a manager -> the manager path.
   Unknown ids keep the `hook.unknown` event + 404.
2. **Kill switch**: effectiveDisabled(id) -- same overrides store, same
   tri-state semantics, same loud 503 + rejection event (constraint 9).
3. **Body read + auth**: the SAME `authenticate()` -- api_key / ed25519
   public_key / legacy HMAC `secret` are manager.json fields with identical
   semantics. pr-minder's and required-builds' committed secrets (== their
   Apps' webhook secrets) carry over untouched.
4. **skip_if**: evaluated exactly as for hooks (same compiled conditions,
   same auth-first ordering). A match answers `200 {"status":"skipped"}`
   and records an event -- but NO run record is fabricated (there is no
   per-delivery run; skip_if's value for managers is pure noise filtering).
5. **Enqueue**: the delivery `{kind:"delivery", received_at, headers,
   payload}` is appended to the manager's bounded in-memory inbox (default
   256 entries) and answered `202 {"manager": id, "status": "queued"}`.
   `?wait=true` / `synchronous` do not apply to managers (ignored).

**Inbox semantics** (the constraint-5 questions, answered explicitly):

- **While the manager is restarting / mid-handover within one process**:
  the inbox buffers; the new session drains it in arrival order. The
  rolling-update handover puts the hook PORT on the new process before the
  old one's managers stop, so deploy-window deliveries land in the NEW
  process's inbox and are consumed when its sessions start.
- **Overflow**: drop OLDEST, loudly (`manager.inbox_dropped` event).
  Newest-wins matches the fleet's latest-event-wins doctrine. The delivery
  is still 202'd -- coalescing is operating-as-designed; GitHub must not
  see errors for it.
- **Process crash**: the inbox is in-memory and dies with the process.
  DELIBERATE -- the reconcile loop is the correctness mechanism and events
  are a latency optimization; that is already the operating doctrine of all
  three consumers (required-builds' drains, pr-minder's
  reconcile-on-contact, the coordinator's markers + ticks). A durable queue
  would be a second source of truth with its own retention/GC semantics
  (storage-has-semantics). Stated plainly: **202 means "accepted", never
  "will be processed"; reconcile is the guarantee.**
- **Manager down long (crash loop)**: inbox fills, oldest drop loudly, the
  attention surface is red (section 12), and the first tick of the next
  healthy session re-derives from ground truth.

**How events reach the live container**: the manager long-polls
`POST /inbox/next` (body `{"wait_seconds": 1..600}`) on the state socket --
the surface it already has mounted, authenticated by its session token.
Response: `200 {event}` or `204` (window elapsed, re-poll; flat). Pop
semantics are at-most-once: an event handed out and lost to a crash is
covered by reconcile. Ticks arrive on the same queue, so a manager's whole
main loop is:

    for (;;) {
      const e = await inboxNext();        // long-poll, flat re-poll
      if (e.kind === 'tick') await reconcile();
      else await handleDelivery(e);       // fast path
    }

Rejected transports: a manager-side HTTP server (container port publishing
and networking -- exactly what the kvproxy shim exists to avoid); stdin
framing (fragile, docker attach complexity); SSE (more protocol, no gain).

**State socket over a long-lived container**: today the runner bind-mounts
the socket FILE; a runner restart unlinks + re-binds the path, and a
running container's file mount would point at the dead inode -- fine for
short-lived hooks (the drain gate exists for exactly this), wrong for
managers. Managers do not outlive the runner in this design (stop/start
handover, section 5), but belt-and-braces: the socket moves to
`$TMPDIR/whr-state/state.sock` (dir created at boot; env override kept),
hooks keep their file mount unchanged, managers bind-mount the DIRECTORY --
directory mounts track member inode replacement, and the kvproxy shim's
per-connection dial (existing flat 250ms/10s retry,
internal/kvproxy/kvproxy.go:16-29) picks up a re-bound socket on the next
request. The shim binary is exec'd at container start; later on-disk
replacement is irrelevant to the running process.

## 7. The reconcile contract (flat cadences: honored)

- `reconcile_interval` (manager.json, optional, Go duration): the runner
  enqueues a synthetic `{kind:"tick"}` inbox event every interval, FLAT --
  plus ONE immediately at session start (the scheduler's
  fire-immediately-on-registration precedent), which is the handover /
  crash-loss recovery pass. Coalescing: at most one tick queued at a time
  (a slow manager never accumulates a tick backlog). No backoff, no
  adaptive cadence, no give-up -- ever.
- **Event-only managers** (no `reconcile_interval`) are first-class: the
  entity must hold pr-minder, whose operator doctrine explicitly killed
  scheduled sweeps. Such a manager gets `{kind:"start"}` at session start
  instead of a tick (it may run a warm-up or ignore it); a start tick that
  swept would be a sweep by the back door. pr-minder ignores it;
  reconcile-on-contact remains its healing model.
- The manager MAY additionally self-time in-process (required-builds'
  settle windows become plain in-process timers); the runtime tick is the
  floor, not the ceiling.
- **Operator ruling (Q3/Q4)**: BOTH pr-minder AND required-builds migrate
  EVENT-ONLY -- no `reconcile_interval` for either; "the less polling you
  do the better." Settle windows and drain deadlines become in-process
  timers armed by the events that create them (a timer serving a specific
  pending obligation is not polling; a cadence that re-scans the world
  is). `reconcile_interval` remains available for managers whose ground
  truth genuinely cannot be event-covered (gha-coordinator's queued-job
  backlog, where a lost workflow_job delivery has no healing event).

### 7c. Liveness: the watchdog, repurposed (crash-restart supervision)

`manager.json` `timeout` keeps the hook field's ACTIVITY semantics with a
manager-shaped arming rule: the watchdog is armed while an event is
**checked out** (delivered by /inbox/next and not yet followed by the next
/inbox/next call) and touched by any output byte. A manager that takes an
event and goes silent past `timeout` is wedged: killed via the existing
docker-kill path, session ends `timeout`, supervisor restarts after the
flat delay. A manager parked in its long-poll with an empty inbox is
healthy and owes nothing -- event-only managers can be silent for hours
without being reaped. Additionally, inbox depth > 0 with nothing checked
out for > `timeout` counts as wedged (the manager stopped consuming).
Default 5m, per-manager tunable. Supervision cadences: restart delay after
any non-operator session end FLAT 10s (a build-broken manager restarts and
fails every 10s until the tree is fixed -- loudly, which is the correct
pressure); lease poll FLAT 2s; no backoff, no restart budget, no give-up.

## 8. GitHub access: gsm as the ENFORCED single gateway (operator addendum 1)

Verified current routing (reported separately; recap): ONLY pr-minder and
required-builds route through gsm today, via opt-in env
`GITHUB_API_URL=https://github-state-mirror.pazer.io`
(pr-minder/hook.json:196, required-builds/hook.json:190; sdk fallback to
api.github.com at src/sdk/github-app.ts:30). The gha fleet, pr-describe,
pr-resolve, license-block, branch-block, github-api, and webhook-runner's
own GitHub clients (internal/githubstatus/client.go:51, used by commit
statuses and the reload-gate poll) are DIRECT. Webhook deliveries are
tunnel-direct to hooks.pazer.io and never touch gsm.

**New posture: gsm is the fleet's single GitHub API gateway, ENFORCED at
the container layer so nothing can bypass it by accident or drift.**
Per-hook opt-in dies; "reconcile direct" (this design's earlier draft
position) is REJECTED -- it would leave the fleet split-brained across two
read paths with different staleness, and it walks away from the rate-relief
and observability of one gateway. Its motivating evidence (the webhooks#66
staleness class) is real, and is answered by fixing gsm at the root
(section 9), not by bypassing it.

### 8a. Enforcement mechanism (concrete)

One master knob on the runner service env: `WEBHOOK_RUNNER_GSM_URL`
(operator deployment sets `https://github-state-mirror.pazer.io`). When
set, for every hook AND manager container the runner launches, EXCEPT ids
on the exemption list below, `runner.execute` injects:

1. `--add-host api.github.com:0.0.0.0` -- a per-container, fail-closed
   blackhole: /etc/hosts resolves api.github.com to 0.0.0.0, connects fail
   immediately, no host-firewall dependency, no effect on any other
   container or host process. Injected on BOTH the live-run path and the
   `webhook-runner test` path (the dind run/test-parity rule) -- test
   containers are hermetic by contract, and the blackhole turns a test that
   illegally calls GitHub into a red build (requirements-are-CI-checks).
2. `-e GITHUB_API_URL=<gsm url>` as the fleet default -- injected BEFORE
   hook.json env (docker last--e-wins), so the two hooks that already set
   the identical value are unchanged, and a hypothetical divergent override
   still cannot reach api.github.com (the blackhole, not the env, is the
   enforcement). `GITHUB_API_URL` does NOT become a ReservedEnvKey
   (existing hook.jsons legitimately set it).

Explicitly rejected: DNS-pointing api.github.com at gsm's address (TLS/SNI
cert mismatch -- gsm's cert names github-state-mirror.pazer.io; clients
would hard-fail, and impersonating GitHub's hostname is the wrong pattern
even if it "worked"); host-level firewalling (hits host processes and the
exempted CI fleets, coarse, outside the runner's control); per-hook egress
networks (heavy, per-container netns machinery for what one hosts line
does).

Honesty note on the threat model: --add-host defeats accidental/default
resolution, which is the actual risk (operator-authored fleet code drifting
back to api.github.com). It does not stop deliberately adversarial code
(hardcoded IPs, DoH); the fleet is operator-curated, so that is out of
scope -- this is misconfiguration-proofing, not sandboxing.

### 8b. Exemption list (CI payloads cannot ride gsm)

`WEBHOOK_RUNNER_GITHUB_DIRECT` (runner service env, operator-curated,
default EMPTY = everyone enforced): a comma list of ids whose containers
get NEITHER the blackhole nor the env default. Operator sets it to
`gha-runner,gha-runner-dind`. Reason: those containers run GitHub's actions
runner serving ARBITRARY CI jobs -- the runner agent talks to the Actions
service endpoints embedded in its jitconfig (*.actions.githubusercontent.com),
and job payloads legitimately call api.github.com themselves (gh CLI,
octokit in org workflows, actions/github-script); none of that can or
should ride gsm, and the runner cannot rewrite a tenant job's view of
GITHUB_API_URL. The exemption is runner-side env -- a hook can never
self-exempt, so enforcement intent holds. (The gha hooks' OWN control-plane
calls -- JIT-config minting via the sdk client -- follow the env default
only in non-exempt containers; for the exempt runners they stay direct,
which is today's behavior.)

### 8c. Fleet GitHub hostname inventory (what is blocked, what stays direct)

Enumerated from both trees (greps over src/ + internal/):

| Hostname | Used by | Under enforcement |
|---|---|---|
| api.github.com | sdk + per-hook clients (12 fallback refs), webhook-runner githubstatus | BLACKHOLED in non-exempt containers; replaced by gsm. Runner's own client re-based to gsm (8d). |
| github.com (git smart-HTTP) | pr-resolve clone/push (resolve.ts:75 DEFAULT_GIT_BASE_URL) | STAYS DIRECT. gsm is an API mirror, not a git server; different hostname, untouched by the blackhole. Documented, deliberate. |
| github.com (link strings) | required-builds publisher.ts:90 target_url | No traffic -- a URL in a status body. |
| git@github.com (SSH) | webhook-runner hooks-repo clone/fetch (EnsureSSHKey path) | STAYS DIRECT -- git over SSH, not API. |
| ghcr.io, nodejs.org | Dockerfile FROM / build-time downloads | Unaffected -- image builds run on the HOST daemon (EnsureImage streams context to dockerd); enforcement is runtime-container-scoped. |
| *.actions.githubusercontent.com + arbitrary job traffic | gha-runner/-dind (run.sh + CI payloads) | EXEMPT containers (8b). |
| secrets.pazer.io, hooks.pazer.io, model gateway | fleet plumbing | Non-GitHub, unaffected. |

Rule: blackhole exactly what gsm replaces (api.github.com), nothing else.

### 8d. webhook-runner's own GitHub calls

`internal/githubstatus.Client` gets a configurable base
(`WEBHOOK_RUNNER_GITHUB_API_URL`; when unset it follows
`WEBHOOK_RUNNER_GSM_URL`, else defaults to api.github.com) -- covering
commit statuses AND the reload-gate reconciliation poll's
`githubstatus.ContextState` read. The runner process has no /etc/hosts
blackhole (it must reach gsm and, under break-glass, GitHub); its
enforcement is configuration, which the operator controls anyway.

### 8e. Availability: blast radius + break-glass

gsm becomes a fleet-wide single point of failure for GitHub access. Named
consequences when gsm is down (enforcement on):

- Every non-exempt hook/manager GitHub call fails (sdk clients fail loud,
  flat-retry per their existing contracts; runs fail; managers keep
  running and heal by reconcile when gsm returns). Deliveries keep landing
  (tunnel-direct) -- KV, inbox, and spawn are unaffected.
- webhook-runner: github_status postings fail (no current hook uses them);
  the reload-gate poll goes BLIND -- the existing `reload.poll_blind` +
  attention machinery says so loudly, and hooks-tree deploys HOLD until gsm
  returns, the operator manual-picks/forces, or break-glass.
- CI (exempt fleets) unaffected.

**Break-glass is operator-level, not hook-level**: unset
`WEBHOOK_RUNNER_GSM_URL` (or set `WEBHOOK_RUNNER_GITHUB_API_URL` back to
https://api.github.com) on the runner service env and restart -- the whole
fleet reverts to direct in one flip; new containers launch without the
blackhole immediately (per-container injection; in-flight containers keep
their hosts entry until they end, which for hooks is minutes and for
managers is the restart the flip causes anyway). No hook-side bypass
exists, so enforcement intent survives the mechanism.

### 8f. Rate-limit math (flat-cadence doctrine holds)

- gsm passthrough (writes + unmodeled shapes) authenticates upstream with
  the CALLER's own token -- per-caller GitHub limits are identical to
  direct. gsm concentrates AVAILABILITY, not QUOTA: there is no shared
  upstream token to bottleneck. **[confirm against gsm source]** (consumer
  docs say "reverse-proxied to GitHub verbatim with our own token").
- Modeled cached reads REDUCE upstream quota (hits never touch GitHub) --
  the mirror's purpose; newly-funneled consumers (gha-coordinator's
  queued-jobs listings, license-block's sweeps) become candidates for
  modeling later, gaining relief they don't have today.
- Conditional revalidation (the section-9 backstop) is quota-cheap by
  GitHub's documented behavior: 304 responses do not count against the
  primary rate limit; a 200 revalidation means the data really changed and
  the fetch was needed regardless.
- Manager cadences are flat and count-capped (MAX_DISPATCH_PER_TICK-style
  bounds carry over), and single-instance coalescing strictly REDUCES call
  volume vs today's burst-of-containers shapes. No new headroom risk.

## 9. gsm caching fixes -- first-class deliverable (operator addendum 2)

The operator's ruling: fix gsm's bugs at the root; no force-fresh bypass.
A force-fresh/no-cache request mode appears here ONLY as the rejected
alternative: it would leave defects in place for every caller that forgets
the header, split the fleet into fresh-mode and stale-mode readers, and
turn the cache into something consumers negotiate with instead of trust --
the "hacky bypass, more downstream failures" the operator vetoed. The fixes
make the cache itself sound; consumers stay dumb.

**The gsm repo was added to this session mid-design and has been
deep-read** (wow-look-at-my/github-state-mirror @ 37badc2, shallow). The
verdict changes materially from the consumer-side evidence: the #66 freeze
class the fleet's comments describe is ALREADY FIXED at HEAD -- the
consumer docs (pr-minder/handlers.ts:1083-1090, written at the 2026-07-12
incident) predate the fix. What follows is the verified current state and
the REAL remaining defect list.

### 9a. Already fixed at gsm HEAD (do not re-fix; consumer docs are stale)

NOTE for the PR-B implementer: this section's verdicts were read at gsm
HEAD 37badc2. RE-VERIFY them against the gsm source at PR-B time before
building on them -- in particular re-confirm the "#66 fixed at HEAD"
premise below; gsm moves, and a fix list keyed to a stale reading would
re-fix or miss.

- **Same-repo push -> PR-row staleness (the literal #66 class): FIXED.**
  The push apply calls `NullPRMergeableByBranch`
  (internal/sync/webhook.go:205; store: internal/ghdata/store.go:361; SQL:
  queries/ghdata.sql:260-272) FIRST, before any fallible step: it nulls
  `mergeable` + `merge_commit_sha` on every OPEN PR whose base OR head is
  the pushed branch and stamps a VERIFIABLE stale marker
  (merge_stale_sha/at/ref/after). An unparseable push falls back to
  `NullPRMergeableByRepo` (un-resolve ALL the repo's open PRs). The absorb
  paths then REFUSE to re-resolve from a refetch re-offering the
  invalidated sha (presumed pre-push) with two proven exemptions: the
  push-tip proof (answer's reported tip == the push's after sha ->
  provably post-push) and the dirty-retained CONFLICTING pattern past a
  30s replica-lag window (live evidence: webhooks#44/#124, 2026-07-17) --
  internal/ghdata/respcache_pulls.go:41-176, mirrored in the upsert SQL
  (ghdata.sql:137-155). The single-PR route serves only rest-complete,
  recently-touched (PRRowTTL 24h) rows with mergeable KNOWN; null
  mergeable always misses and re-fetches.
- **Response-cache invalidation graph: EXISTS and is per-ref.**
  internal/sync/webhook_invalidate.go: push flushes contents/commits-list/
  compare/commit-CI per pushed ref in every accepted spelling (bare,
  heads/x, refs/heads/x -- refSpellings), PR-files + branches-list +
  pull-diff-406 repo-wide ("the belt for missed pull_request deliveries");
  status/check_run/check_suite flush per-ref commit-CI + per-sha
  workflow-runs; workflow_job/workflow_run flush per-sha BEFORE
  disposition (queued jobs included); repository events flush everything;
  installation events flush the mint + repo-installation caches.
- **Freshness backstops: EXIST.** PRRowTTL 24h (single-PR), pulls-list
  marker 24h, closed-PR docs 24h, workflow-runs pages 24h, all
  lazy-expired + LRU-capped; the freshness layer (internal/freshness)
  tracks ETag/TTL/error-retry per resource for the GraphQL truth sync.
- **A consistency checker: EXISTS** (internal/sync/consistency.go):
  read-only Check plus CheckAndApply, which corrects drift against
  GitHub's live state with the mirror's own App -- including writing the
  nulls "the COALESCE upserts can never write" -- and reports
  per-installation rate limits.

### 9b. Remaining defects (found by reading; the ACTUAL PR-B fix list)

- **G1 -- fork-head freeze: the #66 variant still open.** A fork-head push
  emits NO push webhook to gsm (the fork repo isn't installed), so
  `NullPRMergeableByBranch` never runs; the resulting
  `pull_request.synchronize` delivery carries `mergeable: null` and the
  RETAINED pre-push merge_commit_sha, and the webhook upsert's ELSE branch
  `COALESCE(excluded.mergeable, pull_requests.mergeable)`
  (ghdata.sql:137-155) PRESERVES the stale KNOWN value -- events.go has NO
  synchronize-specific handling (the action switch handles closed/open
  only). Result: the row holds the NEW head sha with the PRE-push
  mergeable/merge_commit_sha, passes the mergeable-KNOWN serve gate, and
  serves the frozen answer for up to PRRowTTL (24h).
  **Fix**: in the pull_request apply, when the incoming head sha differs
  from the stored row's (or action == synchronize / a base-retarget
  edit), un-resolve merge fields and stamp the SAME verifiable marker the
  push path stamps -- merge_stale_ref = head branch, merge_stale_after =
  payload head.sha (the proof tip is IN the synchronize payload), reusing
  the existing marker/exemption machinery wholesale. Small, surgical,
  symmetric.
- **G2 -- consumer-App mint/permission staleness (the required-builds 403
  dance): structurally unfixed.** installation events DO invalidate the
  mint cache (webhook_invalidate.go:163-173) -- but gsm only receives ITS
  OWN webhook feed; a CONSUMER App's permission change delivers
  installation events to that App's webhook (the consumer hook), never to
  gsm, so the invalidation cannot fire for exactly the Apps that mint
  through the mirror (required-builds CLAUDE.md:524-526 documents the
  consumer-side workaround). **Fix**: observe permission-shaped upstream
  failures on proxied calls -- a 401/403 passing through for a caller
  invalidates that caller's cached mint row (internal/ghdata/
  respcache.go:262-303 already has the invalidation primitive), so the
  next mint is fresh; the consumer-side new-JWT dance becomes a harmless
  belt.
- **G3 -- CheckAndApply is operator-triggered only** (dashboard call
  sites: internal/api/browse.go:236, checkstream.go:68; nothing runs it
  on a cadence). The originally proposed fix -- a flat periodic
  CheckAndApply cadence -- is **REJECTED by operator ruling**: a
  recurring re-scan is a bandaid sync that papers over whichever
  invalidation is actually broken. PR B fixes staleness at the ROOT
  (G1's synchronize un-resolve, G2's mint invalidation, G4's list
  gating), each a missed-invalidation bug fixed where the event arrives;
  CheckAndApply stays what it is -- an operator-triggered audit tool.
  Accepted consequence: a webhook gsm never received (delivery lost AND
  outside every invalidation path) heals only via TTL or an operator
  check -- the same posture as today, with the actual bug classes closed.
- **G4 -- list-tier merge fields ungated** (low severity, tighten while
  in there): the open-PR LIST rebuild serves rows without the single-PR
  route's mergeable-known/stale gating (respcache_pulls.go:249-271). No
  surveyed consumer reads merge fields from lists today (pr-minder's
  base-push listing uses `?base=` -- unmodeled, passthrough), so the
  exposure is latent; either gate the rebuilt fields or omit them.
- **G5 -- accepted, documented staleness (no action)**: run DELETION emits
  no webhook (workflow-runs pages age out via 24h TTL; consumers fail
  safe -- respcache_actionsruns.go:44-50); abbreviated-sha CI rows bounded
  by TTL. Documented as accepted in gsm's own comments; concur.

### 9c. Enforced-gsm interaction with the manager fleet (verified)

- **gha-coordinator's reconcile reads are passthrough-fresh through gsm**:
  `/actions/runs?status=queued` and `/actions/runs/{id}/jobs`
  (sdk/actions-runner/queued-jobs.ts:84,121) are DELIBERATELY unmodeled
  shapes (respcache_actionsruns.go models only `?head_sha=` pages) -- so
  under enforcement the coordinator's ground-truth reads stay exactly as
  fresh as direct, with gsm adding observability and the OPTION of
  modeling later. Enforcement adds zero staleness to the first manager.
- **required-builds' hot triple-read rides the modeled per-sha routes**,
  which its own triggering events flush (invalidation precedes
  disposition) -- webhook-coincident freshness, sound under G1..G3 fixes.
- **pr-minder's immutable-shape reads** (the #66-era workarounds) remain
  correct and become redundant belts once G1 lands; simplify later, never
  in the migration commit.

### 9d. Sequencing consequence

Fleet-wide enforcement (section 8) does NOT flip on until the gsm fixes
(G1-G3) are deployed -- otherwise enforcement would extend the fork-head
freeze class to consumers that are direct-and-correct today. The runner PR
ships the enforcement machinery inert (env-gated); the operator sets
`WEBHOOK_RUNNER_GSM_URL` after the gsm PR lands. Interim state = exactly
today's routing. Hard dependency, called out in the plan (section 15).

## 10. Spawn integration (constraint 7)

- `WEBHOOK_RUNNER_SPAWN_ALLOW` is UNCHANGED in format and semantics: parent
  ids may now name managers (ids share one namespace with hooks). The
  operative pair `gha-coordinator=gha-runner,gha-runner-dind` carries over
  byte-for-byte across the migration. Deny-by-default preserved; malformed
  values still fail startup.
- `/spawn`'s parent check gains a manager branch (`managerCaller`): a
  token whose namespace is a declared manager AND whose instance id is
  the supervisor's CURRENT one authorizes the spawn; a stale (dead)
  instance's token 409s. Targets remain HOOKS; managers are not spawnable
  -- they are supervised, not started per-request. A spawn body naming a
  manager as target 404s.
- Attribution: `spawned_by {hook_id: <manager id>, run_id: <instance id>}`
  flows through unchanged; the manager's live log lives on its dashboard
  page. Correct and useful.

## 10b. Lock pinning: stealable <-> not-stealable (operator addendum 3)

Operator, verbatim: "enhance the webhook locking system so you can switch a
lock from stealable to not-stealable and vice versa." The motivating case:
pr-minder's push-resolve -> bless-lease -> arm window, where its OWN push's
event echo steals the subject lock from the very run that pushed (the
own-event steal, "cause 1"). The IMMEDIATE pr-minder hook fix
(sender-check + lease-bless) is the stopgap unblocking stuck PRs now,
handled outside this design; THIS primitive is the durable root fix,
riding the runner PR alongside the manager entity.

Spec against the real code (internal/kv/lock.go, internal/server/state.go):

- **Model**: `lockEntry` (lock.go:55-59) gains `pinned bool`; `LockInfo`
  (lock.go:70-75) gains `pinned` (json, omitempty) so a contended 409's
  `held_by` SHOWS the pin -- a refused stealer sees who holds and that it
  is pinned, never an anonymous refusal.
- **API**: two state-socket routes beside acquire/release/steal --
  `POST /kv/{key}/pin` and `POST /kv/{key}/unpin` (no bodies). ONLY the
  holding run may toggle (the release auth rule: owner check against the
  token's run identity; a non-holder gets 409 + held_by, absent/expired
  gets 404). Idempotent both ways. A separate route pair, not an acquire
  flag, mirroring the steal-route precedent: mode changes should be
  unmistakable in request lines and logs. (Acquire MAY additionally take
  `"pinned": true` for take-and-pin atomicity -- one fewer round trip in
  the pr-minder critical section; the toggle routes remain the primitive.)
- **Semantics**: `takeLockLocked` (lock.go:141-168, the ONE
  compare-and-set acquire and steal share) refuses a steal of a live
  PINNED entry: returns the holder's info + a distinct `ErrLockPinned`;
  the server maps it to 409 with `held_by` (pinned: true) and a
  `lock.steal_refused` activity event. Plain contended acquires behave
  exactly as today (pin changes nothing for them -- they already never
  displace anyone).
- **Lifetime invariants**: a pin NEVER outlives its run. `ReleaseRunLocks`
  (lock.go:208-228, the OnFinish seam) frees pinned locks identically on
  every terminal path; explicit `ReleaseLock` by the owner works pinned or
  not; the TTL backstop still applies unchanged (a crashed holder's pin
  dies with the lock's lazy expiry -- lockEntry.expired ignores pinned).
  Pin protects against STEAL only -- never against the holder's own
  completion, the finish-seam release, or expiry. No new liveness
  mechanism, no pin TTL of its own.
- **Latest-event-wins preserved -- the critical subtlety**: an event
  arriving DURING a pin must not be dropped. A refused stealer falls back
  to the blocking acquire it already has (`{"block": true}` polls flat at
  250ms -- state.go:153, blockOnLock): when the pin lifts or the holder
  finishes (finish-seam release), the waiter's next poll takes the lock
  and processes the newer event -- deferred, never lost. Steal callers in
  the fleet (pr-minder subject locks, required-builds per-SHA, pr-describe
  debounce) treat `ErrLockPinned` as "wait, don't displace": steal ->
  on-pinned-409 -> blocked acquire. And for managers the whole layer
  thins: a single-instance manager serializes its own subjects in-process
  (section 13), so pins matter mostly for the HOOK fleet's interim and for
  manager-spawned WORKER runs' own locks.
- **Leader lease composition**: the manager leader lease is DELIBERATELY
  NOT a special always-pinned lock. The kv lock table is in-memory and
  run-bound BY DESIGN (lock.go:20-26: a restart starts lock-free) --
  correct for run-scoped locks, structurally wrong for a lease that must
  arbitrate ACROSS runner processes and survive restarts; bolting
  persistence + cross-process semantics onto the lock table would fork its
  invariants (the storage-has-semantics rule). The two compose by layer:
  the flock lease (section 5) decides WHICH PROCESS supervises managers;
  manager sessions and hook runs alike then pin SUBJECT locks around
  critical sections via this primitive, short-lived and run-bound. Two
  mechanisms, two lifetimes, one auth model.
- **Placement**: entirely webhook-runner/state-socket work -- no hook.json
  fields, no schema exposure, rides PR A. Older runners 404 the routes;
  callers degrade exactly like every other newer primitive (404/405 =
  unavailable, proceed unpinned -- loud, never wedged).

Rejected alternative: hook-side sender-checks everywhere (each steal
caller inspecting event provenance to decide "is this my own echo?").
That spreads a fragile, per-hook heuristic across the fleet -- every new
event source re-litigates it, fork/App-sender edge cases multiply, and a
missed check re-opens the race. The primitive states the actual intent
(this critical section is not displaceable) once, server-side, with one
auth rule -- and the sender-check survives only as pr-minder's interim
stopgap until this lands.

## 11. Lifecycle state machine

Per manager, in the flock-holding process:

    (no flock)  WAITING-LEASE --flock acquired--> orphan rm -f
    LEASED:
      disabled?          -> DISABLED (no container; overrides-driven)
      else               -> STARTING: EnsureImage -> docker run (instance
                            id minted; token minted; inbox bound; start
                            or tick event enqueued)
      STARTING build/start failure -> outcome `error` -> RESTART-WAIT
                            (flat 10s) -> STARTING
      RUNNING            -> container exits         -> instance outcome
                            (failure/error/success) -> RESTART-WAIT -> STARTING
                         -> watchdog fires          -> kill -> `timeout` ->
                            RESTART-WAIT -> STARTING
                         -> operator disable        -> STOPPING (docker stop
                            -t grace) -> `cancelled: disabled by operator`
                            -> DISABLED
                         -> reload, hash changed    -> REPLACING (stop grace)
                            -> `cancelled: superseded by reload` ->
                            STARTING (new image)
                         -> reload, manager removed -> STOPPING ->
                            `cancelled: removed from tree` -> (gone)
                         -> runner shutdown         -> STOPPING all ->
                            `cancelled: runner shutting down` -> flock
                            released (process exit)

New mechanical piece: **graceful stop** (`docker stop -t <grace>`, default
30s) -- today's runner only docker-kills. SIGTERM lets the manager finish
its in-flight event and flush; the manager contract documents "exit
promptly on SIGTERM".

Reload integration (constraint 3): managers load inside the ONE
`buildLoadAndApply` closure (cli/serve.go:460) alongside hooks, groups, and
schedules -- never a second reload path. The closure diffs the declared
manager set + content hashes against the running set and hands the
supervisor a replace/stop/start worklist. The CI-green reload gate is
upstream and unchanged: a tree becomes visible to the closure only after
the gate switches, so managers inherit exactly the deploy-gating hooks
have. KV state is untouched by a switch (namespace files don't care who
reads them), and the lease does not move (same process): a tree switch is a
graceful in-place replace, not a handover.

## 12. Declaration, loading, observability

### manager.json (the FULL hook field set, manager-shaped)

    {
      "$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json",
      "description": "Reconcile queued JIT-runner jobs and spawn workers",

      // auth for POST /hook/<id> -- the hook trio, identical semantics
      "secret": "…",                  // or api_key / public_key

      // ground-truth floor; OMIT for event-only managers
      "reconcile_interval": "3m",

      // wedged-manager bound (checked-out activity semantics, section 7c)
      "timeout": "10m",

      // noise filtering before the inbox -- hook skip_if, verbatim
      "skip_if": [ { "header:x-github-event": { "ne": "workflow_job" } } ],

      "env": { "…": "…" },            // ${NAME} expansion, sops, reserved keys: identical
      "tests": [ ["npx","tsc","--noEmit"], ["node","--test","x.test.ts"] ]
    }

`enable` defaults TRUE, exactly like hooks -- a declared manager works the
moment it deploys (operator ruling, header). Explicit `enable: false` and
the dashboard switch are the only off switches.

**Operator ruling (Q8, rejecting the original "deliberately minimal v1"
cut list)**: "you can't lose existing functionality, implement this
properly." Every hook field is supported with properly designed
manager-shaped semantics -- carried identically: `script`/`command`,
`user`, `workdir`, `networks`, `volumes`, `extra_docker_args`,
`api_key_header`, `signature_header`, `env`, `tests`, `skip_if`, the auth
trio; manager-shaped:

- `concurrency_group`: the INSTANCE holds one slot for its lifetime
  (acquired before the container starts, released at instance end),
  counted against the same declared limit as hook runs in the group.
- `run_title`: the instance's panel title -- rendered once per instance
  (static templates in practice; there is no per-delivery payload), with
  `POST /title` as the live override.
- `synchronous`: the delivery's HTTP response holds until the manager
  finishes THAT inbox event (its next /inbox/next call -> 200 processed;
  drop/abandon -> 500), bounded by `timeout` as wall clock with the
  hook-style 202 degrade.
- `github_status`: PER-DELIVERY statuses -- pending as an event with a
  repo+sha enters the inbox, success/error as the manager
  finishes/abandons it. Ticks/start events post nothing.
- `dind`: the same two docker flags on session + test paths; the nested
  daemon's storage lives as long as the instance.

The only two hook fields that do not exist on managers, each REJECTED at
parse (loudly, not ignored): `state` (implied true -- a manager cannot
function without the state socket) and `schedule` (superseded by
`reconcile_interval`). `Dockerfile` required, `$schema` required
(presence-only) -- the hook rules.

### Loading

- SDK layout only: `DetectLayout` gains `ManagersDir()` =
  `<root>/src/managers` (scanned only when present; legacy trees have no
  managers). Same loader discipline: per-manager typed load errors;
  malformed skip_if/duration/auth = manager dropped loudly; id collision
  with a hook = manager dropped loudly. The ZeroHooks guard is unchanged
  (hooks remain the fleet-offline tripwire).
- `webhook-runner validate` validates managers (docker-free,
  environment-independent -- the CI authority); `webhook-runner test` runs
  manager `tests` in the built image identically to hooks.
- Watcher: also watches `src/managers/*` (one more entry in the src-layout
  watch list).

### Schema/validation story (constraint 6 -- the trap, closed)

- **NEW `schema/manager.schema.json` in webhook-runner**, published by the
  EXISTING `.github/workflows/schemas.yml` to buildhost sites on every
  master push -- the pipeline that already replaced the quota-frozen Pages
  deploy (operator directive 2026-07-19). Canonical URL:
  `https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json`.
  The frozen legacy Pages content is neither touched nor needed.
- **hook.schema.json is NOT modified. Zero new fields in any existing
  hook.json.** The entity lives entirely in a new file in a new directory.
- Old deployed runner binaries never scan `src/managers/` -- there is NO
  DisallowUnknownFields hazard (the trap that bit `enable` and every new
  hook field is structurally impossible for a new entity directory).
  Deploy-first still applies in the useful direction: the manager-capable
  runner deploys BEFORE the first `src/managers/` commit merges; each
  migration runbook gates on it.
- webhooks CI: `webhook-runner validate .` (built from the matching branch,
  as today) is the enforcement; the `schema` json-validator job extends its
  glob to `src/managers/*/manager.json`, validating against the LIVE
  buildhost URL in each file's `$schema` -- updatable at will, killing the
  frozen-schema failure mode for managers permanently.

### Observability (constraint 8)

- **Admin API**: `GET /managers` (roster: id, state
  [waiting-lease|disabled|starting|running|restart-wait|stopped],
  instance id, started-at, restarts-since-boot, inbox depth, last event
  at, enable default + effective disabled, title); `GET /managers/{id}`
  (detail + the bounded output tail). Instances are NOT runs (section 4):
  nothing manager-shaped appears on /runs, the timeline, or the runstore.
- **Dashboard**: a first-class Managers page (roster + the hooks table's
  slider-switch UX + per-manager detail with live output tail); a new
  `managers` stream section token dirtied by `manager.*` lifecycle events
  (the existing section-invalidation machinery, one more token).
- **Activity feed**: `manager.started`, `manager.exited`,
  `manager.skipped`, `manager.wait`, `manager.inbox_dropped`,
  `manager.lease_waiting`/`manager.leased`,
  `manager.disabled`/`manager.enabled`/`manager.restart_requested` (id in
  the hook field so `?hook=` filters work); delivery rejections reuse the
  existing `hook.unknown`/`hook.denied`/`hook.disabled_rejected` kinds.
- **Attention** (new `manager` source with a real clear rule): an entry is
  ACTIVE whenever desired-state is running but no instance holds (start
  failing, crash-looping, image unbuildable) -- message carries the last
  outcome; CLEARS the moment an instance runs. Disabled managers' entries
  drop at read time (the existing rule).
- **Restart forensics**: the supervisor's per-manager restart counter +
  output ring + the activity feed's `manager.exited` trail -- deliberately
  NOT the runstore (the Q1 ruling).

### Kill switch (constraint 9)

Same `overrides.Store`, same tri-state (`enable` default vs persisted
operator override), same dashboard slider, same orphaned-override
announcements -- and the same DEFAULT as hooks: absent `enable` means
enabled, so the switch is an emergency control, never a go-live gate
(operator ruling, header). Disable stops the instance (graceful) and
parks; enable starts it; reloads re-apply. The lease is retained while
disabled (disable is fleet intent; re-enable must be instant and no other
process may start the manager meanwhile).

## 13. Migration mapping (required section)

Order: **gha-coordinator first** (this wave), required-builds second,
pr-minder last (highest traffic, live production) -- each migration its own
PR + runbook, gated on the previous one's soak. Per operator ruling 7:
pr-minder/required-builds are MAPPED here, not migrated in this wave.

Unifying mechanics: hook id == manager id == KV namespace, so a migration
commit is `git mv src/hooks/<id> src/managers/<id>` + `hook.json ->
manager.json` rewrite + main-loop rewrite. ALL KV state (markers, records,
sets) carries over with zero copying. The public endpoint URL, webhook
secret, and spawn-allowlist entries are unchanged. Rollback is the reverse
move. GitHub reads ride the enforced gsm gateway (section 8) once the
operator flips it -- for gha-coordinator that is a routing CHANGE (direct
today), explicitly gated on the gsm fixes (section 9d).

### gha-coordinator -> manager (FIRST implementation)

- **manager.json**: `secret` (same value), NO `enable` field -- the
  manager ships ENABLED and does its job on deploy: reconcile queued
  wow-linux/wow-dind jobs, spawn workers via /spawn (operator ruling,
  header; the hook's `COORDINATOR_ENABLED` env gate is RETIRED, not
  replaced -- the #158-era ship-dormant pattern is rejected outright),
  `reconcile_interval: "3m"` (the backstop the hook could not declare
  without clockwork container spam), `timeout: "10m"`, the same one
  skip_if condition, same env minus COORDINATOR_ENABLED, same tests.
- **Sheds**: the `reconcile` run-owned lock and its contended-exit-0 burst
  collapse (a single instance serializes in-process; bursts coalesce in
  the inbox); the lockless-degrade path; the retired env-gate machinery;
  per-delivery container boots (one delivery = one inbox append, not one
  cold Node start + secret-server fetch).
- **Keeps**: `active:` set and `dispatched:` markers EXACTLY as-is -- they
  dedupe against spawned WORKER lifetime (a worker run pending in the
  semaphore), not against coordinator concurrency, so single-instance does
  not obsolete them; the semaphore stays the accountant; OWNER_ALLOWLIST
  stays the in-code wall; webhook-provision stays; `/spawn` dispatch is
  verbatim (parent = the manager instance); MAX_DISPATCH_PER_TICK stays.
- **KV continuity**: namespace `gha-coordinator` untouched; markers and the
  active set survive the move.
- **Code shape**: `run()`'s classify/lock scaffolding collapses into the
  inbox loop (`tick` -> reconcile all active repos; `delivery` -> stamp
  active + reconcile); `classify()` shrinks (kind is explicit);
  coordinator-pins/skipif/env-contract suites carry over with small
  contract updates (the worker-secret tripwire stays).
- **Routing delta**: queued-jobs listings move direct -> gsm passthrough
  under enforcement (same caller token, same quota; a future gsm modeled
  route for them is optional relief).
- **Runbook ORDERING (load-bearing, because the manager ships enabled)**:
  an enabled coordinator starts working -- or failing loudly -- the moment
  its tree deploys, so the preconditions land FIRST: (1) the
  manager-capable runner (this PR) deployed; (2) the
  `WEBHOOK_RUNNER_SPAWN_ALLOW` allowlist entries
  (`gha-coordinator=gha-runner,gha-runner-dind`) on the service env; (3)
  the App's repository **Actions: Read** grant. Only THEN does PR C merge
  (which the reload gate deploys on green). Correct-before-ship replaces
  gated-off-for-safety: the PR stays draft until verified + operator-
  reviewed, then ships ON. Webhook flip/retire and DRAIN=0 steps follow
  unchanged; the operator kill switch exists for emergencies, not as a
  deploy step.

### required-builds -> manager (mapped; migrates later)

- **Sheds**: the per-SHA acquire-or-STEAL choreography and the `defer:`
  no-steal fallback (in-process per-SHA serialization with
  latest-event-wins coalescing -- a newer queued event for a SHA supersedes
  an in-flight evaluation IN PROCESS; no run cancellation); the
  drain-at-the-tail-of-every-event pattern as the only timer (a persistent
  process holds real timers: `settleHeldGreen`'s declared-wait-in-run
  becomes an in-process timer at the settle deadline).
- **Keeps**: the KV records (`reconcile:`, lastpub dedup, settle records)
  as the DURABLE crash-recovery layer -- a session can die mid-settle; the
  next session's start pass drains due records exactly as today's drains
  do, concentrated at session start + ticks instead of smeared over every
  event; comment-unstick (still an event); skip_if verbatim; gsm routing
  (already enforced-compatible -- it lives on the modeled statuses/
  check-runs routes, which the D1/D2 fixes make sound); the secret.
- **Operator ruling (Q4)**: required-builds migrates EVENT-ONLY -- no
  `reconcile_interval` ("the less polling you do the better"). Settle
  windows and drain deadlines are in-process timers armed by the events
  that create them (obligations, not polling); a totally quiet org
  converges on its next delivery or comment-touch, exactly today's
  accepted trade.

### pr-minder -> manager (mapped; migrates later)

- **Sheds**: the per-SUBJECT `subject:` acquire-or-STEAL machinery -- THE
  cited race -- entirely: one process serializes per subject in memory (a
  keyed-mutex map), latest-event-wins becomes in-process coalescing (drain
  newer queued events for a subject before starting work on it), no run
  ever cancels another; the lockless degrade paths; per-event container
  boots (its largest cost).
- **Keeps**: every KV marker table row (namespace continuity); the
  `openpr:` create choke point becomes in-process (same guarantee,
  simpler); comment-unstick + reconcile-on-contact as the healing model;
  **event-only: NO reconcile_interval** (the operator killed the sweep;
  the entity honors it -- section 7); the describe/resolve hand-offs
  unchanged (pr-describe and pr-resolve STAY ephemeral hooks -- per-work-
  item workers; the manager/worker split working as intended);
  delivery-gap replay (operator ruling Q5: KEPT -- it reads the App's own
  deliveries log, ground truth the inbox cannot cover across downtime).
  Its immutable-shape reads (the #66 workaround) become
  redundant once gsm's D1/D2 fixes land but are harmless belts; simplify
  later, never as part of the migration commit.
- **Risk posture**: highest-traffic, live-production entity; migrates LAST,
  after the pattern has soaked; rollback = reverse move, markers
  idempotent.

## 14. What does NOT change (constraint 10)

Ephemeral hooks: byte-for-byte identical behavior, schema untouched, zero
new fields (the gateway env/blackhole injection is runner-side and changes
no hook.json). `/spawn`: unchanged. Secrets: sops + secret-server patterns
identical. No committed GitHub credentials anywhere new. The webhooks repo
stays SITELESS (manager.schema.json lives in and publishes from
webhook-runner). The reload gate's three switch paths, the concurrency
manager, the KV store contract, the runstore (managers never touch it --
instances are not runs, so no new record type and the per-hook index value
format is untouched), and the runner's shutdown drain for hook runs: all
unchanged. Webhook deliveries stay
tunnel-direct; gsm never sits in the delivery path.

## 15. Implementation plan -- three PRs, explicit dependency order

**PR A -- webhook-runner: manager entity + gsm gateway enforcement**
(deploys first; purely ADDITIVE: with zero managers declared in the hooks
tree and the gsm knob unset, a runner running PR A is behaviorally
identical to master -- the non-breaking proof. That is the additive-PR
property, not a feature gate: the first `src/managers/` commit that lands
after this deploys starts WORKING immediately, enabled by default):

- `internal/managers`: the inbox (bounded ring + coalesced ticks +
  checkout/settle handles) and the supervisor (lease flock, orphan rm -f,
  instance start/replace/stop, flat restart, output ring, attention seam,
  shutdown ordering) -- instances are first-class identities, never runs
  (section 4).
- `internal/hooks`: manager.json model (`ParseManager` -- the full hook
  field set minus state/schedule, both rejected loudly) + `LoadManagers`
  + layout extension (`ManagersDir`) + registry/watcher coverage.
- `internal/runner`: `RunManagerSession` (group slot held per instance,
  synthetic payload, graceful-stop support, checked-out watchdog arming
  via the inbox bind seam), `docker stop -t` plumbing, gateway injection
  (`--add-host` blackhole + `GITHUB_API_URL` default, exemption list) on
  run + test paths.
- `internal/server`: manager-aware `POST /hook/{id}` dispatch (kill
  switch -> auth -> skip_if -> inbox; synchronous holds; per-delivery
  github_status), `POST /inbox/next` on the state mux, manager branches
  in /wait, /title, /spawn and blocking lock acquires, `GET /managers` +
  `GET /managers/{id}` + disable/enable/restart, dashboard Managers page
  + `managers` stream section, events + attention (`manager` source),
  overrides parity (enable default TRUE, the hook rule).
- `internal/kv` + `internal/server`: the lock pin primitive (10b) --
  `pinned` on lockEntry/LockInfo, ErrLockPinned in takeLockLocked,
  `POST /kv/{key}/pin` / `/unpin` routes, steal-refused event.
- `internal/githubstatus`: configurable base (8d) + the per-delivery
  manager status posts.
- `schema/manager.schema.json` (published by the existing schemas.yml
  glob); validate/test coverage for managers.
- Docs (CLAUDE.md/README + this document as-built).

**PR B -- github-state-mirror: caching fixes** (repo read @ 37badc2 --
RE-VERIFY at PR-B time, 9a note; the fix list, section 9b): G1 --
synchronize/head-move un-resolve of PR merge fields stamping the existing
verifiable marker (the fork-head freeze; the push path's machinery reused
wholesale); G2 -- invalidate a caller's cached mint on a
permission-shaped (401/403) proxied upstream failure; G4 -- gate or omit
merge fields in the list rebuild. ROOT-CAUSE invalidation fixes ONLY: the
originally floated G3 cadence (periodic CheckAndApply) is REJECTED by
operator ruling as a recurring bandaid sync -- CheckAndApply stays an
operator-triggered audit tool. Explicitly NOT re-building what HEAD
already has (9a: the push un-resolve, the per-ref invalidation graph, the
TTL backstops, the checker itself). Independent of PR A; MUST deploy
before the operator flips enforcement on.

**PR C -- webhooks: gha-coordinator as the first manager**: `git mv` +
manager.json + coordinator.ts inbox loop (per section 13); tests updated;
runbook rewrite. The manager ships ENABLED and starts reconciling on
deploy (operator ruling, header), so PR C merges only after its
preconditions hold: PR A's runner deployed, the /spawn allowlist entries
on the service env, the App's Actions:Read grant in place -- the runbook
ORDERING in section 13. Verification happens on the draft PR before
merge, never via a disabled deploy.

**Operator flip sequence** (after A and B are deployed): set
`WEBHOOK_RUNNER_GSM_URL` + `WEBHOOK_RUNNER_GITHUB_DIRECT=gha-runner,
gha-runner-dind`, restart runner (enforcement on, fleet through fixed
gsm); merge PR C once its preconditions hold (it goes live on deploy) and
run the remaining #158 cutover steps (webhook flip/retire, DRAIN=0).
Later waves: required-builds PR, pr-minder PR, each soak-gated.

## 16. Risks

1. **gsm as SPOF**: named blast radius + operator break-glass (8e); the
   reload-gate's blind-hold is the sharpest edge (deploys freeze while gsm
   is down) -- mitigated by the existing manual-pick/force paths and the
   break-glass flip.
2. **Enforcement before gsm is sound**: prevented structurally -- the flip
   is a separate operator action gated on PR B's deploy (9d).
3. **Exemption-list drift**: a new CI-payload-running hook must be added to
   WEBHOOK_RUNNER_GITHUB_DIRECT or its jobs break loudly on the blackhole;
   loud is acceptable (fail-closed), documented in the runbook.
4. **Long-lived container vs state-socket handover**: parent-dir mount +
   per-connection dial retry; e2e must kill -9 the runner under a live
   manager and assert orphan reaping + successor session correctness.
5. **Orphaned manager containers after a runner crash**: rm -f-by-name on
   lease acquire; deterministic names make it airtight on one host.
6. **Watchdog false kills on slow-but-honest event handling**: the
   checked-out arming rule + per-manager `timeout`; a kill is a restart,
   not a loss (reconcile covers), sized generously (coordinator 10m).
7. **Inbox loss on process crash**: accepted and documented (reconcile is
   the guarantee); pr-minder leans on reconcile-on-contact + replay --
   exactly its current posture.
8. **bbolt/flock interplay during rolling deploys**: the new process
   crash-loops on runstore's flock until the old drains -- pre-existing
   behavior the lease matches rather than fights; documented so nobody
   "fixes" one without the other.
9. **gsm fix regressions**: G1's un-resolve must not re-open the
   wrong-mark race the push path already solved -- it reuses the SAME
   marker + push-tip-proof machinery (the synchronize payload's head.sha
   IS the proof tip), and the existing absorb tests pin the exemptions.
10. **pr-minder migration risk** (traffic, production): last in order,
    smallest-diff main-loop port, reverse-move rollback.
11. **Pin misuse** (a hook pinning across its whole run, re-blocking
    latest-event-wins): bounded structurally -- the pin dies with the run
    (finish seam + TTL backstop), refused steals fall back to blocking
    acquires that win on release, and `lock.steal_refused` events make a
    long-pinned critical section visible on the feed.

## 17. Operator rulings (the former open questions, all answered)

1. **Session-as-run modeling: REJECTED.** Manager instances are
   first-class identities, never runs -- they must not appear in the runs
   list, the timeline, or the runstore (a forever-running manager would
   permanently pollute the timeline). Reuse the token/lock/spawn/watchdog
   mechanisms against the instance identity; spawned WORKER runs stay
   normal timeline runs. Section 4 is the as-built record.
2. **Lease form: kernel flock CONFIRMED** (single-host deployment; the
   flock + deterministic names + orphan rm -f composition satisfies the
   lease intent).
3. **pr-minder: EVENT-ONLY confirmed** -- no reconcile_interval; healing
   stays reconcile-on-contact + replay.
4. **required-builds: EVENT-ONLY** -- "the less polling you do the
   better"; settle windows are in-process timers armed by their events,
   not poll ticks.
5. **Delivery-gap replay: KEPT** post-migration (the App's deliveries log
   is ground truth the inbox cannot cover across downtime).
6. **Shipping posture: managers ship ENABLED** (ruling quoted in the
   header, superseding the earlier default-off recommendation outright).
   `enable` defaults true exactly like hooks; COORDINATOR_ENABLED is
   retired, not replaced; the dashboard switch is an emergency control.
   Nothing about the entity -- or PR C -- ships dormant or gated off.
7. **Migration order + wave scope: CONFIRMED** -- gha-coordinator ->
   required-builds -> pr-minder, soak-gated; this wave = PR A + PR B +
   PR C.
8. **v1 field cuts: REJECTED** -- "you can't lose existing functionality,
   implement this properly." The full hook field set is supported with
   manager-shaped semantics (section 12).
9. **Exemption list: CONFIRMED** -- `gha-runner,gha-runner-dind` today,
   operator-configurable via WEBHOOK_RUNNER_GITHUB_DIRECT; the blackhole
   default covers test containers too (hermetic-tests enforcement).
10. **gsm PR-B scope: root-cause fixes only** (G1/G2/G4). The G3 cadence
    is REJECTED as a recurring bandaid sync; CheckAndApply stays
    operator-triggered. Re-verify the 9a "already fixed at HEAD" premise
    against gsm source before building PR B.
11. **Lock pinning: CONFIRMED as specced** (owner-only pin/unpin toggle,
    steal refused 409 + pinned held_by, blocking-acquire fallback
    preserving latest-event-wins), and the leader lease STAYS a separate
    flock layer rather than an always-pinned lock.
