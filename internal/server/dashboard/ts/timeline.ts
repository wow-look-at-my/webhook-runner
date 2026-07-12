/*
 * timeline.ts — the dashboard's primary runs view: a realtime swimlane
 * timeline (one lane per hook) rendered by the vendored <timeline-view>
 * canvas element from wow-look-at-my/js-snippets.
 *
 * This file is the webhook-runner adapter: it knows the /runs, /hooks and
 * /config shapes and the runner's semantics (queued vs started_at vs
 * finished, terminal statuses, waiting_on/waiters) and translates them into
 * the component's generic lanes/intervals/connectors model. The component
 * itself is generic and vendored verbatim under ts/vendor/ — fix component
 * bugs upstream, never here.
 *
 * Built by ts0 (see ../ts0.json and the //go:generate directive in
 * dashboard.go) into assets/timeline.js — an IIFE bundle that loads as a
 * classic <script> AFTER dashboard.js and reuses its globals (fetchJSON,
 * el, fmtTime, tsPresent, showRun, … — declared in globals.d.ts).
 *
 * Runner semantics encoded here:
 *   - `started` is the QUEUED/accepted instant; `started_at` (absent while
 *     pending) is the container launch; queue wait renders as a dim leading
 *     'queued' segment so a run stuck behind a concurrency group never
 *     reads as a slow run.
 *   - `finished` is ALWAYS serialized — a Go zero time ("0001-01-01…") on
 *     live runs — so presence goes through tsPresent, never truthiness.
 *   - `waiting_on` / `waiters` (first-class waits) and any new status
 *     values are FEATURE-DETECTED: absent fields degrade to the plain
 *     rendering, unknown statuses render dim instead of crashing.
 *   - Dragging into the past pages GET /runs?before=<cursor> — the cursor
 *     is the oldest run's RAW `started` string (nanosecond precision;
 *     never round-tripped through Date).
 */

import './vendor/js-snippets/ui/timeline-view.ts';
import type {
	TimelineConnector,
	TimelineData,
	TimelineHit,
	TimelineInterval,
	TimelineLane,
	TimelineSegment,
	TimelineViewElement,
} from './vendor/js-snippets/ui/timeline-view.ts';

// -- Runner API shapes (the fields this adapter consumes) --------------------

/** GET /runs list entry (internal/runs.RunState, output stripped). */
interface RunState {
	id: string;
	hook_id: string;
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
	/** Feature-detected (first-class waits): why a live run is paused. */
	waiting_on?: {
		kind?: string; // "wait" | "lock"
		reason?: string;
		until?: string;
		key?: string;
		holder_run_id?: string;
		holder_hook_id?: string;
	};
	/** Feature-detected: live runs whose lock acquire this run is blocking. */
	waiters?: Array<{ run_id: string; hook_id: string; key?: string }>;
}

/** GET /hooks list entry (hook summary + operator kill switch). */
interface HookSummary {
	id: string;
	description?: string;
	schedule?: string;
	disabled?: boolean;
}

const TIMELINE_POLL_MS = 2000; // /runs poll (independent of dashboard.js's 3s)
const HOOKS_POLL_MS = 30_000; // /hooks poll (lane roster)
const POLL_MAX = 400; // newest window fetched per poll
const BACKFILL_MAX = 200; // page size for ?before= history paging
const BACKFILL_MAX_PAGES = 30; // per loadRange call; a reject resumes deeper
const PRUNE_AT = 10_000; // runs held in memory before pruning kicks in
const PRUNE_TO = 8_000; // newest runs kept when it does
const TABLE_PREF_KEY = 'whr-show-runs-table';

/** Statuses that mean the run is over (mirror internal/runs.Status). */
const TERMINAL_STATUSES = new Set(['success', 'failure', 'timeout', 'error', 'cancelled']);

function isTerminal(status: string): boolean {
	return TERMINAL_STATUSES.has(status);
}

// -- State --------------------------------------------------------------------

/** Every run this page has seen, newest poll data winning, keyed by id. */
const runsById = new Map<string, RunState>();
/** Raw `started` of the oldest run held — the next ?before= cursor. */
let oldestStartedRaw: string | null = null;
let oldestStartedMs = Infinity;
/** GET /hooks roster (registered hooks appear as lanes even when idle). */
let hookMeta = new Map<string, HookSummary>();
let laneOrderKey = '';
let seeded = false; // first successful poll went through setData

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

// -- Run → timeline translation ------------------------------------------------

/** Style-map key for a run: status (+ waiting_on) → rendering treatment. */
function stateFor(r: RunState): string {
	if (r.waiting_on && !isTerminal(r.status)) return 'waiting'; // hatched
	switch (r.status) {
		case 'success':
		case 'running':
			return ''; // solid; running renders ongoing with a live edge
		case 'pending':
			return 'queued'; // dim
		case 'failure':
		case 'error':
		case 'timeout':
			return 'failed'; // unmissable emphasis
		case 'cancelled':
			return 'outline'; // hollow
		default:
			// Unknown status (e.g. a future 'skipped'): render safely dim.
			// Zero-duration runs become instant pips on their own.
			return 'dim';
	}
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
	let segments: TimelineSegment[] | undefined;
	if (tsPresent(r.started_at)) {
		const launched = Date.parse(r.started_at as string);
		if (launched > start) {
			// Queue wait (accepted → container launch) as a dim lead-in.
			segments = [{ start, end: launched, kind: 'queued' }];
		}
	}
	return {
		id: r.id,
		laneId: r.hook_id,
		start,
		end,
		label: r.id.slice(0, 8),
		category: r.hook_id, // stable hue per hook
		state: stateFor(r),
		segments,
		data: r,
	};
}

/** Live lock waits become connectors: waiter → holder, labeled by key. */
function computeConnectors(): TimelineConnector[] {
	const out: TimelineConnector[] = [];
	for (const r of runsById.values()) {
		const w = r.waiting_on;
		if (w && w.kind === 'lock' && w.holder_run_id && !isTerminal(r.status)) {
			out.push({
				fromIntervalId: r.id,
				toIntervalId: w.holder_run_id,
				kind: 'lock',
				label: w.key || 'lock',
			});
		}
	}
	return out;
}

/** Lane order: most recent activity first; registered-but-idle hooks last. */
function computeLanes(): TimelineLane[] {
	const latest = new Map<string, number>();
	for (const r of runsById.values()) {
		const t = Date.parse(r.started);
		const prev = latest.get(r.hook_id);
		if (prev === undefined || t > prev) latest.set(r.hook_id, t);
	}
	const ids = new Set<string>([...latest.keys(), ...hookMeta.keys()]);
	return [...ids]
		.sort((a, b) => {
			const la = latest.get(a);
			const lb = latest.get(b);
			if (la !== undefined && lb !== undefined) return lb - la || (a < b ? -1 : 1);
			if (la !== undefined) return -1;
			if (lb !== undefined) return 1;
			return a < b ? -1 : a > b ? 1 : 0;
		})
		.map((id) => ({ id, label: id }));
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

function waitingLine(r: RunState): string | null {
	const w = r.waiting_on;
	if (!w || isTerminal(r.status)) return null;
	const remaining =
		w.until && tsPresent(w.until) ? Math.max(0, Date.parse(w.until) - Date.now()) : null;
	if (w.kind === 'lock') {
		let line = `on lock ${w.key || '?'}`;
		if (w.holder_run_id) {
			line += ` — held by ${shortRunId(w.holder_run_id)}`;
			if (w.holder_hook_id) line += ` (${w.holder_hook_id})`;
		}
		return line;
	}
	let line = w.reason || 'declared wait';
	if (remaining !== null) line += ` — ${fmtDuration(remaining)} left`;
	return line;
}

function runTooltip(r: RunState): Node {
	const frag = document.createDocumentFragment();
	frag.appendChild(el('div', { class: 'tt-title' }, `${r.hook_id} · ${shortRunId(r.id)}`));
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
	const waiting = waitingLine(r);
	if (waiting) frag.appendChild(ttRow('waiting', waiting));
	if (r.waiters && r.waiters.length > 0) {
		frag.appendChild(
			ttRow('holds', `${r.waiters.length} run(s) waiting on this run's lock(s)`),
		);
	}
	return frag;
}

function connectorTooltip(c: TimelineConnector, missing?: 'from' | 'to'): Node {
	const frag = document.createDocumentFragment();
	frag.appendChild(el('div', { class: 'tt-title' }, `lock ${c.label || ''}`.trim()));
	const describe = (id: string): string => {
		const r = runsById.get(id);
		return r ? `${shortRunId(id)} (${r.hook_id})` : `${shortRunId(id)} (not loaded)`;
	};
	frag.appendChild(ttRow('waiter', describe(c.fromIntervalId)));
	frag.appendChild(ttRow('holder', describe(c.toIntervalId)));
	if (missing) frag.appendChild(ttRow('note', `${missing} endpoint not loaded`));
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

// -- The element + wiring --------------------------------------------------------

function initTimeline(): void {
	const tl = document.getElementById('runs-timeline') as TimelineViewElement | null;
	if (tl === null || typeof tl.setData !== 'function') return; // markup missing / element failed to register

	tl.tooltipFor = (hit: TimelineHit) => {
		if (hit.type === 'interval') {
			const r = runsById.get(hit.interval.id);
			return r ? runTooltip(r) : null;
		}
		if (hit.type === 'connector') return connectorTooltip(hit.connector, hit.missingEndpoint);
		if (hit.type === 'lane') return laneTooltip(hit.lane);
		return null;
	};

	// Click-through: a bar opens the same run modal the tables use; a lane
	// label opens the hook's drill-down page (same href the hooks table uses).
	tl.addEventListener('intervalclick', (e: Event) => {
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
	// past. Page /runs?before=<cursor> newest-first until the range is
	// reached; the cursor is always the RAW `started` string of the oldest
	// run held (nanosecond precision — a Date round-trip would truncate and
	// mis-tile the pages). An empty/short page means history ran out
	// (retention or genuinely first run) → {exhausted: true} pins the
	// "history ends here" boundary.
	tl.loadRange = async (start: number, end: number) => {
		let cursor: string;
		if (oldestStartedRaw === null || oldestStartedMs > end + 60_000) {
			// Nothing held yet (or nothing near the requested range): seed the
			// cursor from the range end. Millisecond precision is fine here —
			// there are no held runs for it to collide with.
			cursor = new Date(end).toISOString();
		} else {
			cursor = oldestStartedRaw;
		}
		let cursorMs = Date.parse(cursor);
		for (let page = 0; page < BACKFILL_MAX_PAGES; page++) {
			const rows = await fetchJSON<RunState[]>(
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
			if (cursorMs <= start) return; // requested range reached
		}
		// Page cap: bail loudly instead of claiming coverage we didn't fetch.
		// The component backs off and retries; the cursor resumes from the
		// (now deeper) oldest run, so every attempt makes progress.
		throw new Error('timeline backfill: page cap reached');
	};

	// "history ends here — retention 48h" when the server reports a run store.
	void (async () => {
		try {
			const cfg = await fetchJSON<{ run_retention?: string }>('/config');
			if (cfg && typeof cfg.run_retention === 'string' && cfg.run_retention !== '') {
				tl.setAttribute('history-end-text', `history ends here — retention ${cfg.run_retention}`);
			}
		} catch {
			/* older server / no store: keep the component's default label */
		}
	})();

	initTableToggle();

	// -- Polling ---------------------------------------------------------------

	let pollInFlight = false;
	const pollRuns = async (): Promise<void> => {
		if (pollInFlight) return;
		pollInFlight = true;
		try {
			const page = await fetchJSON<RunState[]>(`/runs?max=${POLL_MAX}`);
			const now = Date.now();
			ingestRuns(page);
			if (maybePrune(tl, now)) return; // prune did a full setData already
			const pageOldestMs = page.length > 0 ? Date.parse(page[page.length - 1].started) : now - 60_000;
			const data: TimelineData = {
				intervals: page.map(runToInterval),
				coverage: { start: pageOldestMs, end: now },
			};
			if (!seeded) {
				seeded = true;
				laneOrderKey = '';
				tl.setData(data);
			} else {
				tl.mergeData(data);
			}
			syncLanes(tl);
			tl.setConnectors(computeConnectors());
		} catch (e) {
			console.error('timeline: poll failed:', e);
		} finally {
			pollInFlight = false;
		}
	};

	const pollHooks = async (): Promise<void> => {
		try {
			const hooks = await fetchJSON<HookSummary[]>('/hooks');
			hookMeta = new Map(hooks.map((h) => [h.id, h]));
			syncLanes(tl);
		} catch (e) {
			console.error('timeline: hooks poll failed:', e);
		}
	};

	// Poll only while the timeline can be seen: tab visible AND the overview
	// main is the active view (the #hook= drill-down hides it).
	const visible = (): boolean => !document.hidden && currentHookId() === null;
	const tick = (): void => {
		if (visible()) void pollRuns();
	};
	setInterval(tick, TIMELINE_POLL_MS);
	setInterval(() => {
		if (visible()) void pollHooks();
	}, HOOKS_POLL_MS);
	document.addEventListener('visibilitychange', tick);
	window.addEventListener('hashchange', tick);
	void pollHooks();
	tick();
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
	tl.setData({
		lanes,
		intervals: keep.map(runToInterval),
		connectors: computeConnectors(),
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

initTimeline();
