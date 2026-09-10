# Resource graphs: the host in the title bar

The dashboard shows cpu, ram, disk, net and gpu for the host in the title bar. Per-container graphs live on docker-updater's dashboard, which owns the container list. The runner measures NONE of it. Every number comes from a [simple-stats-api](https://github.com/wow-look-at-my/simple-stats-api) instance named by `WEBHOOK_RUNNER_STATS_URL`, which `GET /config` exposes as `stats_url`. That API reads procfs, the Docker daemon socket and NVML on the host. It keeps its own history window and serves everything with CORS open. The browser fetches it directly. The runner is not a relay.

`ts/stats.ts` owns the page side. It loads `<perf-graph>` from js-snippets' library site (the same never-give-up loader as the other components). It then polls `stats.json` on the API's own `sampling.intervalSeconds`. The first fetch asks for the full history and replays it into the graphs oldest-first. Every later fetch uses `?history=0`. A poll then carries the current values and nothing the page already holds.

## Gauges

| gauge | source metric |
|---|---|
| cpu  | `cpu/percent` as a share of its possible range (all cores = 100) |
| ram  | `ram/percent` |
| disk | `disk/percent` (filesystem use) |
| net  | `network/rx_rate` + `tx_rate` in MB/s |
| gpu  | `gpu/0/utilization` |

## What the page says when a number is missing

A graph is believed at a glance. An empty one must explain itself.

- No `stats_url`: the title bar carries a badge naming the variable. Nothing draws a flat zero.
- The API is unreachable: every strip dims and a red badge reads "stats unreachable". The poll keeps its fixed cadence and never gives up. The next success re-seeds the history.
- A metric the API does not report (no GPU, no Linux disk stats): that gauge stays empty with a tooltip naming the gap.

