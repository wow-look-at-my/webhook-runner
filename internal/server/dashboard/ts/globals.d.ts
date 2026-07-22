// Globals provided by dashboard.js — both bundles are classic scripts on
// the same page and timeline.js loads AFTER dashboard.js (script order in
// index.html), so these are present at runtime. Declared here instead of
// re-implemented so the formatting/zero-time/duration logic has exactly one
// source of truth. Keep the signatures in sync with dashboard.js.

/** GET url, Accept: application/json; throws on any non-2xx status. */
declare function fetchJSON<T = unknown>(url: string): Promise<T>;

/** createElement + attributes (class/data/other) + text-node children. */
declare function el(
	tag: string,
	attrs?: Record<string, unknown> | null,
	...children: Array<string | Node | null | undefined>
): HTMLElement;

/** Locale date+time for an RFC3339 stamp ('' for empty input). */
declare function fmtTime(s: string | undefined): string;

/** '### ms' / '12.3s' / '4m 5s' / '2h 3m' ('' for negative/NaN). */
declare function fmtDuration(ms: number): string;

/** Is a Go time actually set? (zero time marshals as 0001-01-01T00:00:00Z.) */
declare function tsPresent(s: string | undefined): boolean;

/** Queue wait: started → started_at, ticking live for pending runs. */
declare function runWaited(r: unknown): string;

/** Processing time: started_at → finished, ticking live while running. */
declare function runDuration(r: unknown): string;

/** Opens the run-detail modal for a run id (the tables' click target). */
declare function showRun(id: string): Promise<void>;

/** The #hook=<id> fragment's hook id, or null on the overview. */
declare function currentHookId(): string | null;

// -- Contracts this module PROVIDES to dashboard.js ---------------------------
//
// timeline.ts owns the /runs/stream EventSource and publishes its state so
// the classic script can gate its own fallbacks (e.g. the run modal's fixed
// 3s poll and the section tables' fixed 5s poll run only while the stream
// is down):
//   window.whrStreamLive          — true while the stream is open
//   'whr:stream-state' (window)   — CustomEvent {detail: {live}} on change
//   'whr:run-delta' (window)      — CustomEvent {detail: {id, run}} per run
//                                    lifecycle delta from the stream (coalesced
//                                    to one flush per animation frame, deduped
//                                    by id — a backlog fires as a single batch)
//   'whr:sections-changed' (window) — CustomEvent {detail: {sections}}: the
//                                    server's coarse "these admin sections
//                                    changed, refetch once" signal
//   'whr:runs-table-shown' (window) — the runs table just became visible
//                                    (it starts stale; refill it)
//
// -- Contracts dashboard.js PROVIDES to this module ----------------------------
//
// dashboard.js owns the single /hooks fetch and republishes the payload
// (this module never fetches /hooks — see applyHooksData):
//   window.whrHooks               — the latest GET /hooks payload
//   'whr:hooks-data' (window)     — CustomEvent {detail: {hooks}} per fetch
declare interface Window {
	whrStreamLive?: boolean;
	whrHooks?: Array<{ id: string; description?: string; schedule?: string; disabled?: boolean }>;
}
