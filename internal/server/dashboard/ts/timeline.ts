/*
 * timeline.ts — the dashboard's primary runs view: a realtime swimlane
 * timeline (one lane per hook) rendered by the generic <timeline-view>
 * canvas element from wow-look-at-my/js-snippets.
 *
 * This file is the webhook-runner adapter: it knows the /runs, /hooks,
 * /config and /runs/stream shapes and the runner's semantics (queued vs
 * started_at vs finished, terminal statuses, waiting_on/waiters) and
 * translates them into the component's generic lanes/intervals model
 * (deliberately NO connectors — wait indication lives on the spans; see
 * runLabel). The component itself is NOT part of this repo: the browser imports
 * it at runtime from js-snippets' GitHub Pages (live at master head — the
 * org's standard js-snippets consumption model), so component fixes reach
 * this dashboard on js-snippets merge with no runner change. Fix component
 * bugs upstream in js-snippets; only adapter logic lives here. The import's
 * types come from js-snippets-timeline.d.ts (an INTERIM hand-maintained
 * shim — see its header).
 *
 * Built by ts0 (see ../ts0.json) into assets/timeline.js — an ES module
 * (the component URL passes through unbundled) loaded via
 * <script type="module"> AFTER dashboard.js (module scripts defer; the
 * classic dashboard.js has long executed) and reusing its globals
 * (fetchJSON, el, fmtTime, tsPresent, showRun, … — declared in
 * globals.d.ts). If the Pages fetch fails, the chart section shows
 * "chart loading…" and the load retries on a fixed cadence forever (see
 * boot() at the bottom); dashboard.js's tables are never affected either
 * way.
 *
 * -- The live feed: SSE primary, fixed-cadence poll fallback ---------------
 *
 * Run data arrives over ONE EventSource on /runs/stream: a connect
 * `snapshot` (the live+recent window), then one `run` delta per lifecycle
 * change, plus `hb` heartbeats (~10s). An idle dashboard makes ZERO /runs
 * requests; updates land at push latency. The old 2s /runs poll survives
 * only as the FALLBACK: while the stream is down beyond a short grace
 * period, a fixed 5s poll (never growing, never giving up) keeps the chart
 * honest until the stream reconnects, and every stream (re)open does ONE
 * full /runs resync then goes stream-only again.
 *
 * The SAME connection multiplexes `changed` events — coarse "these admin
 * sections changed, refetch each once" signals (hooks/images/concurrency/
 * kv/events) — which this module re-publishes as whr:sections-changed for
 * dashboard.js, whose section tables follow the exact same push-first/
 * fixed-fallback pattern. One stream feeds the whole dashboard; an idle
 * page makes zero requests of any kind. The /hooks lane roster likewise
 * arrives via whr:hooks-data from dashboard.js instead of a poll here.
 *
 * WHY the poll was demoted — the stuck-running-bars post-mortem. The 2s
 * poll design had three independent ways to show fiction:
 *
 *  1. THE WEDGE (the probable killer): pollRuns used a single-flight guard
 *     (`if (pollInFlight) return`) around a fetch with NO timeout. One
 *     fetch that never settles — a network change mid-request, a proxy
 *     holding a half-open connection — left pollInFlight=true FOREVER.
 *     Every later tick returned at the guard, nothing was ever logged
 *     (nothing rejected), and the canvas kept extrapolating "running" bars
 *     to the live edge indefinitely. Silent, permanent, invisible.
 *  2. THE GATE: polling was visibility-gated (`!document.hidden` AND the
 *     overview being the active view) while the canvas kept rendering —
 *     a hidden tab or a #hook= drill-down froze the DATA but not the
 *     DRAWING, so ongoing bars grew fictionally until a visibilitychange
 *     tick happened to land (and if that tick wedged per 1, forever).
 *  3. THE MERGE HOLE: each poll merged only the newest max=400 window. A
 *     run whose terminal transition happened while it was outside that
 *     window (busy server + a long gated stretch) kept its stale
 *     "running" status in runsById with nothing ever retiring it.
 *
 * The new feed is immune by construction: the stream is not visibility
 * gated and pushes every terminal transition; the fallback poll loop is
 * structurally unkillable (one setInterval armed once and never cleared,
 * body wrapped in try/catch, fetches bounded by AbortSignal.timeout so the
 * in-flight guard cannot wedge); markFresh is called ONLY on real data or
 * heartbeats, so the component's staleness marking (feature-detected)
 * shows honest "stale" state instead of fiction whenever the feed truly
 * dies; and each resync retires stale non-terminal runs that fell out of
 * the window (reconcileMissing). EventSource reconnects itself on error at
 * the server-directed fixed 2s retry — we never layer our own backoff on
 * top; the supervisor only replaces a source the browser has permanently
 * CLOSED (e.g. the endpoint 404ing on an older server), again on a fixed
 * cadence.
 *
 * Runner semantics encoded here:
 *   - `started` is the QUEUED/accepted instant; `started_at` (absent while
 *     pending) is the container launch; queue wait renders as a dim leading
 *     'queued' segment so a run stuck behind a concurrency group never
 *     reads as a slow run.
 *   - `finished` is ALWAYS serialized — a Go zero time ("0001-01-01…") on
 *     live runs — so presence goes through tsPresent, never truthiness.
 *   - `waiting_on` / `waiters` (first-class waits), the per-run friendly
 *     `title`, and any new status values are FEATURE-DETECTED: absent
 *     fields degrade to the plain rendering (short run id as the label),
 *     unknown statuses render dim instead of crashing.
 *   - Skipped runs are born terminal with zero duration (started ==
 *     finished): each feeds through as its OWN instant interval at its
 *     true timestamp, with its real label/tooltip/modal link. The
 *     COMPONENT clusters visually-overlapping instant markers into ×N
 *     point markers (scale-aware — splitting apart as you zoom in), so a
 *     redelivery burst can't blow up lane height and the adapter does no
 *     pre-merging.
 *   - Lanes are ordered ALPHABETICALLY by hook id — stable and
 *     viewport-independent. (Most-recent-activity ordering made rows jump
 *     around as runs entered/left the window; if activity ordering is
 *     ever wanted it becomes an explicit user toggle, not the default.)
 *   - Dragging into the past pages GET /runs?before=<cursor> — the cursor
 *     is the oldest run's RAW `started` string (nanosecond precision;
 *     never round-tripped through Date). The walk is BOUNDED by the
 *     requested window (stop as soon as a page's oldest predates the
 *     range floor), single-flight (a concurrent second walk throws), and
 *     armed only after the first data seeds coverage — so a page load
 *     fetches the visible window only, never an exhaustive history walk.
 *     History paging stays request/response BY DESIGN (it is user-driven
 *     and bounded); only the live tail is push.
 *   - COVERAGE'S TRAILING EDGE IS THIS ADAPTER'S JOB: the component hatches
 *     every uncovered range up to now as unknown history, and only the
 *     consumer can vouch that a live stream means "quiet == known-empty".
 *     Every feed sign of life therefore extends the claim — deltas fold
 *     `coverage` into their merge, hb/changed make a throttled coverage-only
 *     claim (claimLiveCoverage) — so the hatch can trail the now-marker by
 *     at most ~one heartbeat. A dead feed stops claiming, and the growing
 *     hatch + stale note honestly mark the unknown until the reconnect
 *     snapshot back-fills it. (Pre-#73 this held only by accident, via the
 *     skip-driven rebuildAll's coverage reset — deleting it produced the
 *     2026-07-15 full-window-crosshatch-over-live-bars incident.)
 */

// Types only — erased at compile time. The component itself is loaded at
// RUNTIME by loadComponentForever() below (a dynamic import of the same URL,
// kept verbatim in the built bundle via esbuild `external`); the browser
// fetches it (and its sibling chunk imports) from GitHub Pages. Deliberately
// NOT a static side-effect import: a static import that fails would kill
// this whole module, and the load must retry forever instead.
import type {
	TimelineData,
	TimelineHit,
	TimelineInterval,
	TimelineLane,
	TimelineSegment,
	TimelineViewElement,
} from 'https://wow-look-at-my.github.io/js-snippets/ui/timeline-view.js';

// -- Runner API shapes (the fields this adapter consumes) --------------------

/** GET /runs list entry (internal/runs.RunState, output stripped). */
interface RunState {
	id: string;
	hook_id: string;
	/** Feature-detected: friendly display title (e.g. "owner/repo#47"). */
	title?: string;
	/** Queued/accepted instant (RFC3339Nano). Always present. */
	started: string;
	/** Container-launch instant; genuinely absent while pending. */
	started_at?: string;
	/** Terminal instant; zero time ("0001-01-01…") until the run ends. */
	finished?: string;
	status: string;
	exit_code: number;
	error?: string;
	cancel_requested?: boolean;
	/** Feature-detected: when the first cancel request arrived. */
	cancel_requested_at?: string;
	/** Feature-detected: historical wait segments (closed at their REAL
	 * end times by the server — never derived client-side). */
	wait_history?: Array<{ kind?: string; key?: string; start: string; end?: string }>;
	wait_history_truncated?: boolean;
	/** Feature-detected (first-class waits): why a live run is paused. */
	waiting_on?: {
		kind?: string; // "wait" | "lock" | "group"
		reason?: string;
		until?: string;
		key?: string;
		holder_run_id?: string;
		holder_hook_id?: string;
		holder_run_ids?: string[];
		position?: number;
	};
	/** Feature-detected: live runs whose acquire this run is blocking. */
	waiters?: Array<{ run_id: string; hook_id: string; key?: string }>;
}

/** GET /hooks list entry (hook summary + operator kill switch). */
interface HookSummary {
	id: string;
	description?: string;
	schedule?: string;
	disabled?: boolean;
}

const COMPONENT_URL = 'https://wow-look-at-my.github.io/js-snippets/ui/timeline-view.js';
const COMPONENT_RETRY_MS = 5000; // FIXED retry cadence — never grows, never gives up
const STREAM_PATH = '/runs/stream';
// One supervisor/fallback tick: FIXED cadence, forever. Handles both the
// fallback poll (stream down) and replacing a permanently-CLOSED source.
const FALLBACK_POLL_MS = 5000;
// How long the stream may be down before the fallback poll engages. The
// server-directed EventSource retry is 2s, so a blip reconnects well within
// the grace period and never wakes the poll at all.
const STREAM_GRACE_MS = 8000;
// Every feed fetch is bounded: a fetch that cannot settle must fail, not
// wedge an in-flight guard (root cause #1 above).
const FETCH_TIMEOUT_MS = 15000;
const RESYNC_MAX = 400; // /runs window fetched at boot, on stream open, by the fallback poll
const RECONCILE_MAX = 20; // stale non-terminal runs re-checked per resync
// Component staleness marking (feature-detected): with 10s heartbeats,
// ~2.5 missed beats = the feed is genuinely dead, say so on the chart.
const STALE_AFTER_MS = 25_000;
// Minimum step for stream-vouched trailing-coverage claims (see
// claimLiveCoverage): deltas fold theirs into the merge they already do,
// so this only throttles the standalone hb/changed extensions — a signal
// burst coalesces into at most one extra mergeData per second.
const LIVE_COVERAGE_MIN_STEP_MS = 1_000;
// Page size for ?before= history paging. Deliberately NOT raised: with the
// walk bounded to the requested window a pan needs 1-2 pages, and at
// observed production density a 200-run page is already ~3.5MB — a larger
// page would slow each pan-driven request for no real round-trip savings.
const BACKFILL_MAX = 200;
const BACKFILL_MAX_PAGES = 30; // per loadRange call; a reject resumes deeper
const PRUNE_AT = 10_000; // runs held in memory before pruning kicks in
const PRUNE_TO = 8_000; // newest runs kept when it does
const TABLE_PREF_KEY = 'whr-show-runs-table';

/** Statuses that mean the run is over (mirror internal/runs.Status). */
const TERMINAL_STATUSES = new Set(['success', 'failure', 'timeout', 'error', 'cancelled', 'skipped']);

function isTerminal(status: string): boolean {
	return TERMINAL_STATUSES.has(status);
}

// -- State --------------------------------------------------------------------

/** Every run this page has seen, newest data winning, keyed by id. */
const runsById = new Map<string, RunState>();
/** Raw `started` of the oldest run held — the next ?before= cursor. */
let oldestStartedRaw: string | null = null;
let oldestStartedMs = Infinity;
/** /hooks roster (registered hooks appear as lanes even when idle) — fed
 * by dashboard.js, never fetched here: the classic script owns the single
 * /hooks fetch (boot, stream-open resyncs, changed{hooks} push signals,
 * fallback polls) and republishes the payload as window.whrHooks + a
 * whr:hooks-data event. Consuming that killed both this module's old 30s
 * roster poll and the page-load double-fetch (two scripts each fetching
 * /hooks). */
let hookMeta = new Map<string, HookSummary>();
/** The mounted <timeline-view>, once initTimeline ran (lane resync target). */
let timelineEl: TimelineViewElement | null = null;
let laneOrderKey = '';
let seeded = false; // first data application went through setData

// -- Stream-vouched trailing coverage ------------------------------------------
//
// The component hatches every UNCOVERED range in view up to `now` — the same
// crosshatch as an unfetched history gap — because coverage is only what the
// data explicitly vouched for; it cannot know a live SSE stream is attached.
// While the stream delivers (run deltas, hb keepalives, changed signals),
// every instant since the last claim IS vouched: the server pushes a delta
// for every run change, so a quiet stretch is KNOWN-empty, not unknown.
// This contract used to be met by ACCIDENT: every skipped-run delta forced a
// debounced rebuildAll(), whose setData re-registered coverage up to
// Date.now(), and this fleet skips constantly — so the trailing edge stayed
// current. #73 removed the skip rebuild (correctly) and with it the only
// thing extending coverage on a live stream: the hatch then grew from the
// connect snapshot to the now-marker, over live bars (the 2026-07-15
// full-window-crosshatch incident). claimLiveCoverage makes the contract
// EXPLICIT: each claim covers [previous claim, now] — contiguous ranges the
// component's tracker merges — bounding the trailing hatch to one heartbeat
// (~10s) worst case. Deliberately NOT clock-driven: claims ride only real
// feed bytes, so a dead stream's growing hatch (plus the component's stale
// note) stays the truth, and the reconnect snapshot's [pageOldest, now]
// claim back-fills the outage gap the moment the listing vouches for it.
let liveCoveredToMs = 0; // trailing edge of the last claim (0 = nothing claimed yet)

/** The next trailing-coverage claim: [last claim, now], advancing the mark.
 * Callers hand it to mergeData/setData `coverage`; zero-width claims (a
 * delta landing within the same ms, a clock stepping backwards) are no-ops
 * in the tracker. */
function claimLiveCoverage(now: number): { start: number; end: number } {
	const start = liveCoveredToMs > 0 ? Math.min(liveCoveredToMs, now) : now;
	if (now > liveCoveredToMs) liveCoveredToMs = now;
	return { start, end: now };
}

function applyHooksData(hooks: HookSummary[]): void {
	hookMeta = new Map(hooks.map((h) => [h.id, h]));
	if (timelineEl) syncLanes(timelineEl);
}
// Seed from whatever dashboard.js already fetched (its boot refresh usually
// beats this module's evaluation), then track pushes.
if (window.whrHooks) applyHooksData(window.whrHooks);
window.addEventListener('whr:hooks-data', (e) => {
	applyHooksData((e as CustomEvent<{ hooks: HookSummary[] }>).detail.hooks);
});

function noteOldest(r: RunState): void {
	const ms = Date.parse(r.started);
	if (!Number.isFinite(ms)) return;
	if (ms < oldestStartedMs || (ms === oldestStartedMs && (oldestStartedRaw === null || r.started < oldestStartedRaw))) {
		oldestStartedMs = ms;
		oldestStartedRaw = r.started;
	}
}

function ingestRuns(page: RunState[]): void {
	for (const r of page) {
		runsById.set(r.id, r);
		noteOldest(r);
	}
}

// -- The live feed -------------------------------------------------------------

/** The chart's hooks into the feed; attached once the component loads. */
interface FeedConsumer {
	onPage(page: RunState[]): void;
	/** prev = the state this delta replaced (undefined for a new run) —
	 * needed to re-render bars the run STOPPED affecting (e.g. the former
	 * holders' waiter badges when its wait ended). */
	onDelta(r: RunState, prev: RunState | undefined): void;
	/** Rebuild from runsById outright — used after removals, which
	 * mergeData cannot express. */
	rebuild(): void;
	markFresh(): void;
	/** Extend stream-vouched trailing coverage to now (see liveCoveredToMs).
	 * Rides the same triggers as markFresh; throttled internally. */
	extendCoverage(): void;
}
let chart: FeedConsumer | null = null;

/** Bounded fetch: identical contract to fetchJSON, but it CANNOT hang
 * forever (AbortSignal.timeout) — the wedge-proofing for every feed path. */
async function fetchJSONBounded<T>(url: string): Promise<T> {
	const res = await fetch(url, {
		headers: { Accept: 'application/json' },
		signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
	});
	if (!res.ok) throw new Error(`${url}: ${res.status}`);
	return (await res.json()) as T;
}

let streamLive = false;
let streamDownSince = Date.now(); // page load starts "down" until onopen
let es: EventSource | null = null;

/** Publish stream state: dashboard.js gates its own fallbacks (the modal's
 * 3s poll) on window.whrStreamLive; the event lets it react to recovery. */
function setStreamLive(live: boolean): void {
	if (streamLive === live) return;
	streamLive = live;
	if (!live) streamDownSince = Date.now();
	window.whrStreamLive = live;
	window.dispatchEvent(new CustomEvent('whr:stream-state', { detail: { live } }));
	console.info(`timeline: run stream ${live ? 'connected' : 'down (EventSource retries on its fixed 2s cadence)'}`);
}

// THE one freshness clock. Every byte of received data — SSE snapshot,
// SSE run delta, hb keepalive, or a successful fallback-poll page — lands
// here; nothing else may decide freshness. (The component ALSO auto-stamps
// on setData/mergeData since js-snippets#37, so rendered data can never
// coexist with a frozen clock — this explicit stamp additionally covers
// hb keepalives and no-change polls, where nothing merges.) Detection is
// per-call, not captured at init: a mixed-version tab (component upgraded
// under an open page) must never wedge freshness on a stale capability
// snapshot.
function fresh(): void {
	chart?.markFresh();
	// The same bytes that prove freshness vouch the trailing time range:
	// nothing changed since the last claim, or its delta would have arrived.
	chart?.extendCoverage();
}

/** One run delta: update the store, the chart, and anyone else listening
 * (dashboard.js's run modal refreshes in place off this event). */
function ingestDelta(r: RunState): void {
	const prev = runsById.get(r.id);
	runsById.set(r.id, r);
	noteOldest(r);
	chart?.onDelta(r, prev);
	window.dispatchEvent(new CustomEvent('whr:run-delta', { detail: { id: r.id, run: r } }));
	fresh(); // data arrived — the freshness clock advances HERE, unconditionally
}

function ingestPage(page: RunState[]): void {
	ingestRuns(page);
	chart?.onPage(page);
	fresh(); // ditto for page-shaped data (snapshot, resync, fallback poll)
}

/** On (re)connect and per fallback poll: any run we believe is live but
 * which fell out of the fetched window gets checked individually — the
 * permanent fix for merge-hole fiction (#3): nothing can stay "running"
 * forever just because its terminal update happened out of view. */
async function reconcileMissing(page: RunState[]): Promise<void> {
	const inPage = new Set(page.map((r) => r.id));
	const stale = [...runsById.values()]
		.filter((r) => !isTerminal(r.status) && !inPage.has(r.id))
		.slice(0, RECONCILE_MAX);
	let dropped = false;
	for (const r of stale) {
		try {
			ingestDelta(await fetchJSONBounded<RunState>(`/runs/${encodeURIComponent(r.id)}?tail=0`));
		} catch (e) {
			// 404 = the server genuinely no longer knows this run: it was in
			// flight during a restart, which loses live runs by design (they
			// never reached a terminal state, so history has nothing). Keeping
			// it would freeze a fictional "running" bar forever — remove it
			// and let the chart reflect server truth.
			if (e instanceof Error && / 404$|: 404$/.test(e.message)) {
				console.info(`timeline: run ${r.id} is gone server-side (lost in a restart) — dropping it`);
				runsById.delete(r.id);
				dropped = true;
				continue;
			}
			console.error('timeline: reconcile of', r.id, 'failed:', e);
		}
	}
	if (dropped) chart?.rebuild();
}

/** ONE full resync per stream (re)open, then stream-only. */
async function resyncOnce(): Promise<void> {
	try {
		const page = await fetchJSONBounded<RunState[]>(`/runs?max=${RESYNC_MAX}`);
		ingestPage(page);
		fresh();
		await reconcileMissing(page);
	} catch (e) {
		// Benign: the connect snapshot covers the same window; the next
		// delta or heartbeat keeps freshness honest.
		console.error('timeline: resync failed:', e);
	}
}

function openStream(): void {
	es = new EventSource(STREAM_PATH);
	es.onopen = () => {
		setStreamLive(true);
		void resyncOnce();
	};
	// On error EventSource retries BY ITSELF at the server-directed fixed
	// 2s cadence (readyState CONNECTING). No backoff of ours on top, no
	// close/reopen loop — the supervisor below only replaces a source the
	// browser has permanently CLOSED.
	es.onerror = () => {
		setStreamLive(false);
	};
	es.addEventListener('snapshot', (e) => {
		try {
			ingestPage(JSON.parse((e as MessageEvent<string>).data) as RunState[]);
			fresh();
		} catch (err) {
			console.error('timeline: bad snapshot event:', err);
		}
	});
	es.addEventListener('run', (e) => {
		try {
			ingestDelta(JSON.parse((e as MessageEvent<string>).data) as RunState);
			fresh();
		} catch (err) {
			console.error('timeline: bad run event:', err);
		}
	});
	// Heartbeats are the idle-stream freshness signal (data may legitimately
	// be quiet for hours; the FEED being alive is what markFresh attests).
	es.addEventListener('hb', () => fresh());
	// Coarse "section changed → refetch once" signals for the non-run admin
	// sections (hooks/images/concurrency/kv/events), multiplexed onto this
	// same connection. This module only re-publishes them: dashboard.js
	// owns those sections' fetching and rendering, and gates its own
	// fallback poll on window.whrStreamLive exactly like the runs feed.
	es.addEventListener('changed', (e) => {
		try {
			const d = JSON.parse((e as MessageEvent<string>).data) as { sections?: string[] };
			fresh(); // server data on the feed is liveness too
			window.dispatchEvent(new CustomEvent('whr:sections-changed', { detail: { sections: d.sections ?? [] } }));
		} catch (err) {
			console.error('timeline: bad changed event:', err);
		}
	});
}

/** The feed supervisor + fallback poll: one interval, armed once, never
 * cleared, every branch guarded — structurally unkillable (the answer to
 * root causes #1 and #2). While the stream is up it does nothing. */
let fallbackInFlight = false;
function startFeedSupervisor(): void {
	setInterval(() => {
		try {
			// A permanently-CLOSED EventSource never retries on its own —
			// that happens when the endpoint itself refuses (e.g. an older
			// server without /runs/stream). Replace it on this same fixed
			// cadence: not a backoff, never gives up.
			if (es !== null && es.readyState === EventSource.CLOSED) {
				setStreamLive(false);
				openStream();
			}
			if (streamLive) return;
			if (Date.now() - streamDownSince < STREAM_GRACE_MS) return;
			if (fallbackInFlight) return; // bounded fetch — cannot wedge this guard
			fallbackInFlight = true;
			fetchJSONBounded<RunState[]>(`/runs?max=${RESYNC_MAX}`)
				.then((page) => {
					ingestPage(page);
					fresh(); // markFresh only on SUCCESS — staleness stays honest
					return reconcileMissing(page);
				})
				.catch((e) => console.error('timeline: fallback poll failed:', e))
				.finally(() => {
					fallbackInFlight = false;
				});
		} catch (e) {
			console.error('timeline: feed supervisor tick failed:', e);
		}
	}, FALLBACK_POLL_MS);
}

// -- Run → timeline translation ------------------------------------------------

/** Who waits on a given run, derived CLIENT-SIDE by inverting the held
 * runs' waiting_on (lock holder + group holders). The server derives the
 * same thing for /runs (attachWaiters), but stream DELTAS deliberately
 * don't carry it — one RunState per event — so the chart inverts locally
 * and stays correct on push data. Rebuilt per render batch. */
interface WaiterRef {
	runId: string;
	hookId: string;
	kind: 'lock' | 'group';
	/** Lock key or group name ('?' when the server omitted it). */
	key: string;
}
let waiterIndex = new Map<string, WaiterRef[]>();

/** Display form of what a waiter is queued on: "lock k" | "a slot in group g". */
function waiterWhat(w: WaiterRef): string {
	return w.kind === 'lock' ? `lock ${w.key}` : `a slot in group ${w.key}`;
}

function holderIdsOf(r: RunState | undefined): string[] {
	const w = r?.waiting_on;
	if (!w || (r && isTerminal(r.status))) return [];
	if (w.kind === 'lock' && w.holder_run_id) return [w.holder_run_id];
	if (w.kind === 'group' && w.holder_run_ids) return w.holder_run_ids;
	return [];
}

function rebuildWaiterIndex(): void {
	waiterIndex = new Map();
	for (const r of runsById.values()) {
		const w = r.waiting_on;
		if (!w || isTerminal(r.status)) continue;
		const kind = w.kind === 'lock' ? 'lock' : w.kind === 'group' ? 'group' : null;
		if (kind === null) continue;
		for (const holder of holderIdsOf(r)) {
			const list = waiterIndex.get(holder) ?? [];
			list.push({ runId: r.id, hookId: r.hook_id, kind, key: w.key || '?' });
			waiterIndex.set(holder, list);
		}
	}
}

/** Style-map key for a run: status (+ waiting_on) → rendering treatment. */
function stateFor(r: RunState): string {
	// Segments carry historical state now: with wait_history present the
	// hatches live on SEGMENTS closed at their real end times, so the
	// interval itself stays neutral (an open-ended whole-bar hatch derived
	// from current status is exactly the growing-hatch bug). Old servers
	// without the field keep the legacy whole-bar treatment.
	if (r.waiting_on && !isTerminal(r.status) && !(r.wait_history && r.wait_history.length)) {
		return 'waiting';
	}
	switch (r.status) {
		case 'success':
		case 'running':
			return ''; // solid; running renders ongoing with a live edge
		case 'pending':
			return 'queued'; // dim
		case 'failure':
		case 'error':
		case 'timeout':
			return 'failed'; // unmissable emphasis (the failure IS the terminal fact)
		case 'cancelled':
			// First-class terminal treatment (a component built-in since the
			// feedback round): hollow body + dashed category-hue border —
			// "stopped, not failed" at any zoom, never the emphasis color,
			// never a solid success-look body. The kill tail
			// (cancel_requested_at → finished) STAYS a separate terminal-cut
			// segment (see runToInterval) composing over it. States are
			// freeform strings, so this is purely additive: an older cached
			// component treats the unknown key as the neutral default.
			return 'cancelled';
		default:
			// Unknown status (e.g. a future value): render safely dim.
			// Zero-duration runs become instant pips on their own.
			return 'dim';
	}
}

/** Friendly title when the server sends one (additive /runs field), else null. */
function runTitle(r: RunState): string | null {
	return typeof r.title === 'string' && r.title.trim() !== '' ? r.title : null;
}

/** "1st", "2nd", "3rd", "4th", … (11th-13th included). */
function ordinal(n: number): string {
	const rem = n % 100;
	if (rem >= 11 && rem <= 13) return `${n}th`;
	switch (n % 10) {
		case 1:
			return `${n}st`;
		case 2:
			return `${n}nd`;
		case 3:
			return `${n}rd`;
		default:
			return `${n}th`;
	}
}

/** Bar label: never hardcode "label = run id" — the title wins when present.
 * Wait indication lives HERE, on the span (no connectors): a queued run's
 * badge carries the group and its REAL place in line ("⧗ model-gateway ·
 * 3rd" — the server re-stamps position as the queue advances, so it counts
 * down live); a run others wait on carries how many it is holding up
 * ("⏳N"). Both derive from the same manager bookkeeping the /concurrency
 * drill-down shows (via waiting_on / the inverted index). */
function runLabel(r: RunState): string {
	const base = runTitle(r) ?? r.id.slice(0, 8);
	const w = r.waiting_on;
	if (w && w.kind === 'group' && !isTerminal(r.status)) {
		const place = typeof w.position === 'number' && w.position > 0 ? ` · ${ordinal(w.position)}` : '';
		return `${base} ⧗ ${w.key || 'group'}${place}`;
	}
	const waiters = waiterIndex.get(r.id);
	if (waiters && waiters.length > 0 && !isTerminal(r.status)) {
		return `${base} ⏳${waiters.length}`;
	}
	return base;
}

function runToInterval(r: RunState): TimelineInterval {
	const start = Date.parse(r.started);
	// finished is always serialized; zero time means "not finished".
	let end: number | null = tsPresent(r.finished) ? Date.parse(r.finished as string) : null;
	if (end === null && isTerminal(r.status)) {
		// Defensive: a terminal run always stamps finished. If one ever
		// doesn't, render an instant pip rather than an ongoing bar forever.
		end = tsPresent(r.started_at) ? Date.parse(r.started_at as string) : start;
	}
	const segments: TimelineSegment[] = [];
	if (tsPresent(r.started_at)) {
		const launched = Date.parse(r.started_at as string);
		if (launched > start) {
			// Queue wait (accepted → container launch) as a dim lead-in.
			segments.push({ start, end: launched, kind: 'queued' });
		}
	}
	// Historical waits: hatches with REAL boundaries — each ends exactly
	// when the server recorded the wait ending (an open segment, end null,
	// is a LIVE wait and legitimately rides the live edge).
	for (const ws of r.wait_history || []) {
		const s0 = Date.parse(ws.start);
		if (!Number.isFinite(s0)) continue;
		const e0 = ws.end && tsPresent(ws.end) ? Date.parse(ws.end) : null;
		segments.push({ start: s0, end: e0, kind: 'waiting' });
	}
	// The kill tail: the span up to the cancel request renders as the run's
	// normal life; the request → death tail is an 'outline' segment, which
	// the component draws as a TERMINAL CUT (dark scrim + bright cut line,
	// kept >= ~3 device px at any zoom — a sub-second docker-kill latency
	// tail can never vanish) composed over the cancelled treatment.
	if (r.status === 'cancelled' && tsPresent(r.cancel_requested_at)) {
		segments.push({ start: Date.parse(r.cancel_requested_at as string), end, kind: 'outline' });
	}
	return {
		id: r.id,
		laneId: r.hook_id,
		start,
		end,
		label: runLabel(r),
		category: r.hook_id, // stable hue per hook
		state: stateFor(r),
		segments: segments.length ? segments : undefined,
		data: r,
	};
}

// Waits draw NO connector lines (operator ruling: "make the lines go
// away"): the adapter feeds the component zero connectors, so no
// cross-canvas geometry, pixels, or hit-targets exist. Wait indication
// lives ON the spans instead — the queued run's "⧗ group · Nth" badge and
// the holder's "⏳N" badge (runLabel), the hatched wait segments
// (runToInterval), the tooltips, and the run modal's clickable
// holder/waiter links (dashboard.js). The component's generic connector
// capability is untouched upstream in js-snippets.

/**
 * Lane order: ALPHABETICAL by hook id — deterministic, stable, and
 * viewport-independent, full stop. (The old most-recent-activity sort
 * re-evaluated per poll, so rows visibly reordered themselves as runs
 * entered/left the window. Activity-based ordering, if ever wanted, is a
 * future explicit user toggle — never the default.)
 */
function computeLanes(): TimelineLane[] {
	const ids = new Set<string>(hookMeta.keys());
	for (const r of runsById.values()) ids.add(r.hook_id);
	return [...ids].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0)).map((id) => ({ id, label: id }));
}

// -- Tooltips -------------------------------------------------------------------

function trimText(s: string, max: number): string {
	return s.length > max ? s.slice(0, max - 1) + '…' : s;
}

function shortRunId(id: string): string {
	return id.length > 10 ? id.slice(0, 10) + '…' : id;
}

function statusColor(status: string): string | null {
	switch (status) {
		case 'success':
			return 'var(--success)';
		case 'failure':
		case 'error':
		case 'timeout':
			return 'var(--failure)';
		case 'running':
			return 'var(--running)';
		case 'pending':
			return 'var(--pending)';
		default:
			return null;
	}
}

/** One `key: value` tooltip line, reusing the component's tt-* styling. */
function ttRow(key: string, value: string | Node): HTMLElement {
	const v = typeof value === 'string' ? el('span', { class: 'tt-v' }, value) : value;
	return el('div', { class: 'tt-row' }, el('span', { class: 'tt-k' }, key), v);
}

/** Appends wait/lock rows for a live waiting run (short rows — the tooltip
 * never wraps, so the holder gets its own line instead of one long one). */
function appendWaitingRows(frag: DocumentFragment, r: RunState): void {
	const w = r.waiting_on;
	if (!w || isTerminal(r.status)) return;
	if (w.kind === 'lock') {
		frag.appendChild(ttRow('waiting', `on lock ${w.key || '?'}`));
		if (w.holder_run_id) {
			const holder = shortRunId(w.holder_run_id) + (w.holder_hook_id ? ` (${w.holder_hook_id})` : '');
			frag.appendChild(ttRow('holder', holder));
		}
		return;
	}
	if (w.kind === 'group') {
		// The ⧗ badge ("⧗ model-gateway · 3rd") in plain language, from the
		// SAME waiting_on fields: "waiting for model-gateway · 3rd in line".
		const place =
			typeof w.position === 'number' && w.position > 0 ? ` · ${ordinal(w.position)} in line` : '';
		frag.appendChild(ttRow('waiting', `for ${w.key || 'group'}${place}`));
		for (const holder of (w.holder_run_ids || []).slice(0, 3)) {
			const hr = runsById.get(holder);
			frag.appendChild(ttRow('held by', shortRunId(holder) + (hr ? ` (${hr.hook_id})` : '')));
		}
		if ((w.holder_run_ids || []).length > 3) {
			frag.appendChild(ttRow('', `…and ${(w.holder_run_ids as string[]).length - 3} more`));
		}
		return;
	}
	const remaining =
		w.until && tsPresent(w.until) ? Math.max(0, Date.parse(w.until) - Date.now()) : null;
	let line = w.reason || 'declared wait';
	if (remaining !== null) line += ` — ${fmtDuration(remaining)} left`;
	frag.appendChild(ttRow('waiting', line));
}

function runTooltip(r: RunState): Node {
	const frag = document.createDocumentFragment();
	// The friendly title (when the server sends one) is the primary line;
	// the run id demotes to a detail row. Without it: the old id line.
	const title = runTitle(r);
	frag.appendChild(el('div', { class: 'tt-title' }, title ? trimText(title, 140) : `${r.hook_id} · ${shortRunId(r.id)}`));
	if (title) frag.appendChild(ttRow('run', `${r.hook_id} · ${shortRunId(r.id)}`));
	const color = statusColor(r.status);
	frag.appendChild(
		ttRow('status', el('span', { class: 'tt-v', style: color ? `color: ${color}` : null }, r.status)),
	);
	if (isTerminal(r.status) && r.status !== 'cancelled') {
		const bad = r.exit_code !== 0;
		frag.appendChild(
			ttRow(
				'exit',
				el('span', { class: 'tt-v', style: bad ? 'color: var(--failure)' : null }, String(r.exit_code)),
			),
		);
	}
	frag.appendChild(ttRow('queued', fmtTime(r.started)));
	frag.appendChild(ttRow('waited', runWaited(r) || '—'));
	frag.appendChild(ttRow('ran', runDuration(r) || '—'));
	if (r.error) frag.appendChild(ttRow('error', trimText(r.error, 160)));
	appendWaitingRows(frag, r);
	const held = waiterIndex.get(r.id);
	if (held && held.length > 0 && !isTerminal(r.status)) {
		// The ⏳N badge in plain language, from the SAME inverted index (N is
		// exactly the badge count): when every waiter is queued on one group
		// slot — the common case — name it ("holds the model-gateway slot ·
		// 2 waiting"); mixed lock/group waiters keep the generic count, with
		// the per-waiter rows below spelling out each one.
		const oneGroup = held.every((h) => h.kind === 'group' && h.key === held[0].key);
		frag.appendChild(
			ttRow(
				'holds',
				oneGroup
					? `the ${held[0].key} slot · ${held.length} waiting`
					: `${held.length} run(s) waiting on this run`,
			),
		);
		for (const wr of held.slice(0, 3)) {
			frag.appendChild(ttRow('', `${shortRunId(wr.runId)} (${wr.hookId}) → ${waiterWhat(wr)}`));
		}
		if (held.length > 3) frag.appendChild(ttRow('', `…and ${held.length - 3} more`));
	}
	return frag;
}

function laneTooltip(lane: TimelineLane): Node {
	const frag = document.createDocumentFragment();
	frag.appendChild(el('div', { class: 'tt-title' }, lane.id));
	const desc = hookMeta.get(lane.id)?.description;
	if (desc) frag.appendChild(ttRow('hook', trimText(desc, 120)));
	frag.appendChild(ttRow('', 'click to open this hook’s page'));
	return frag;
}

// (The adapter-side skip pre-merge that used to live here is GONE: skipped
// runs — zero-duration instants — now feed through as INDIVIDUAL intervals
// at their true timestamps, each with its own label/tooltip/modal link. The
// component clusters visually-overlapping instant markers itself, scale-
// aware: within ~12px they merge into ONE ×N point marker occupying ONE
// packing slot — so a redelivery burst still can't blow up lane height —
// and zooming in progressively splits every cluster back into true-time
// pips. The old fixed 5s data-space buckets rendered as duration bars and
// never split on zoom; a pile of instants has no length.)

// -- The element + wiring --------------------------------------------------------

function initTimeline(): void {
	const tl = document.getElementById('runs-timeline') as TimelineViewElement | null;
	if (tl === null || typeof tl.setData !== 'function') return; // markup missing / element failed to register

	// ×N instant-cluster hits never land here: the component builds cluster
	// summary tooltips itself and never consults tooltipFor for them — so
	// every interval id received IS a run id.
	tl.tooltipFor = (hit: TimelineHit) => {
		if (hit.type === 'interval') {
			const r = runsById.get(hit.interval.id);
			return r ? runTooltip(r) : null;
		}
		if (hit.type === 'lane') return laneTooltip(hit.lane);
		return null; // connectors are never fed, so no connector hits exist
	};

	// Staleness marking (feature-detected per call, never captured): the
	// component dims/flags the chart when markFresh hasn't been called for
	// staleAfterMs. With it, a dead feed LOOKS dead instead of extrapolating
	// running bars; without it, the fallback poll still keeps data honest.
	// staleAfterMs is set EXPLICITLY whenever the API exists — the
	// component's own default (10s, tuned for 2s pollers) would otherwise
	// hatch a healthy push stream between 10s-apart heartbeats.
	if (typeof tl.markFresh === 'function') tl.staleAfterMs = STALE_AFTER_MS;

	// Teach the "?" legend panel the badge glyphs THIS adapter composes into
	// labels (runLabel) — the component's built-in rows only cover its own
	// vocabulary. Feature-detected: the live Pages component may predate
	// legendEntries, in which case an old component keeps exactly today's
	// behavior (built-in legend rows only, no errors).
	if ('legendEntries' in tl) {
		tl.legendEntries = [
			{ glyph: '⧗', text: 'waiting for a concurrency-group slot (group · place in line)' },
			{ glyph: '⏳N', text: 'holding a slot N queued runs are waiting on' },
		];
	}

	// Click-through: a bar opens the same run modal the tables use; a lane
	// label opens the hook's drill-down page (same href the hooks table uses).
	tl.addEventListener('intervalclick', (e: Event) => {
		// Single intervals only — a ×N cluster click ZOOMS to its member
		// extent component-side (splitting the cluster) and never dispatches
		// intervalclick — so this id is always a run id, and every skipped
		// run opens ITS OWN run modal again.
		const detail = (e as CustomEvent<{ interval: TimelineInterval }>).detail;
		void showRun(detail.interval.id);
	});
	tl.addEventListener('laneclick', (e: Event) => {
		const detail = (e as CustomEvent<{ lane: TimelineLane }>).detail;
		location.hash = '#hook=' + encodeURIComponent(detail.lane.id);
	});

	// -- History paging (drag into the past) -----------------------------------
	//
	// The component asks for [start, end] when the viewport reaches uncovered
	// past (BACKWARD ranges only — it never chases the live edge). Page
	// /runs?before=<cursor> newest-first, BOUNDED BY THE REQUESTED WINDOW:
	// the chain stops the moment a page's oldest `started` predates the
	// range floor. The cursor resumes from the oldest run held only when
	// that run sits INSIDE the requested range (the normal contiguous
	// pan-back; its RAW `started` string keeps nanosecond tiling — a Date
	// round-trip would truncate and mis-tile). For any other request the
	// cursor seeds from the range's own end — NEVER unconditionally from
	// the oldest run held: that turned every request newer than it into
	// "fetch one page deeper than everything", an unbounded walk to
	// retention (the 30 req/s page-load flood, together with the
	// component's forward-gap refire). An empty/short page means history
	// ran out (retention or genuinely first run) → {exhausted: true} pins
	// the "history ends here" boundary. Single-flight: a second concurrent
	// walk (double init, a coverage reset mid-flight) throws instead of
	// starting its own paging chain — the component backs off and retries.
	let backfillInFlight = false;
	const backfill = async (start: number, end: number): Promise<{ exhausted?: boolean } | undefined> => {
		if (backfillInFlight) throw new Error('timeline backfill: already in flight');
		backfillInFlight = true;
		try {
			let cursor: string;
			if (oldestStartedRaw !== null && oldestStartedMs > start && oldestStartedMs <= end + 60_000) {
				cursor = oldestStartedRaw; // contiguous pan-back: exact next cursor
			} else {
				// Disjoint request (hole, or deeper jump): page from its end.
				// Millisecond precision is fine — nothing held tiles against it.
				cursor = new Date(end).toISOString();
			}
			let cursorMs = Date.parse(cursor);
			for (let page = 0; page < BACKFILL_MAX_PAGES; page++) {
				const rows = await fetchJSONBounded<RunState[]>(
					`/runs?before=${encodeURIComponent(cursor)}&max=${BACKFILL_MAX}`,
				);
				if (rows.length === 0) return { exhausted: true };
				ingestRuns(rows);
				syncLanes(tl);
				const oldest = rows[rows.length - 1]; // pages are newest-first
				tl.mergeData({
					intervals: rows.map(runToInterval),
					coverage: { start: Date.parse(oldest.started), end: cursorMs },
				});
				cursor = oldest.started; // raw server string — the exact next cursor
				cursorMs = Date.parse(cursor);
				if (rows.length < BACKFILL_MAX) return { exhausted: true };
				if (cursorMs <= start) return undefined; // window floor reached — STOP
			}
			// Page cap: bail loudly instead of claiming coverage we didn't fetch.
			// The component backs off and retries; the cursor resumes from the
			// (now deeper) oldest run, so every attempt makes progress.
			throw new Error('timeline backfill: page cap reached');
		} finally {
			backfillInFlight = false;
		}
	};
	// Armed only after the first data seeds coverage (see applyPage): first
	// paint is the feed's own window — a page load issues ZERO ?before=
	// requests until the user actually pans into uncovered history.
	const armBackfill = (): void => {
		if (tl.loadRange === null) tl.loadRange = backfill;
	};

	// "history ends here — retention 48h" when the server reports a run store.
	void (async () => {
		try {
			const cfg = await fetchJSONBounded<{ run_retention?: string }>('/config');
			if (cfg && typeof cfg.run_retention === 'string' && cfg.run_retention !== '') {
				tl.setAttribute('history-end-text', `history ends here — retention ${cfg.run_retention}`);
			}
		} catch {
			/* older server / no store: keep the component's default label */
		}
	})();

	// -- Feed → chart -----------------------------------------------------------

	const applyPage = (page: RunState[]): void => {
		const now = Date.now();
		rebuildWaiterIndex(); // labels/tooltips read it during interval mapping
		if (maybePrune(tl, now)) return; // prune did a full setData already
		const pageOldestMs = page.length > 0 ? Date.parse(page[page.length - 1].started) : now - 60_000;
		liveCoveredToMs = Math.max(liveCoveredToMs, now); // page claims through now
		const data: TimelineData = {
			intervals: [...runsById.values()].map(runToInterval),
			coverage: { start: pageOldestMs, end: now },
		};
		if (!seeded) {
			seeded = true;
			laneOrderKey = '';
			tl.setData(data);
			// Default zoom: a 10-minute window on initial load (the
			// component defaults to 15). Ending exactly at now keeps the
			// follow pin engaged; only the INITIAL span changes — user
			// pans/zooms and jumpToNow keep their own span from then on.
			tl.setViewport(now - 10 * 60_000, now);
			armBackfill(); // coverage exists now — history paging may engage
		} else {
			// MERGE, never setData: later snapshots/resyncs must not wipe the
			// backfilled history the component already holds.
			tl.mergeData(data);
		}
		syncLanes(tl);
	};

	// Skipped runs need no special-casing: each is a zero-duration instant
	// interval upserted like any other delta — the component's scale-aware
	// ×N clustering absorbs redelivery bursts (one packing slot per
	// cluster), so no adapter-side pre-merge or rebuild debounce exists.
	const applyDelta = (r: RunState, prev: RunState | undefined): void => {
		if (!seeded) {
			// No coverage yet (deltas can precede the first page when the
			// stream connects before the seed fetch returns): render what we
			// hold as the seed.
			applyPage([...runsById.values()]);
			return;
		}
		// A delta can change OTHER bars' badges: every run this one was — or
		// now is — waiting on gains/loses its ⏳ waiter count. Re-merge the
		// union of the old and new holder sets alongside the run itself.
		const affected = new Set<string>([r.id]);
		for (const holder of holderIdsOf(prev)) affected.add(holder);
		for (const holder of holderIdsOf(r)) affected.add(holder);
		rebuildWaiterIndex();
		const intervals = [...affected]
			.map((id) => runsById.get(id))
			.filter((x): x is RunState => x !== undefined)
			.map(runToInterval);
		// The delta also vouches the range since the last claim (fold the
		// trailing-coverage extension into the merge this path already does —
		// without it nothing extends coverage on a live stream, and the
		// component hatches [connect snapshot, now] as unknown history).
		tl.mergeData({ intervals, coverage: claimLiveCoverage(Date.now()) });
		syncLanes(tl);
	};

	// Full rebuild (the only way to REMOVE an interval — mergeData upserts).
	// Coverage restarts at the held window, so deeper panning re-pages from
	// the server; same trade maybePrune makes.
	const rebuildAll = (): void => {
		oldestStartedRaw = null;
		oldestStartedMs = Infinity;
		for (const r of runsById.values()) noteOldest(r);
		laneOrderKey = '';
		const lanes = computeLanes();
		laneOrderKey = lanes.map((l) => l.id).join('\n');
		const now = Date.now();
		liveCoveredToMs = Math.max(liveCoveredToMs, now); // rebuild claims through now
		tl.setData({
			lanes,
			intervals: [...runsById.values()].map(runToInterval),
			connectors: [], // rebuild replaces data — pin connectors to none
			coverage: { start: Number.isFinite(oldestStartedMs) ? oldestStartedMs : now - 60_000, end: now },
		});
	};

	chart = {
		onPage: applyPage,
		onDelta: applyDelta,
		rebuild: rebuildAll,
		markFresh: () => {
			if (typeof tl.markFresh === 'function') tl.markFresh();
		},
		extendCoverage: () => {
			// hb keepalives and changed signals are the quiet-feed path: no
			// interval merge carries the claim, so make a coverage-only one.
			// Throttled so a signal burst can't spray rebuilds; the per-delta
			// path claims inside its own merge and lands here as a no-op.
			if (!seeded) return; // no baseline claim yet — the first page owns it
			const now = Date.now();
			if (now - liveCoveredToMs < LIVE_COVERAGE_MIN_STEP_MS) return;
			tl.mergeData({ coverage: claimLiveCoverage(now) });
		},
	};

	// The chart attached after the feed started: render everything already
	// ingested (seed fetch and/or connect snapshot that raced the component
	// load).
	if (runsById.size > 0) applyPage([...runsById.values()]);

	// Lane metadata (descriptions, kill-switch state) is push-fed via
	// whr:hooks-data (see applyHooksData) — register as its resync target
	// and apply whatever roster has already arrived.
	timelineEl = tl;
	if (hookMeta.size > 0) syncLanes(tl);
}

/** Re-apply lane order only when it actually changed (setLanes re-ingests). */
function syncLanes(tl: TimelineViewElement): void {
	const lanes = computeLanes();
	const key = lanes.map((l) => l.id).join('\n');
	if (key === laneOrderKey) return;
	laneOrderKey = key;
	tl.setLanes(lanes);
}

/**
 * Memory guard: only when the held set grows silly (>10k runs) drop the
 * oldest beyond the newest 8k and rebuild the component's data outright
 * (coverage restarts at the kept window, so panning further back re-pages
 * from the server instead of showing pruned emptiness as truth).
 */
function maybePrune(tl: TimelineViewElement, now: number): boolean {
	if (runsById.size <= PRUNE_AT) return false;
	rebuildWaiterIndex();
	const keep = [...runsById.values()]
		.sort((a, b) => Date.parse(b.started) - Date.parse(a.started))
		.slice(0, PRUNE_TO);
	runsById.clear();
	oldestStartedRaw = null;
	oldestStartedMs = Infinity;
	ingestRuns(keep);
	laneOrderKey = '';
	const lanes = computeLanes();
	laneOrderKey = lanes.map((l) => l.id).join('\n');
	liveCoveredToMs = Math.max(liveCoveredToMs, now); // prune claims through now
	tl.setData({
		lanes,
		intervals: keep.map(runToInterval),
		connectors: [], // prune replaces data — pin connectors to none
		coverage: { start: oldestStartedMs, end: now },
	});
	return true;
}

// -- Runs-table toggle ------------------------------------------------------------
//
// The timeline replaces the runs table as the primary view; the table stays
// reachable behind a small toggle (persisted, default hidden).

function tablePref(): boolean {
	try {
		return localStorage.getItem(TABLE_PREF_KEY) === '1';
	} catch {
		return false;
	}
}

function applyTablePref(show: boolean): void {
	const section = document.getElementById('runs-section');
	if (section) section.hidden = !show;
	const btn = document.getElementById('timeline-table-toggle');
	if (btn) btn.textContent = show ? 'Hide table' : 'Show table';
	// A just-revealed table starts stale (nothing fetches /runs for a
	// hidden one): tell dashboard.js so it refills immediately instead of
	// waiting for the next run delta.
	if (show) window.dispatchEvent(new CustomEvent('whr:runs-table-shown'));
}

function initTableToggle(): void {
	applyTablePref(tablePref());
	const btn = document.getElementById('timeline-table-toggle');
	if (!btn) return;
	btn.addEventListener('click', () => {
		const show = !tablePref();
		try {
			localStorage.setItem(TABLE_PREF_KEY, show ? '1' : '0');
		} catch {
			/* private mode etc. — the toggle still works for this page load */
		}
		applyTablePref(show);
	});
}

// -- Boot: feed first, then the component (retrying forever) ---------------------
//
// The FEED (EventSource + fallback supervisor + seed fetch) starts before —
// and independently of — the <timeline-view> component load: runsById fills,
// whr:run-delta events flow to dashboard.js (the run modal's live refresh),
// and stream state publishes, even if GitHub Pages is unreachable. The
// component module lives on js-snippets' Pages and is imported at runtime;
// that fetch can fail, and a failure must NOT leave the chart section
// permanently dead: the load retries on a FIXED short cadence forever — no
// growing backoff, no attempt cap, never parks silently — with a visible
// "chart loading…" note in the section until it succeeds. Retries cache-bust
// the URL (?retry=N) because a failed module fetch can be memoized in the
// browser's module map — a bare re-import would reject from cache without
// ever hitting the network.

function loadingNote(): HTMLElement | null {
	let note = document.getElementById('timeline-loading');
	if (note) return note;
	const tl = document.getElementById('runs-timeline');
	if (!tl) return null;
	note = el('p', { id: 'timeline-loading', class: 'empty' }, 'chart loading…');
	tl.before(note);
	return note;
}

async function loadComponentForever(): Promise<void> {
	for (let attempt = 0; ; attempt++) {
		try {
			await import(attempt === 0 ? COMPONENT_URL : `${COMPONENT_URL}?retry=${attempt}`);
			return;
		} catch (e) {
			loadingNote(); // make the pending state visible (idempotent)
			console.error(`timeline: component load failed (retry in ${COMPONENT_RETRY_MS}ms):`, e);
			await new Promise((r) => setTimeout(r, COMPONENT_RETRY_MS));
		}
	}
}

async function boot(): Promise<void> {
	// The table toggle must work even while (or if) the chart is loading —
	// the runs table is the fallback view and depends only on this module.
	initTableToggle();
	// Seed fetch (coverage for the first paint), then the stream, then the
	// unkillable supervisor. None of these wait on the component.
	void (async () => {
		try {
			ingestPage(await fetchJSONBounded<RunState[]>(`/runs?max=${RESYNC_MAX}`));
		} catch (e) {
			console.error('timeline: initial seed fetch failed (the stream snapshot covers it):', e);
		}
	})();
	openStream();
	startFeedSupervisor();
	await loadComponentForever();
	document.getElementById('timeline-loading')?.remove();
	initTimeline();
}

void boot();
