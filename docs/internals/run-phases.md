# Run phase marks: measuring what a container costs

Why the runner stamps lifecycle instants, what each one means, and the one
rule for reading them.

## Why this exists

The cost of running a hook in a disposable container was argued from first
principles and never measured. Estimates circulated ("a couple hundred
milliseconds", "Node cold start dwarfs it") with nothing behind them, and
proposals to change the execution model — a second sandbox backend, moving
hot hooks to managers — were being weighed against numbers nobody had.

A run now carries marks. Container overhead is a figure on the dashboard,
per run and aggregated per hook, and any future argument about it starts
from data.

## The marks

Stamped in `internal/runs` (`Phase`, `Run.Mark`), first write wins, and
persisted with the terminal snapshot like every other `RunState` field.

| Mark | Stamped when | By |
|---|---|---|
| `image_ready` | `EnsureImage` returned (cache hit or real build) | runner |
| `slot_acquired` | group + global concurrency slots both held | runner |
| `inspected` | the argv-reconstruction `docker inspect` returned | runner (state hooks only) |
| `spawned` | `exec.Command.Start` returned for `docker run` | runner |
| `container_entry` | the first instruction ran INSIDE the container | the injected shim |
| `first_output` | the run's first stdout/stderr line reached the server | runner |
| `exited` | `cmd.Wait` returned — container gone, `--rm` done | runner |

The spans that matter:

- **boot** = `spawned` → `container_entry`. Docker's own
  create/namespace/overlay/entrypoint cost, with no hook runtime in it.
- **runtime start** = `container_entry` → `first_output`. The interpreter
  warming up, imports, whatever the hook does before it prints.
- **argv inspect** = `slot_acquired` → `inspected`. The extra CLI + daemon
  round trip state hooks pay before their container starts.
- **reap** = `exited` → `Finished`. The runner's own bookkeeping tail.

## The one rule: never quote a bound as a measurement

`container_entry` is reported by the shim, and the shim is injected only
into `state: true` hooks. Every other hook has no in-container vantage
point, so the only available figure is `spawned` → `first_output`, which
**contains the runtime cold start**.

`RunState.BootDuration` returns `(duration, exact, ok)` and the `exact`
flag is not decoration: it is the difference between "Docker costs this"
and "Docker plus Node cost this, together, and we cannot say in what
proportion". Conflating them is the specific error that made the original
question unanswerable, so:

- the aggregate keeps them in separate buckets (`boot_*` vs `bound_*` in
  `OverheadStats`) and never averages one into the other;
- the dashboard prints the bounded figure with a leading `≤` and the words
  "docker + runtime";
- the timeline draws an exact split as two stippled segments and a bounded
  one as a single hatched segment — the same "unknown contents" texture the
  component uses for uncovered time.

A missing mark means UNKNOWN, never zero. Pre-upgrade history carries no
marks at all, and `computeOverhead` returns nil rather than reporting a
window of zero-cost boots.

## The in-container mark's path

The shim (`webhook-runner kv-forward`, the entrypoint of a state hook's
container) POSTs `/phase/container-entry` on the state socket before it
starts the proxy or execs the hook. The server stamps its own receive
time rather than trusting a body timestamp: the socket is local and
sub-millisecond, and a client-supplied instant would be unverifiable.

Two consequences to keep straight:

- The mark **includes the shim binary's own process start**. That is
  honest — the shim IS the container's entrypoint, so its start is part of
  what launching this container costs — but it means the figure is
  "container startup as this runner configures it", not a pure `runc`
  benchmark.
- The report is **best-effort by contract**. No socket, no token, a server
  mid-restart, a manager instance with no run to stamp: every one answers
  204 or is swallowed client-side, and the shim never retries. One missing
  data point is always cheaper than a hook delayed by its own telemetry.
  Instrumentation must never be able to fail a run.

## What is deliberately NOT here

- **No per-mark SSE frame.** Marks land in bursts around launch and ride
  the deltas the surrounding lifecycle transitions already emit
  (`SetRunning`, `Finish`). A frame per mark would multiply stream traffic
  to say nothing new.
- **No `POST /phase/{name}`.** The route is a single fixed path. If hooks
  could stamp arbitrary marks the measurement would stop meaning what it
  says.
- **No percentiles.** Averages and maxima over the existing stats window,
  matching the duration/wait figures beside them. A p95 needs retained
  samples, which is a different (and much larger) change.
