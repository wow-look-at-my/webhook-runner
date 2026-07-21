// Pending-backlog collapse harness: evaluates the REAL assets/timeline.js
// in the vm sandbox (see timeline-coverage.test.mjs for the pattern) and
// pins the ×N queued view model:
//
//   1. M pending runs in one lane feed EXACTLY ONE synthetic aggregate
//      interval ("queued:<lane>", label "×M queued") — the individual
//      pending spans are withheld while collapsed; executing/terminal runs
//      keep their own spans.
//   2. A pending run leaving the backlog (pending → running) appears
//      individually and the aggregate restamps to ×(M-1).
//   3. Draining below 2 crosses the collapse boundary: the aggregate
//      disappears (full setData replace) and the survivor renders as its
//      real span. (N=0 is the same boundary from 1.)
//
// Run: node --test internal/server/dashboard/testjs/*.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';

const bundleSrc = readFileSync(
	path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'assets', 'timeline.js'),
	'utf8',
);
const scriptSrc = bundleSrc.replace(/\bimport\(/g, '__import(');
assert.notEqual(scriptSrc, bundleSrc, 'bundle lost its dynamic component import — update this harness');

function makeSandbox() {
	const calls = [];
	const chartEl = {
		id: 'runs-timeline',
		loadRange: null,
		listeners: new Map(),
		setData(data) {
			calls.push({ kind: 'setData', data });
		},
		mergeData(data) {
			calls.push({ kind: 'mergeData', data });
		},
		setLanes() {},
		setViewport() {},
		setAttribute() {},
		getAttribute() {
			return null;
		},
		markFresh() {},
		addEventListener(type, fn) {
			this.listeners.set(type, fn);
		},
		before() {},
	};

	const sources = [];
	class StubEventSource {
		static CONNECTING = 0;
		static OPEN = 1;
		static CLOSED = 2;
		constructor(url) {
			this.url = url;
			this.readyState = StubEventSource.OPEN;
			this.onopen = null;
			this.onerror = null;
			this.handlers = new Map();
			sources.push(this);
		}
		addEventListener(type, fn) {
			this.handlers.set(type, fn);
		}
		emit(type, data) {
			const fn = this.handlers.get(type);
			if (fn) fn({ data: JSON.stringify(data) });
		}
		close() {
			this.readyState = StubEventSource.CLOSED;
		}
	}

	const routes = (url) => {
		if (url.startsWith('/runs?max=')) return [];
		if (url.startsWith('/config')) return { run_retention: '48h' };
		return null;
	};

	const winTarget = new EventTarget();
	const sandbox = {
		console,
		Date,
		JSON,
		Math,
		Promise,
		Set,
		Map,
		Number,
		Infinity,
		Error,
		encodeURIComponent,
		CustomEvent,
		EventTarget,
		AbortSignal,
		setTimeout,
		clearTimeout,
		setInterval: () => 0,
		clearInterval: () => {},
		requestAnimationFrame: (fn) => setTimeout(fn, 0),
		localStorage: { getItem: () => null, setItem: () => {} },
		tsPresent: (s) => typeof s === 'string' && s !== '' && !s.startsWith('0001-01-01'),
		fmtTime: (s) => s ?? '',
		fmtDuration: () => '',
		runWaited: () => '',
		runDuration: () => '',
		el: () => ({ setAttribute() {}, append() {}, appendChild() {}, remove() {}, textContent: '' }),
		showRun: async () => {},
		currentHookId: () => null,
		fetchJSON: async () => {
			throw new Error('fetchJSON unused by timeline.js');
		},
		EventSource: StubEventSource,
		__import: async () => ({}),
		fetch: async (url) => {
			const body = routes(url);
			return {
				ok: body !== null,
				status: body !== null ? 200 : 404,
				json: async () => body,
			};
		},
		document: {
			getElementById: (id) => (id === 'runs-timeline' ? chartEl : null),
			createElement: () => ({ setAttribute() {}, append() {}, appendChild() {}, textContent: '' }),
			createDocumentFragment: () => ({ appendChild() {} }),
			createTextNode: (t) => ({ textContent: t }),
			addEventListener: () => {},
		},
	};
	sandbox.window = new Proxy(winTarget, {
		get(t, prop) {
			const v = t[prop];
			if (typeof v === 'function') return v.bind(t);
			return v;
		},
		set(t, prop, v) {
			t[prop] = v;
			return true;
		},
	});
	vm.createContext(sandbox);
	return { sandbox, calls, sources, chartEl };
}

const settle = async () => {
	for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
};

async function bootSeeded(seedPage) {
	const h = makeSandbox();
	new vm.Script(scriptSrc, { filename: 'timeline.js' }).runInContext(h.sandbox);
	await settle();
	assert.equal(h.sources.length, 1, 'expected the module to open one EventSource');
	h.sources[0].emit('snapshot', seedPage);
	await settle();
	return h;
}

const run = (id, hook, startedMs, status, finishedMs) => ({
	id,
	hook_id: hook,
	started: new Date(startedMs).toISOString(),
	finished: finishedMs === undefined ? '0001-01-01T00:00:00Z' : new Date(finishedMs).toISOString(),
	status,
	exit_code: 0,
});

function lastCallWithIntervals(calls, kind) {
	const of = calls.filter((c) => c.kind === kind && c.data.intervals);
	assert.ok(of.length > 0, `no ${kind} call carrying intervals`);
	return of[of.length - 1];
}

const AGG = 'queued:gha-runner';
const t0 = Date.now();
const p1 = run('p1p1p1p1p1p1p1p1p1p1p1p1p1', 'gha-runner', t0 - 90_000, 'pending');
const p2 = run('p2p2p2p2p2p2p2p2p2p2p2p2p2', 'gha-runner', t0 - 80_000, 'pending');
const p3 = run('p3p3p3p3p3p3p3p3p3p3p3p3p3', 'gha-runner', t0 - 70_000, 'pending');
const exec = { ...run('rrrrrrrrrrrrrrrrrrrrrrrrrr', 'gha-runner', t0 - 60_000, 'running'), started_at: new Date(t0 - 59_000).toISOString() };

test('a ~170-deep pending flood feeds ONE aggregate row, not a wall of sub-tracks', async () => {
	// The production shape that filled the whole view: one lane, ~170
	// pending runs, each on its own packing sub-track. The no-wall property
	// is asserted DIRECTLY on what the component is fed: the lane
	// contributes exactly its executing spans + ONE ×170 aggregate.
	const flood = [];
	for (let i = 0; i < 170; i++) {
		flood.push(run(`w${String(i).padStart(25, '0')}`, 'gha-runner', t0 - 500_000 + i * 1_000, 'pending'));
	}
	const h = await bootSeeded([...flood, exec]);

	const seed = lastCallWithIntervals(h.calls, 'setData');
	const laneIntervals = seed.data.intervals.filter((i) => i.laneId === 'gha-runner');
	assert.equal(laneIntervals.length, 2, 'the flooded lane feeds exactly [1 running span, 1 aggregate]');
	const agg = laneIntervals.find((i) => i.id === AGG);
	assert.ok(agg, 'the backlog aggregate must exist');
	// The badge must be UNMISTAKABLE: the count AND the word "waiting" —
	// a regression to a cryptic bare bar fails here.
	assert.equal(agg.label, '×170 waiting', 'the badge carries the full backlog depth');
	assert.ok(/×170/.test(agg.label) && /waiting/.test(agg.label), 'count + the word "waiting", always');
	assert.ok(Array.isArray(agg.labelTiers) && agg.labelTiers[0] === '×170 waiting for a slot',
		'the widest tier spells the meaning out in full');
	assert.equal(agg.labelTiers[agg.labelTiers.length - 1], '×170', 'the narrowest tier is still the count');
	assert.ok(laneIntervals.some((i) => i.id === exec.id), 'the executing run keeps its own span');
});

test('a pending backlog collapses into ONE ×N aggregate span', async () => {
	const h = await bootSeeded([p1, p2, p3, exec]);

	const seed = lastCallWithIntervals(h.calls, 'setData');
	const aggs = seed.data.intervals.filter((i) => i.id === AGG);
	assert.equal(aggs.length, 1, 'exactly one aggregate per collapsed lane');
	assert.equal(aggs[0].label, '×3 waiting', 'the badge carries the backlog depth');
	assert.ok(/×3/.test(aggs[0].label) && /waiting/.test(aggs[0].label), 'count + the word "waiting", always');
	assert.equal(aggs[0].end, null, 'the backlog rides the live edge');
	assert.equal(aggs[0].start, Date.parse(p1.started), 'the aggregate starts at the earliest queued run');
	const ids = seed.data.intervals.map((i) => i.id);
	for (const p of [p1, p2, p3]) {
		assert.ok(!ids.includes(p.id), `collapsed pending run ${p.id} must not feed individually`);
	}
	assert.ok(ids.includes(exec.id), 'the executing run keeps its individual span');
});

test('leaving the backlog re-individualizes the run and restamps the badge', async () => {
	const h = await bootSeeded([p1, p2, p3, exec]);
	const before = h.calls.length;

	// p1 starts executing: 3 → 2 pending, the lane stays collapsed.
	h.sources[0].emit('run', {
		...p1,
		status: 'running',
		started_at: new Date(Date.now()).toISOString(),
	});
	await settle();

	const merge = lastCallWithIntervals(h.calls.slice(before), 'mergeData');
	const ids = merge.data.intervals.map((i) => i.id);
	assert.ok(ids.includes(p1.id), 'the now-running span appears individually');
	const agg = merge.data.intervals.find((i) => i.id === AGG);
	assert.ok(agg, 'the aggregate restamps in the same merge');
	assert.equal(agg.label, '×2 waiting', 'the badge decrements with the backlog');
	assert.ok(/waiting/.test(agg.label), 'the restamped badge keeps the spelled-out meaning');
});

test('draining below 2 retires the aggregate and restores real spans', async () => {
	const h = await bootSeeded([p2, p3, exec]); // depth 2: collapsed

	// p2 finishes while queued (e.g. cancelled → here: skipped shape): the
	// backlog drops to 1 — a collapse-boundary crossing, so a full setData
	// replaces the aggregate with the survivor's real span.
	h.sources[0].emit('run', { ...p2, status: 'cancelled', finished: new Date().toISOString() });
	await settle();

	const rebuilt = lastCallWithIntervals(h.calls, 'setData');
	const ids = rebuilt.data.intervals.map((i) => i.id);
	assert.ok(!ids.includes(AGG), 'the aggregate disappears below depth 2');
	assert.ok(ids.includes(p3.id), 'the single remaining pending run renders as itself');
	assert.ok(ids.includes(p2.id), 'the terminal run renders as itself');
	assert.ok(rebuilt.data.coverage, 'the boundary rebuild still claims coverage');
	assert.ok(Math.abs(rebuilt.data.coverage.end - Date.now()) < 5_000, 'rebuild coverage ends ~now');

	// And to zero: the survivor going terminal is an ordinary delta now.
	h.sources[0].emit('run', { ...p3, status: 'success', finished: new Date().toISOString() });
	await settle();
	const merged = lastCallWithIntervals(h.calls, 'mergeData');
	assert.ok(
		merged.data.intervals.some((i) => i.id === p3.id),
		'the drained lane keeps updating through plain merges',
	);
});
