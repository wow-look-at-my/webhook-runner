# Gotchas: the state-socket proxy shim and the dashboard timeline

Why state hooks reach the KV API at a plain localhost URL through an injected shim, and how the timeline splits between the runtime-imported component and this repo's adapter.

Moved VERBATIM out of `CLAUDE.md` when that file went over the
40,000-character instruction-file budget. Nothing here was condensed.

- State hooks reach the KV API at a plain `http://localhost:9002` URL, NOT over
  networking — Docker has no native TCP→unix-socket forward, so webhook-runner
  runs the proxy itself. The KV server listens on a Unix socket at
  `$TMPDIR/whr-state.sock` (chmod 0666 so non-root hook users can connect; the
  bearer token, not file perms, is the real gate). For a state hook the runner
  sets the container entrypoint to webhook-runner's own binary (copied to
  `$TMPDIR/whr-shim` at startup by `copyExecutable`, bind-mounted in) invoked as
  the hidden `kv-forward` subcommand; that shim (`internal/kvproxy`) proxies
  `localhost:9002` → the bind-mounted socket, then execs the hook's real command
  (reconstructed from the image's entrypoint+cmd via `imageCommand`, or
  `hook.Command`). Both the socket and the shim MUST sit in the host-shared
  `TMPDIR` (same requirement as payload mounts) — so it works identically
  whether the server runs on the host or in a container. This was chosen after
  host-gateway and a shared Docker network proved more fragile; the shim keeps
  the hook's own networking intact (no netns sharing) and publishes no port.
  `WEBHOOK_RUNNER_STATE_SOCKET` overrides the socket path (must stay host-shared).
- The dashboard timeline splits in two: the **`<timeline-view>` component
  is consumed at RUNTIME from js-snippets' buildhost library site** — the
  browser imports
  `https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js`
  (live at master head — republished on every js-snippets master push;
  replaced the quota-dead GitHub Pages deploy 2026-07-20; the org's
  standard js-snippets consumption model, NEVER vendored copies) — while
  this repo ships only the runner-specific adapter. Component fixes deploy to this dashboard on js-snippets merge
  with no runner change; fix component bugs upstream in js-snippets, full
  stop. Consequences to keep straight: `assets/timeline.js` is a small
  ES-module adapter bundle whose component import passes through UNBUNDLED
  (ts0.json: esbuild `format: "esm"` + `external: ["https://*"]`) and is
  loaded via `<script type="module">` (after dashboard.js — modules defer,
  so its globals are always ready); the admin dashboard's chart therefore
  needs reach to sites.pazer.build at page load. A failed component
  fetch degrades softly and NEVER parks: the adapter module still runs,
  shows a "chart loading…" note in the Runs section, and retries the
  dynamic import on a FIXED 5s cadence forever (cache-busted `?retry=N`,
  because browsers can memoize a failed module fetch; no backoff, no
  attempt cap — see boot() in ts/timeline.ts), while dashboard.js's tables
  are untouched and the runs-table toggle keeps working. TypeScript types
  for the URL import come from `ts/js-snippets-timeline.d.ts`, an INTERIM
  hand-maintained ambient shim (types only) — temporary until the generate
  step fetches js-snippets' published declarations mechanically (the
  library site already serves a .d.ts next to every .js; do not grow the
  shim beyond what the adapter consumes).
  The adapter is compiled by ts0 into the COMMITTED `assets/timeline.js`
  (go:embed needs it on a fresh clone; the bundle carries a DO-NOT-EDIT
  banner — never hand-edit it, edit ts/ and regenerate). Regeneration is
  **temporarily manual**: the npx `//go:generate` directive (and with it
  ci.yml's `generate:` approval hash, setup-node, ts0 git-auth, and the
  assets freshness gate) was removed so the build uses the committed
  bundle as-is with NO node/npm/npx anywhere; run ts0 yourself after
  editing ts/ and commit the regenerated bundle. A prebuilt ts0 binary
  served from buildhost, fetched by a small Go bootstrap, is landing next
  to re-automate regeneration.
