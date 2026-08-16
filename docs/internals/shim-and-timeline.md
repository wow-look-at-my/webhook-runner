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
  (live at master head — republished on every js-snippets master push; the
  org's standard js-snippets consumption model, NEVER vendored copies) —
  while this repo ships only the runner-specific adapter. Component fixes
  deploy to this dashboard on js-snippets merge with no runner change; fix
  component bugs upstream in js-snippets, full stop. Consequences to keep
  straight: `assets/timeline.js` is a small ES-module adapter bundle whose
  component import passes through UNBUNDLED (ts0.json: esbuild
  `format: "esm"` + `external: ["https://*"]`) and is loaded via
  `<script type="module">` (after dashboard.js — modules defer, so its
  globals are always ready); the admin dashboard's chart therefore needs
  reach to sites.pazer.build at page load. A failed component fetch
  degrades softly and NEVER parks: the adapter module still runs, shows a
  "chart loading…" note in the Runs section, and retries the dynamic
  import on a FIXED 5s cadence forever (cache-busted `?retry=N`, because
  browsers can memoize a failed module fetch; no backoff, no attempt cap —
  see boot() in ts/timeline.ts), while dashboard.js's tables are untouched
  and the runs-table toggle keeps working.
  TypeScript types for the runtime import come from the committed
  `ts/js-snippets/timeline-view.d.ts` pair, NOT from the live URL: a
  type-only import is erased at build time, so it resolves against a real
  local file rather than needing TypeScript to fetch an `https://`
  specifier (it never will). `ts/timeline.ts` type-imports
  `./js-snippets/timeline-view.d.ts` directly; the separate runtime
  `import()` in `loadComponentForever()` still targets `COMPONENT_URL`,
  the live buildhost URL — the two specifiers are deliberately different,
  and that is fine, because the type import is gone by the time any code
  runs. Regeneration is `dashboard.go`'s
  `//go:generate sh generate-timeline.sh` (this package's directory,
  cwd = the package dir): curl a PINNED ts0 build from buildhost (`?v=N`,
  never `branch=latest`) into `.cache/` (gitignored), curl the two
  `.d.ts` siblings from the library site into the committed
  `ts/js-snippets/`, then `node ts0.cjs build`. Needs curl and Node 22+ —
  deliberately no npm/npx and no git auth. The bundle and the fetched
  `.d.ts` pair are committed (go:embed needs the bundle on a fresh clone,
  and it carries a DO-NOT-EDIT banner — never hand-edit it, edit `ts/`
  and regenerate); ci.yml's `generate:` input carries the approval hash
  (a bare `go-toolchain` run prints the new one after any directive-line
  edit), and the freshness gate
  (`git diff --exit-code -- internal/server/dashboard/assets/
  internal/server/dashboard/ts/js-snippets/`) fails CI on any drift, so a
  stale bundle, stale fetched types, or upstream component API change all
  turn CI red instead of shipping silently stale.
