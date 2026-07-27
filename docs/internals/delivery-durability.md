# Deploy windows: how a delivery survives a restart

> A webhook that arrives while webhook-runner is restarting used to be
> **answered with an error AND lost**. GitHub does not re-send a failed
> delivery, so a 503 during a deploy was permanent. Three mechanisms now cover
> the window: `/restart-ready` keeps the stop from being issued while runs are
> in flight, the **delivery spool** parks anything that arrives once the drain
> has begun and answers 202, and the **shutdown ordering** keeps the hook
> listener and the state socket up across the drain instead of tearing them
> down in front of it.

## The premise that was false

`handleTrigger` answered a draining server's deliveries with 503 under this
comment:

```go
// A draining server is a RETRYABLE condition, not a hook failure:
// answer 503 so the sender (GitHub redelivers webhooks) tries the
// restarted server instead of recording a permanent failure.
```

GitHub does not redeliver. The hooks repo's own delivery-gap replay SDK
(`src/sdk/deliveries.ts`) exists precisely because deliveries are
"consumed-and-lost during webhook-runner downtime", and it applies **no status
filter** when replaying — in its own words, "a delivery that got a 202 before
the runner lost the run reads as success on GitHub's side and still needs
replaying". Every 503 in a deploy window was therefore an errored response and
a dropped webhook, and only a hook that had wired up the replay SDK ever got it
back.

The drain gate itself is right and stays: a run LAUNCHED by a dying process
races the state-socket handover and dies on its first lock call
(`internal/runner/drain.go`). The fix is not to run it anyway — it is to keep
the delivery.

## 1. `/restart-ready` — don't stop at all while runs are live

`internal/server/restartready.go`, on the admin port. 200 when no runs are in
flight, 503 when any are, so the docker-updater label

```
docker-updater.pre-check.url=:9001/restart-ready
```

makes an update skip that cycle and retry the next one. This is the outermost
layer: the best deploy window is the one that happens when nothing is running.
`WEBHOOK_RUNNER_RESTART_MAX_DEFER` (default 6h of *continuous* blocking) forces
a 200 anyway so a permanently busy fleet cannot pin the binary forever. See
[admin-api.md](admin-api.md) for the full contract.

## 2. The spool — park what arrives anyway

`internal/spool` plus `internal/server/spooldelivery.go`.

Once `BeginShutdown` has been called, `handleTrigger` checks
`s.runner.Draining()` **before** `runner.Start` and writes the delivery to
`<data-dir>/spool/` instead, answering:

```json
202 {"spooled":"<id>","status":"spooled","hook":"pr-minder",
     "detail":"server is restarting; this delivery is parked and will run on the next start",
     "received":"..."}
```

Load-bearing details:

- **Checked before `Start`, not after its error.** A parked delivery leaves no
  errored run record — it did not fail, it is waiting. Routing it through
  `Start`'s `ErrDraining` path would have written a red `error` run to history
  for every successfully-parked webhook.
- **After auth and `skip_if`.** The spool never holds an unauthenticated body,
  and a delivery the hook would have skipped is never parked.
- **Headers are preserved verbatim** (`X-GitHub-Event` and friends), so the
  replayed run is indistinguishable from the live delivery.
- **Atomic temp+rename** per entry, so a crash mid-write cannot leave a
  half-entry for the next process; only committed `*.json` files are replayed.
- **Filenames sort by receipt instant** (`<unix-nanos>-<id>.json`), so a plain
  directory listing is already oldest-first and arrival order is preserved.
- **Bounded** — `DefaultMaxEntries` 1000, `DefaultMaxBytes` 64 MiB. A spool is
  a deploy-window buffer, not a queue. Past the bound `Put` returns `ErrFull`
  and the handler falls through to the honest 503 rather than a 202 that lies.
- **Loud either way**: `spool.parked` on success, `spool.failed` when it could
  not be parked. A dropped delivery is invisible on GitHub's side, so the
  activity feed is the only place the loss can surface.

### Replay

`internal/cli/spoolreplay.go`, wired into `runServe` behind a `sync.Once` that
fires on the FIRST load that populates the registry — event-driven off the
watcher's initial scan, not a readiness poll, and necessarily after it because
a replay needs its hook to exist.

Each entry becomes an ordinary run via `rn.Start` with the original body,
headers and title. The `Replay` contract is what makes it safe:

| Outcome | What happens to the entry |
|---|---|
| dispatched | deleted |
| dispatch failed | **kept** for the next boot — never dropped, that is the loss this exists to prevent |
| unparseable | renamed aside to `*.corrupt` — inspectable, and the queue keeps moving |
| hook no longer in the tree | dropped LOUDLY (`spool.dropped`) — it can never succeed, and keeping it would wedge every later replay |

## 3. Shutdown ordering — keep the door open while draining

The largest hole was not the 503; it was that the process spent the entire
drain **alive with nothing listening**. `hookSrv.Shutdown()` ran before
`rn.Wait()`, and `rn.Wait()` is unbounded — as long as the longest in-flight
run, which for a CI job is minutes. Every delivery arriving in that stretch got
a connection refusal, which is a silent loss.

`runServe`'s teardown now reads:

```go
rn.BeginShutdown()   // refuse new runs; deliveries spool from here on
sup.Shutdown()       // managers stop first — the lease releases with the process
srv.CloseStreams()   // SSE clients, so adminSrv.Shutdown can complete
adminSrv.Shutdown()  // no dependents
rn.Wait()            // drain in-flight runs — minutes, with the door still open
hookSrv.Shutdown()   // only now
stateSrv.Shutdown()
```

Two reasons the order is what it is:

- **The hook port stays up** so arriving deliveries get spooled and 202'd
  rather than connection-refused for the whole drain.
- **The state socket stays up** because DRAINING RUNS ARE STILL USING IT —
  locks, `/wait`, `/title` all ride it. Closing it before `rn.Wait` pulled the
  floor out from under the very runs being drained.

The hook/state grace context is also created **after** `rn.Wait()`. Armed
before it, a 10s deadline would already be blown by the time it was used,
turning a graceful close into an abrupt one.

## What this does NOT cover

The **port-down swap window** — old container gone, new one not yet listening.
Nothing in-process can fix that: a delivery arriving there never reaches any
webhook-runner. Closing it needs either a front-door buffer (something that
accepts on :9000 across the swap) or making the hooks repo's `deliveries.ts`
replay universal instead of per-hook. `/restart-ready` shrinks the window's
blast radius by choosing when it happens; it does not remove it.

## Source

- `internal/spool/spool.go` — the store (`Put`, `Replay`, bounds, quarantine)
- `internal/server/spooldelivery.go` — the park + events
- `internal/server/handlers.go` — the `Draining()` check in `handleTrigger`
- `internal/cli/spoolreplay.go` — replay on the first load
- `internal/cli/serve.go` — spool wiring and the shutdown ordering
- `internal/runner/drain.go` — why new runs are refused in the first place
- `internal/server/restartready.go` — the docker-updater pre-check
