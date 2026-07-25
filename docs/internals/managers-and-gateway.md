# Gotchas: managers and the enforced-GitHub gateway

Managers as a first-class sibling entity to hooks -- an instance is not a run -- and the fail-closed GitHub API gateway.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- MANAGERS (`internal/managers` + `internal/hooks/manager.go` +
  `internal/runner/managersession.go` + `internal/server/managers.go`):
  persistent, single-instance watchers as a first-class SIBLING entity to
  hooks — declared at `src/managers/<id>/manager.json` (+ mandatory
  Dockerfile; SDK layout only, legacy trees never scanned; ids share ONE
  namespace with hooks, collision = loud load error). The things to hold
  straight: (1) an INSTANCE IS NOT A RUN (operator ruling) — it never
  touches the tracker, /runs, the timeline, or the runstore; it carries
  an instance id in the run-id alphabet so `kv.Token(managerID,
  instanceID)`, locks (incl. pinning), /wait, /title, and /spawn reuse
  verbatim, with the supervisor's `OnInstanceEnd` as the finish-seam
  analog (lock release + `lock.released_on_finish`); spawned WORKER runs
  stay normal tracked runs. (2) Deliveries feed a BOUNDED INBOX (256,
  drop-oldest loudly — `manager.inbox_dropped`), never boot containers;
  dispatch order is kill switch → auth → skip_if → inbox (a skip_if
  match answers 200 skipped + `manager.skipped`, no run record); the
  manager consumes via long-poll `POST /inbox/next`, and CALLING NEXT
  AGAIN is the ack — it settles the previous event as processed, which
  is what completes `synchronous` delivery holds (200 processed;
  drop/abandon = 500; timeout-degrade to 202 per the hook sync rule) and
  per-delivery `github_status` (pending on accept, success/error on
  settle; ticks post nothing). (3) The WATCHDOG arms only while an event
  is checked out or queued unconsumed — an idle parked long-poll is
  healthy FOREVER; /wait and output bytes touch it (`TouchInstance`).
  (4) Single-instance = kernel flock on `<data-dir>/managers.lock` (flat
  2s poll, fail-closed on errors) + deterministic container name
  `webhook-runner-mgr-<id>` + orphan `docker rm -f` before every start;
  restart is FLAT 10s forever (no backoff, no give-up), with failures on
  the attention seam (`manager` source) until an instance holds.
  (5) `enable` defaults TRUE exactly like hooks (features ship enabled
  and working, never dormant-gated — the org-wide shipping rule; the
  dashboard switch is an emergency control). (6) Full hook field parity, manager-shaped:
  `concurrency_group` = the instance holds one slot for its LIFETIME;
  `run_title` = instance panel title; `dind` = same two flags; plus the
  manager-ONLY `spawn_targets` (the /spawn grant — see the spawn
  bullet); only
  `state` (implied) and `schedule` (superseded by `reconcile_interval` —
  coalesced flat ticks + one `start` event per instance; omitted =
  event-only, first-class) are REJECTED at parse. (7) Reloads: a
  content-hash change supersedes the live instance ("superseded by
  reload"); managers reload atomically with hooks/groups/schedules
  through the same `buildLoadAndApply` (internal/cli/loadapply.go) —
  don't fork a second reload path. Deploy-first rule as usual: old
  binaries never scan `src/managers/`, so the runner deploys before the
  first manager directory merges.
- The enforced-GitHub gateway (`internal/runner/managersession.go`
  `gsmArgs`/`GSMConfig`, wired in runner.execute + runOneTest +
  RunManagerSession): with `WEBHOOK_RUNNER_GSM_URL` set, every
  hook/manager/test container EXCEPT the `WEBHOOK_RUNNER_GITHUB_DIRECT`
  csv exemptions gets `--add-host api.github.com:0.0.0.0` (fail-closed
  blackhole) + a `GITHUB_API_URL` env DEFAULT pointing at the gateway
  (injected BEFORE hook env, so a hook's own value wins — the blackhole,
  not the env var, is the enforcement). Unset (the default) = ZERO
  docker args, byte-identical behavior — the knob is an operator
  infrastructure flip gated on the gsm caching fixes (PR B), not a
  feature gate. The runner's OWN GitHub calls (githubstatus posts, the
  reload poll) follow the knob via `gh.SetAPIURL` —
  `WEBHOOK_RUNNER_GITHUB_API_URL` overrides that separately. Run+test
  parity is load-bearing (the dind precedent): both paths inject the
  same args, so a hook's `tests` see the same network posture as its
  runs.
