# Gotchas: managers and github-state-mirror routing

Managers as a first-class sibling entity to hooks -- an instance is not a run -- the push-fed admin surface, and unconditional github-state-mirror routing.

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
  stay normal tracked runs. (2) Deliveries feed an UNBOUNDED INBOX (it
  used to stop at 256 and drop the oldest), never boot containers;
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
  don't fork a second reload path. (8) The admin surface is PUSH-fed:
  `Supervisor.SetOnChange` (fired by the output sink, the inbox, and
  every state transition) dirties the "managers" stream section — see
  the streamhub bullet — which is what keeps the roster and the
  `#manager=<id>` drill-down live without polling. Client side, the log
  tail follows the bottom ONLY while the operator is already there (a
  refresh per output line must not yank a scrolled-back reader down) and
  the "Instance up" row ticks locally between refreshes, so a silent
  instance's panel still reads as live. Deploy-first rule as usual: old
  binaries never scan `src/managers/`, so the runner deploys before the
  first manager directory merges.
- github-state-mirror routing (`internal/runner/managersession.go`
  `GSMBaseURL`/`gsmArgs`, wired in runner.execute + runOneTest +
  RunManagerSession): EVERY hook/manager/test container is launched with
  `-e GITHUB_API_URL=https://github-state-mirror.pazer.io`, and the
  runner's own GitHub calls (githubstatus posts, the reload poll) use the
  same base via `gh.SetAPIURL`. **UNCONDITIONAL — there is no knob**
  (operator ruling 2026-07-25: "*Everything* must go through GSM
  otherwise we are blowing up our API quota and github servers for ZERO
  benefit"). `WEBHOOK_RUNNER_GSM_URL`, `WEBHOOK_RUNNER_GITHUB_DIRECT`
  and `WEBHOOK_RUNNER_GITHUB_API_URL` are DELETED; do not reintroduce an
  off switch or a per-id carve-out. **GSM IS A PROXY, NOT A FIREWALL**
  (operator correction, same day: "GSM is not a blackhole") — #98's
  `--add-host api.github.com:0.0.0.0` was never requested and is gone.
  The mirror passes through whatever it does not model, so pointing
  GITHUB_API_URL at it IS the mechanism; blackholing would only break
  callers that cannot honor GITHUB_API_URL (tenant CI job steps), which
  is breakage, not routing. NOTE a transparent DNS redirect of
  api.github.com to the mirror is NOT possible: the mirror terminates no
  TLS itself and its edge serves a `github-state-mirror.pazer.io`
  certificate, so any client opening `https://api.github.com` fails
  hostname verification (it would need a cert for api.github.com, which
  no public CA will issue). The injection lands BEFORE hook env (docker
  keeps the last -e), so a hook.json declaring its own GITHUB_API_URL
  still wins — today pr-minder and required-builds declare this exact
  base, so the injection makes those lines redundant rather than
  conflicting. Run+test parity is load-bearing (the dind precedent).
