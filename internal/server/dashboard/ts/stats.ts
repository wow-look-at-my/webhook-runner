// Resource graphs: the host in the title bar, one strip per live container on
// the overview. Every number comes from a simple-stats-api instance named by
// /config's stats_url. This module measures nothing itself: it polls that
// API on the API's own sample interval and pushes values into <perf-graph>
// elements (js-snippets, loaded at runtime from the library site).
//
// Honesty rules, because a graph is believed at a glance:
//   - No stats_url: the title bar says so. Nothing draws a flat zero.
//   - The API is unreachable: the strips dim and a badge says "unreachable".
//   - A metric the API does not report (no GPU, no docker socket) keeps its
//     graph empty with a tooltip naming the gap, never a zero trace.

const PERF_GRAPH_URL = 'https://sites.pazer.build/js-snippets/branch/library/ui/perf-graph.js';
/** Samples asked for on the seed fetch: the API's own window is 300 at 1s. */
const SEED_HISTORY = 300;
const RETRY_MS = 5000; // fixed cadence after a failed poll, never grows
const MIN_POLL_MS = 1000;
const FETCH_TIMEOUT_MS = 8000;

/** The subset of <perf-graph> this module drives. */
interface PerfGraphLike extends HTMLElement {
	push(value: number): void;
	clear(): void;
}

interface Metric {
	value: number;
	percent?: number | null;
	history?: number[];
}

interface ContainerMetrics {
	cpu?: { percent?: Metric };
	ram?: { used?: Metric; percent?: Metric };
	network?: { rx_rate?: Metric; tx_rate?: Metric };
	disk?: { io?: { read_rate?: Metric; write_rate?: Metric } };
	gpu?: { utilization?: Metric; vram?: Metric };
}

interface StatsResponse {
	sampling?: { intervalSeconds?: number; maxHistorySize?: number };
	metrics?: {
		cpu?: { percent?: Metric };
		ram?: { percent?: Metric };
		disk?: { percent?: Metric; io?: { read_rate?: Metric; write_rate?: Metric } };
		network?: { rx_rate?: Metric; tx_rate?: Metric };
		gpu?: Array<{ utilization?: Metric }>;
		docker?: { containers?: Metric };
		containers?: Record<string, ContainerMetrics>;
	};
}

/** One gauge: how to read its series from a metrics subtree. */
interface Gauge {
	label: string;
	unit: string;
	min: number | null;
	max: number | null;
	/** Newest-first series, or null when the API does not report it. */
	series: (m: unknown) => number[] | null;
	/** Tooltip when the series is absent. */
	absent: string;
}

/** Newest-first series of one metric: [value, ...history]. */
function seriesOf(m: Metric | undefined): number[] | null {
	if (!m || typeof m.value !== 'number') return null;
	return [m.value, ...(m.history ?? [])];
}

/** Share of the possible range, newest first (the API's `percent` is only the current value). */
function percentSeries(m: Metric | undefined): number[] | null {
	if (!m || typeof m.value !== 'number') return null;
	const cur = typeof m.percent === 'number' ? m.percent : m.value;
	if (m.value === 0) return [cur, ...(m.history ?? []).map(() => 0)];
	const scale = cur / m.value;
	return [cur, ...(m.history ?? []).map((v) => v * scale)];
}

/** Elementwise sum of two newest-first series, scaled; the shorter one bounds the length. */
function sumSeries(a: Metric | undefined, b: Metric | undefined, scale: number): number[] | null {
	const sa = seriesOf(a);
	const sb = seriesOf(b);
	if (!sa && !sb) return null;
	if (!sa) return sb!.map((v) => v * scale);
	if (!sb) return sa.map((v) => v * scale);
	const n = Math.min(sa.length, sb.length);
	const out = new Array<number>(n);
	for (let i = 0; i < n; i++) out[i] = (sa[i] + sb[i]) * scale;
	return out;
}

const MB = 1 / 1e6;
const MIB = 1 / 2 ** 20;

const HOST_GAUGES: Gauge[] = [
	{ label: 'cpu', unit: '%', min: 0, max: 100, absent: 'no cpu figure from the stats source',
		series: (m) => percentSeries((m as StatsResponse['metrics'])?.cpu?.percent) },
	{ label: 'ram', unit: '%', min: 0, max: 100, absent: 'no ram figure from the stats source',
		series: (m) => seriesOf((m as StatsResponse['metrics'])?.ram?.percent) },
	{ label: 'disk', unit: '%', min: 0, max: 100, absent: 'no disk figure from the stats source (Linux only)',
		series: (m) => seriesOf((m as StatsResponse['metrics'])?.disk?.percent) },
	{ label: 'net', unit: 'MB/s', min: 0, max: null, absent: 'no network figure from the stats source',
		series: (m) => { const n = (m as StatsResponse['metrics'])?.network; return sumSeries(n?.rx_rate, n?.tx_rate, MB); } },
	{ label: 'gpu', unit: '%', min: 0, max: 100, absent: 'no GPU reported by the stats source',
		series: (m) => seriesOf((m as StatsResponse['metrics'])?.gpu?.[0]?.utilization) },
];

const CONTAINER_GAUGES: Gauge[] = [
	{ label: 'cpu', unit: '%', min: 0, max: 100, absent: 'no cpu figure for this container',
		series: (m) => percentSeries((m as ContainerMetrics).cpu?.percent) },
	{ label: 'ram', unit: 'MiB', min: 0, max: null, absent: 'no memory figure for this container',
		series: (m) => seriesOf((m as ContainerMetrics).ram?.used)?.map((v) => v * MIB) ?? null },
	{ label: 'disk', unit: 'MB/s', min: 0, max: null, absent: 'no block I/O figure for this container',
		series: (m) => { const io = (m as ContainerMetrics).disk?.io; return sumSeries(io?.read_rate, io?.write_rate, MB); } },
	{ label: 'net', unit: 'MB/s', min: 0, max: null, absent: 'no network figure for this container',
		series: (m) => { const n = (m as ContainerMetrics).network; return sumSeries(n?.rx_rate, n?.tx_rate, MB); } },
	{ label: 'gpu', unit: '%', min: 0, max: 100, absent: 'no GPU work attributed to this container',
		series: (m) => seriesOf((m as ContainerMetrics).gpu?.utilization) },
];

/** A row of gauges bound to one metrics subtree. */
class Strip {
	readonly el: HTMLElement;
	private graphs: Array<{ gauge: Gauge; el: PerfGraphLike; seeded: boolean; present: boolean }> = [];

	constructor(gauges: Gauge[], history: number) {
		this.el = el('div', { class: 'stat-strip' });
		for (const gauge of gauges) {
			const g = document.createElement('perf-graph') as PerfGraphLike;
			g.setAttribute('compact', '');
			g.setAttribute('label', gauge.label);
			g.setAttribute('unit', gauge.unit);
			g.setAttribute('history', String(history));
			if (gauge.min != null) g.setAttribute('min', String(gauge.min));
			if (gauge.max != null) g.setAttribute('max', String(gauge.max));
			this.el.append(g);
			this.graphs.push({ gauge, el: g, seeded: false, present: false });
		}
	}

	/** Feed one API reading. A seed reading carries history and is replayed oldest-first. */
	update(m: unknown, seed: boolean): void {
		for (const g of this.graphs) {
			const s = g.gauge.series(m);
			if (!s) {
				if (g.present || !g.el.title) {
					g.el.clear();
					g.el.title = g.gauge.absent;
					g.el.classList.add('absent');
					g.present = false;
				}
				continue;
			}
			if (!g.present) {
				g.el.title = '';
				g.el.classList.remove('absent');
				g.present = true;
			}
			if (seed || !g.seeded) {
				g.el.clear();
				for (let i = s.length - 1; i >= 0; i--) g.el.push(s[i]);
				g.seeded = true;
			} else {
				g.el.push(s[0]);
			}
		}
	}

	setStale(stale: boolean, why: string): void {
		this.el.classList.toggle('stale', stale);
		if (stale) this.el.title = why;
		else this.el.removeAttribute('title');
	}
}

function el(tag: string, attrs: Record<string, string> | null, ...children: Array<string | Node>): HTMLElement {
	const e = document.createElement(tag);
	if (attrs) for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
	e.append(...children);
	return e;
}

async function fetchJSONBounded<T>(url: string): Promise<T> {
	const ctl = new AbortController();
	const timer = setTimeout(() => ctl.abort(), FETCH_TIMEOUT_MS);
	try {
		const res = await fetch(url, { signal: ctl.signal, headers: { Accept: 'application/json' } });
		if (!res.ok) throw new Error(`HTTP ${res.status}`);
		return (await res.json()) as T;
	} finally {
		clearTimeout(timer);
	}
}

/** Where a container name points: a run's modal, a manager's page, or nowhere. */
function containerLink(name: string): HTMLElement {
	const mgr = name.match(/^webhook-runner-mgr-(.+)$/);
	if (mgr) return el('a', { href: '#manager=' + encodeURIComponent(mgr[1]), class: 'container-name' }, name);
	const run = name.match(/^webhook-runner-([a-z2-7]{26})$/);
	if (run) {
		const a = el('a', { href: '#', class: 'container-name' }, name);
		a.addEventListener('click', (ev) => {
			ev.preventDefault();
			void showRun(run[1]);
		});
		return a;
	}
	return el('span', { class: 'container-name' }, name);
}

class Containers {
	private rows = new Map<string, { row: HTMLElement; strip: Strip }>();
	private section: HTMLElement;
	private list: HTMLElement;
	private note: HTMLElement;

	constructor(private history: number) {
		this.section = document.getElementById('containers-section')!;
		this.list = document.getElementById('containers-list')!;
		this.note = document.getElementById('containers-note')!;
	}

	update(metrics: StatsResponse['metrics'], seed: boolean): void {
		const docker = metrics?.docker?.containers;
		const all = metrics?.containers ?? {};
		if (!docker) {
			// The API is reachable but has no daemon socket: say so, list nothing.
			this.note.textContent = 'The stats source reports no Docker containers (no daemon socket mounted).';
			this.note.hidden = false;
		} else {
			this.note.hidden = true;
		}
		const names = Object.keys(all).sort();
		for (const [name, r] of this.rows) {
			if (!(name in all)) {
				r.row.remove();
				this.rows.delete(name);
			}
		}
		let prev: HTMLElement | null = null;
		for (const name of names) {
			let r = this.rows.get(name);
			if (!r) {
				const strip = new Strip(CONTAINER_GAUGES, this.history);
				const row = el('div', { class: 'container-row' }, containerLink(name), strip.el);
				r = { row, strip };
				this.rows.set(name, r);
			}
			// Keep DOM order sorted without rebuilding rows that exist.
			if (prev ? prev.nextElementSibling !== r.row : this.list.firstElementChild !== r.row) {
				if (prev) prev.after(r.row);
				else this.list.prepend(r.row);
			}
			r.strip.update(all[name], seed);
			prev = r.row;
		}
		const empty = document.getElementById('containers-empty')!;
		empty.hidden = !docker || names.length > 0;
	}

	setStale(stale: boolean, why: string): void {
		for (const r of this.rows.values()) r.strip.setStale(stale, why);
	}
}

/** Show the title-bar note (no source, unreachable) or clear it. */
function setNote(text: string, cls: string): void {
	const n = document.getElementById('stats-note')!;
	n.textContent = text;
	n.className = 'badge ' + cls;
	n.hidden = text === '';
}

async function pollForever(base: string, host: Strip, containers: Containers): Promise<void> {
	let seed = true;
	let intervalMs = MIN_POLL_MS;
	for (;;) {
		const url = `${base}/api/v1/stats.json?history=${seed ? SEED_HISTORY : 0}`;
		try {
			const r = await fetchJSONBounded<StatsResponse>(url);
			host.update(r.metrics, seed);
			containers.update(r.metrics, seed);
			host.setStale(false, '');
			containers.setStale(false, '');
			setNote('', '');
			const iv = r.sampling?.intervalSeconds;
			if (typeof iv === 'number' && iv > 0) intervalMs = Math.max(MIN_POLL_MS, iv * 1000);
			seed = false;
		} catch (e) {
			const why = `stats source unreachable: ${base} (${e instanceof Error ? e.message : String(e)})`;
			console.error(why);
			host.setStale(true, why);
			containers.setStale(true, why);
			setNote('stats unreachable', 'bad');
			seed = true; // resync the history once it is back
			await new Promise((r) => setTimeout(r, RETRY_MS));
			continue;
		}
		await new Promise((r) => setTimeout(r, intervalMs));
	}
}

/**
 * Boot the graphs. `load` is timeline.ts's never-give-up module loader,
 * so this file shares its retry policy instead of owning a second one.
 */
export async function bootStats(load: (url: string, name: string) => Promise<void>): Promise<void> {
	let cfg: { stats_url?: string };
	try {
		cfg = await fetchJSONBounded<{ stats_url?: string }>('/config');
	} catch (e) {
		console.error('stats: /config failed:', e);
		setNote('stats: config unavailable', 'warn');
		return;
	}
	const base = (cfg.stats_url ?? '').replace(/\/$/, '');
	if (base === '') {
		setNote('no stats source (WEBHOOK_RUNNER_STATS_URL unset)', 'warn');
		document.getElementById('containers-section')!.hidden = true;
		return;
	}
	setNote('stats loading…', 'warn');
	await load(PERF_GRAPH_URL, 'perf-graph');
	const host = new Strip(HOST_GAUGES, SEED_HISTORY);
	document.getElementById('host-stats')!.replaceChildren(host.el);
	const containers = new Containers(SEED_HISTORY);
	void pollForever(base, host, containers);
}
