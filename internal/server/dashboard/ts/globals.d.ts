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
