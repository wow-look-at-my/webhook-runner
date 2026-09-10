# The admin port (:9001): endpoints and the dashboard

Every admin endpoint, the per-hook app page, the operator kill switch, the reload panel, the KV views, the manager surface, and the realtime swimlane timeline.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- **Admin port** (`:9001`): dashboard, `/version` (build identity +
  `hooks_tree` state, same as the hook port's; the dashboard footer shows
  the build string), `/restart-ready` (the docker-updater PRE-CHECK —
  `internal/server/restartready.go`: 200 when no runs are in flight, 503
  when any are, so the label
  `docker-updater.pre-check.url=:9001/restart-ready` makes an update skip
  that cycle and retry the next. `/health` cannot serve this — it answers
  "is the process up", always yes. Shutdown ALREADY drains correctly
  (BeginShutdown refuses new runs — deliveries arriving from then on are
  PARKED in the spool and answered 202, see
  [delivery-durability.md](delivery-durability.md) — then an unbounded
  `rn.Wait()` runs before the runstore flock releases, with the hook port
  and state socket still up); what it cannot survive is the SIGKILL after
  docker-updater's hardcoded stop grace — 30s normal, 300s rolling, both
  shorter than a CI job — after which the runs are orphaned and the
  successor's `SweepOrphanContainers` reaps the very `gha-runner` container
  serving a live build. The pre-check keeps the stop from being issued at
  all. Do NOT deploy this container as a *rolling* update: docker-updater
  skips the pre-check entirely when `Rolling` is set (`updater.go:75`).
  LIVENESS: docker-updater retries forever with no max-defer of its own, so
  after `WEBHOOK_RUNNER_RESTART_MAX_DEFER` (default 6h) of CONTINUOUS
  blocking the check answers 200 anyway, loudly (`restart.deferred_force`)
  — a permanently busy fleet would otherwise pin the binary at its current
  version, a silent freeze indistinguishable from a working gate. Any idle
  moment resets the clock; a negative value never forces. Manager instances
  deliberately do not block: a flat restart is their declared contract, and
  gating on them would mean never updating), `/hooks`, `/hooks/{id}` (one hook's
  drill-down: value-free config summary — api_key as a boolean, env var
  names only, never any api_key/env/secret value, `skip_conditions` as a
  count — plus image state, KV namespace stats, and run stats over the live
  tracker window merged with the persisted run history; `stats.retention`
  labels that window and `stats.skipped` is the skip bucket — see the
  skip_if bullet under "Things easy to get wrong"), `/hooks/{id}/diagnostics`
  (one downloadable JSON bundle — `Content-Disposition: attachment` — for
  handing an incident to someone debugging it: the same config summary as
  `/hooks/{id}` plus hooks-tree state, the hook's concurrency-group state,
  its most recent runs newest-first WITH FULL OUTPUT (`?runs=`, default 50
  max 500; `?tail=`, default 500 lines, `-1` = full transcript — the one
  deliberate widening past the rest of this file's value-free contract,
  because `/runs/{id}` already serves output in full to admin-port callers
  and a diagnostic bundle without the logs misses the point of an incident
  bundle), its activity events (`?events=`, default 300 max 2000, every
  kind — not `exclude=run` like the dashboard feeds), and every currently
  ACTIVE needs-attention entry scoped to the hook or server-wide, even for
  an effectively-disabled hook (unlike `/attention`'s dashboard-banner
  view, which drops those — a bundle FOR a disabled hook is exactly where
  an operator wants to see why it got disabled). See
  `internal/server/diagnostics.go`.),
  `/runs` (`?hook=` filters; live + persisted history, deduped by run ID,
  newest-first; `?exclude=<csv of statuses>` drops those runs BEFORE `?max=`
  applies — FILTER FIRST, THEN LIMIT, so a hook whose newest 50 runs are all
  skips still answers 50 non-skipped ones, and an unknown status is a 400
  rather than a silently empty page. It honors `?live=1` too, and overrides
  the always-include-active rule: an explicit filter is a request, not a
  window size), `/runs/stream` (SSE live tail: `retry: 2000`, a connect `snapshot` shaped exactly like `/runs`, then one `run` event per lifecycle change + `hb` heartbeats ~10s + multiplexed `changed` section-invalidation signals (`{"sections":["hooks","kv",...]}` — the dashboard's push channel for /hooks /images /concurrency /kv /events /attention; "changed → refetch once", coalescing, drop-proof); fed by the tracker's OnChange seam through a never-blocking hub — see "Things easy to get wrong"), `/runs/{id}/cancel`, `/reload`, the
  hooks-repo reload panel (`GET /reload/status` — mode gated/legacy/none,
  branch, live commit with CI + src/hooks-tree verdicts, the gate's held
  tip; `GET /reload/commits` — ~20 fetched-fresh origin commits with
  per-commit CI/src/is_live; `POST /reload/check` — reload on demand:
  one `Gate.Reconcile` pass in gated mode / the legacy pull+reload;
  `POST /reload/switch` — the manual commit pick, body `{"ref","override"}`
  — see the reload-gate bullet's manual-pick paragraph under "Things easy
  to get wrong"), `/events`
  (activity feed; `?hook=` filters on the `hook` field every hook-scoped
  event carries, and `?exclude=run[,image,…]` drops whole kind FAMILIES —
  the segment before the dot, so `run` takes `run.started`/`run.finished`
  but never `runstore.*`; a full kind is a 400, since silently matching
  nothing reads as a broken filter. BOTH narrowings are applied by
  `events.ListFiltered` BEFORE `max`: the cap bounds what is RETURNED,
  never what is EXAMINED, so an excluded burst can never crowd the
  survivors out of the page — the same filter-before-limit rule `/runs`
  follows, and for the same reason. BOTH dashboard feeds — the overview
  Activity page and the per-hook section — pass `exclude=run`, because run
  lifecycle already has a richer home in the runs table (one row per run
  with status, timings and output, instead of three log lines); what
  remains is what has NO run to show: rejected deliveries, image builds,
  `env.unresolved`, reload/git activity. Both render with js-snippets'
  `<activity-feed>` component, imported at runtime; it owns the table, the
  derived kind badges and the built-in filtering (free-text +
  severity/family chips, persisted per feed) over whatever the endpoint
  hands it), `/attention` (the aggregated needs-attention problem
  set: `{count, entries:[{source, hook, key, message, since}]}`, oldest
  first — the dashboard's red banner + panel; see the attention bullet
  under "Things easy to get wrong"), `/images` (per-hook image state), the operator kill
  switch (`POST /hooks/{id}/disable|enable`,
  `PUT|DELETE /concurrency/{group}/limit`,
  `PUT|DELETE /concurrency-global/limit` — see the overrides and
  global-run-cap bullets under
  "Things easy to get wrong"), `/concurrency` (`{global, groups}`: the
  global run cap's limit/default/overridden/active/waiting plus live
  per-group effective limit/declared/overridden/active/waiting, each with
  holders/waiting_runs drill-down), `/kv` (read-only state-store stats:
  per-namespace key count and bytes — shape unchanged, still value-free),
  `/kv/{namespace}` (one namespace's keys, sorted, `?prefix=` filters:
  name, size, and `expires_at` + remaining `ttl_seconds` when a TTL is
  set — the entry model tracks nothing else, so no created/updated
  stamps), and `/kv/{namespace}/{key}` (one entry **including its
  value**: `value_base64` always, `value_utf8` when the bytes are valid
  UTF-8; 404 on absent-or-expired via the same lazy-expiry rule as the
  state API). Exposing values on `/kv/{namespace}/{key}` is a
  **deliberate reversal** of the original "never values" stance, made at
  the operator's explicit request — the admin port is operator-only
  behind Zero Trust; the hook port and `/hooks/{id}` stay value-free
  (`/hooks/{id}`'s KV field remains the count/bytes summary). Plus the
  MANAGER surface: `GET /managers` (roster: state, effective disabled,
  instance id/started, restarts, inbox depth, last delivery/tick, config
  summary), `GET /managers/{id}` (roster row + the instance's recent
  output lines — managers are not runs, their logs live HERE, never in
  /runs), and the manager kill switch + bounce
  (`POST /managers/{id}/disable|enable|restart` — disable gracefully
  stops the instance and parks; restart bounces it). Internal,
  behind Cloudflare Zero Trust. The dashboard's `#hook={id}` fragment
  opens a per-hook "app" page built on those endpoints — an app is
  exactly one hook for now; grouping several hooks into one app is
  future work, which is why `/hooks/{id}` keeps a hook-scoped shape a
  grouping layer could aggregate. For `state: true` hooks that app page
  renders a "State (KV)" section: the key table (name, size, TTL
  remaining) with click-through to the stored value (pretty-printed when
  it parses as JSON, base64 for binary; text-node rendering, so stored
  bytes can't inject markup). The page is organized by a PERSISTENT
  SIDEBAR (dashboard.js's hash router): the bare `#` hash is the
  chart-first overview (timeline front and center), and every other
  section is its own `#page=<name>` route (hooks, managers, runs,
  events, kv, concurrency, images, attention, reload), with dynamic
  per-hook (`#hook=`) and per-manager (`#manager=`) drill-down links in
  the sidebar. Routing toggles a `.page-off` CLASS only — never the
  `hidden` attribute — so it composes with each section's own
  data-driven visibility (attention hides when healthy, the runs table
  behind timeline.js's toggle) and every section keeps refreshing over
  the same SSE section feed regardless of the active page (nothing is
  lost, only organized; the dedicated Managers page renders the roster
  with the hook-style slider kill switch, a Restart bounce, and the
  drill-down's live output tail). The overview's PRIMARY runs view is a
  realtime swimlane timeline (`<timeline-view>`, canvas, one lane per
  hook, hue per hook): queue wait as a dim lead-in segment, declared
  waits/blocked locks/queued group acquires hatched. Wait indication is
  ON-SPAN ONLY — the adapter deliberately feeds the component ZERO
  connectors (operator ruling: no cross-canvas lines; the generic
  connector capability stays upstream in js-snippets): a queued run's
  label badge carries the group and its live place in line ("⧗
  model-gateway · 3rd", re-stamped as the queue advances), a holder's
  badge carries how many runs it is holding up ("⏳N" — derived
  CLIENT-side by inverting waiting_on, because stream deltas never ship
  the server's waiters field), and holder/waiter click-through lives in
  the run modal's links. The adapter registers both badge glyphs as
  consumer rows in the component's "?" legend (`legendEntries`,
  feature-detected — an older Pages component just shows its built-in
  rows), and run tooltips spell them out in plain language from the same
  data ("waiting for <group> · Nth in line" / "holds the <group> slot ·
  N waiting"). One logical wait is ONE wait_history entry:
  internal/runs.SetWaitingOn CONTINUES the trailing open segment on a
  same-kind+key restamp (queue position/holder churn) instead of
  fragmenting it (pre-fix, a single 7-deep queue wait shipped 14
  micro-segments on every SSE delta). Failures
  emphasized; cancelled runs map to the component's first-class
  'cancelled' state (hollow + dashed category-hue border — "stopped, not
  failed"), with the kill tail (cancel_requested_at→finished) still a
  separate 'outline' segment the component draws as a terminal cut that
  never vanishes; instant runs (e.g. skips) as pips, fed INDIVIDUALLY at
  their true timestamps — the component clusters visually-overlapping
  instants into scale-aware ×N markers that split on zoom (the old
  adapter-side skip pre-merge is gone; each skip keeps its own tooltip
  and modal click-through); wheel/drag
  pan + zoom (plus `html { overscroll-behavior-x: none }` in
  dashboard.css so a trackpad back-swipe around the canvas never
  triggers history navigation), and panning into the past pages
  `/runs?before=` history
  down to retention (`/config`'s `run_retention` labels the boundary).
  `/config` also carries `stats_url` when `WEBHOOK_RUNNER_STATS_URL` is set:
  the simple-stats-api the browser polls for the title-bar host graphs
  (docs/internals/resource-graphs.md).
  COVERAGE'S TRAILING EDGE IS THE ADAPTER'S JOB: the component hatches
  every uncovered range up to now as unknown history, so on a live
  stream the adapter must keep vouching [last claim, now] — run deltas
  fold a `coverage` claim into their merge, hb/changed keepalives make a
  throttled coverage-only claim (`claimLiveCoverage`) — bounding the
  trailing hatch to ~one heartbeat; a dead feed stops claiming (growing
  hatch + stale note = the truth) and the reconnect snapshot back-fills
  the gap. Pre-#73 this held only by accident (the skip-driven
  rebuildAll re-registered coverage to now; deleting it hatched the
  whole live window over live bars — the 2026-07-15 incident); the
  testjs timeline-coverage harness pins the contract.
  RUN DELTAS ARE FRAME-COALESCED (the 2026-07-21 freeze fix): each SSE
  `run` delta updates `runsById` synchronously but defers the expensive
  component mergeData + the `whr:run-delta` fan-out to ONE
  `requestAnimationFrame` flush (`pendingDeltas` / `flushDeltas` in
  ts/timeline.ts), deduped by run id. A backlog buffered while the tab
  sat backgrounded for hours — a captured profile showed 6,335 deltas
  flushed in a single 15.3s main-thread block, zero repaints — used to
  run one full merge PER delta synchronously on the SSE handler; rAF is
  parked while backgrounded, so the whole backlog now collapses into a
  single deduped flush on foreground. `onDelta` became `onDeltas(batch)`;
  the batched apply does one collapse check + one waiter-index rebuild +
  one mergeData for the union of affected bars (skips are still fed
  individually, never pre-clustered). The testjs timeline-batch harness
  pins it (one merge per burst, deduped, zero synchronous chart work on
  the handler).
  THE CHART IS POSITIVELY RECOVERING (operator directive): it must
  always reflect what is happening RIGHT NOW, derived from the server's
  live snapshot of active state — never from replaying accumulated
  start/end events. Every hb carries the active run-id set and the
  adapter diffs runsById against it each beat (`reconcileActive` in
  ts/timeline.ts): local non-terminal runs ABSENT from the set drop
  (uncapped, no per-run probes — absence from truth IS the verdict; one
  rebuildAll per batch), unknown active ids truth-fetch once
  (`/runs?live=1` for several, `/runs/{id}` for one) — so a missed
  start/end costs at most ~one heartbeat of fiction. Feature-detected
  PER STREAM CONNECTION (`serverHasActiveSet`, reset on open, proven by
  the first payload-bearing hb): an old server's `data: {}` keeps the
  capped legacy `reconcileMissing` probes byte-for-byte. While the
  stream is DOWN, fallback-poll pages from a capable server are
  themselves active-complete, so `reconcilePage` applies the same
  uncapped diff — zombies die during outages too. Two supporting
  invariants: terminal-is-final (`staleRegression` — an out-of-band
  fetch racing a terminal delta can never regress a run to live; no
  later delta would fix it) and coverage floors (`coverageFloorMs` —
  full-feed claims reach down to the oldest TERMINAL row only, since
  pages now always carry every active run and an ancient live span must
  not vouch unfetched terminal history as known-empty). The testjs
  timeline-reconcile harness pins all of it.
  QUEUE COLLAPSE (operator directive — a flood must read as depth, not
  a wall of spans): waiting never drives packing. Per lane the adapter
  clusters the runs' QUEUED EXTENTS (accepted → launched; → the live
  edge for one still waiting) and any cluster holding 2+ becomes ONE
  synthetic `queued:<startMs>:<lane>` aggregate over the whole
  oversubscribed stretch — with the runs inside it losing their
  lead-ins: a run that launched is drawn from LAUNCH, one that never
  launched is withheld (it has nothing but wait to show). What is left
  overlapping is actual execution, so a lane packs to its concurrency
  limit: N rows of real spans plus one row saying how deep the queue
  got behind them. Before this, ~480 runs accepted at once each carried
  a lead-in over the same stretch and the lane stacked ~480 sub-tracks
  deep. A lane nobody queued behind is untouched — a lone waiter keeps
  its dim lead-in. The badge counts what is WAITING NOW while the
  cluster is open (it counts down as the queue drains; the tooltip adds
  the peak) and the PEAK once it is closed. The DATA model stays
  per-run — runsById and the truth reconcile never see the aggregation,
  it exists only in the fed interval set (`buildAllIntervals`). A
  change to the SET of clusters rebuilds via setData (mergeData cannot
  remove intervals); within a stable set, depth changes are aggregate
  upserts. Aggregate click opens the lane's hook page (a run modal
  cannot show N runs); its tooltip names the depth + the first few
  queued runs. The ':' in the aggregate id namespace can never appear
  in a run id (26-char base32), so ids never collide. The testjs
  timeline-collapse harness pins it.
  A bar click opens the run modal, a lane-label click opens `#hook={id}`,
  and the old runs table stays behind a persisted "Show table" toggle.
  THE RUN MODAL POLLS `/runs/{id}` EVERY 3s WHILE IT IS OPEN ON A
  NON-TERMINAL RUN — deliberately, stream or no stream, and the one
  place the zero-polling-while-live rule does not apply. Deltas cannot
  carry OUTPUT (`runs.Run.AppendOutput` fires no OnChange: deltas are
  output-stripped and per-line fan-out would hit every client), so a run
  that is merely logging emits no deltas and the old
  `whrStreamLive === true` stand-down froze the open modal for the whole
  run. Deltas still refresh it instantly for the state changes they DO
  carry, and any refresh restarts the 3s clock (`lastRunDetailFetch`),
  so a busy run still costs at most one fetch per interval; it stops at
  a terminal render and on close. The delta fan-out itself must survive
  a missing chart: `flushDeltas` drains and dispatches even when the
  runtime-imported component never attached (pre-fix it returned early,
  killing the modal's and table's feed and growing `pendingDeltas`
  unbounded) — the testjs rundetail-live and timeline-batch harnesses
  pin both halves.
  waiting_on/waiters and unknown statuses are feature-detected, so the
  timeline works against servers with or without first-class waits.
  GITHUB SLUGS ARE CLICKABLE WHEREVER THE DASHBOARD RENDERS TEXT
  (`linkifyGH` in dashboard.js): run titles, raw + conversation log
  output, the activity feed, needs-attention messages, wait reasons, hook
  descriptions, image build errors, manager output/title/last-error, KV
  values and reload-panel commit subjects. `owner/repo#41` links to
  `https://github.com/owner/repo/issues/41` — the ISSUES form on purpose,
  since GitHub redirects it to `/pull/41` when the number is a PR — and
  opens in a new tab. A BARE `owner/repo` links only in title-ish fields
  (`linkifyTitle`: run titles, the manager drill-down's Title), never in
  free-form text, where `true/false` and `hooks/gha-runner` are
  indistinguishable from a repo slug; a slug that is part of a longer
  path, word or URL (`src/hooks/pr-minder`, `.../pull/41`) never links,
  and neither does a digits-only pair (`24/7`). Rendering stays TEXT
  NODES plus `<a>` elements — never innerHTML — so no payload can inject
  markup, and each link stops click propagation so a slug inside a run
  row opens GitHub WITHOUT also opening the run modal. The manager
  roster's name cell is deliberately NOT linkified (it is already the
  link to the manager page). The testjs linkify harness pins the
  contract, false positives included.
  Dashboard assets are content-addressed (`internal/server/
  dashboard` rewrites index.html to `/dashboard.<hash>.css|.js` +
  `/timeline.<hash>.js`, served
  immutable; `/` and the bare asset paths are no-cache, stale hashes 404)
  so an edge cache can never pair new HTML with stale assets.
