// Delta-coalescing harness for the timeline adapter: evaluates the REAL
// assets/timeline.js in a vm sandbox (stub <timeline-view>, stub EventSource,
// recorded setData/mergeData calls) and proves the burst-batching contract
// that fixes the 2026-07-21 freeze:
//
//   A backlog of SSE `run` deltas — buffered while the tab sat backgrounded
//   for hours, then flushed all at once on wake (a captured profile showed
//   6,335 deltas in one 15.3s block, 0 repaints) — used to run one full
//   component mergeData PER delta, synchronously, on the SSE message handler.
//   The adapter now:
//     1. does ZERO chart work on the `run` handler itself (the merge is
//        deferred to requestAnimationFrame — so the event loop is never
//        blocked by the burst; rAF is parked while backgrounded, so the
//        whole backlog collapses into ONE flush on foreground), and
//     2. coalesces the frame's deltas into ONE mergeData, deduped by run id
//        (a run that changed N times in the burst is one interval, last
//        state wins).
//
// Run: node --experimental-strip-types --test internal/server/dashboard/testjs/*.test.ts
// No dependencies; node's built-in test runner.

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
// One dynamic import (the component's Pages URL); rewrite it so vm.Script can
// evaluate the otherwise import/export-free bundle. (See timeline-coverage.)
const scriptSrc = bundleSrc.replace(/\bimport\(/g, '__import(');
assert.notEqual(scriptSrc, bundleSrc, 'bundle lost its dynamic component import — update this harness');

function makeSandbox() {
	const calls = []; // recorded { kind: 'setData'|'mergeData', data } on the stub element
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

	const sources = []; // EventSource instances the module opened
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
		setInterval: () => 0, // the forever supervisor would pin the event loop
		clearInterval: () => {},
		// Route rAF through setImmediate so settle()'s setImmediate loop drains
		// the deferred flush deterministically (a real setTimeout(0)=1ms timer
		// does not reliably fire inside settle()).
		requestAnimationFrame: (fn) => {
			setImmediate(fn);
			return 1;
		},
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
		__import: async () => ({}), // the component module — element is stubbed
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

const mkid = (i) => `delta${i}`.padEnd(26, 'a').slice(0, 26);

test('a burst of run deltas does ZERO work on the handler and coalesces to ONE merge (the 15s-freeze fix)', async () => {
	const h = await bootSeeded([run('seedseedseedseedseedseedse', 'pr-minder', Date.now() - 60_000, 'success', Date.now() - 50_000)]);
	const before = h.calls.length;

	// A backlog of distinct RUNNING deltas emitted back-to-back with NO
	// intervening frame — exactly the buffered-flush-on-wake shape. Running
	// (not pending) so no lane collapses; each is its own span.
	const N = 250;
	for (let i = 0; i < N; i++) {
		h.sources[0].emit('run', run(mkid(i), 'required-builds', Date.now(), 'running'));
	}
	// THE freeze fix: the SSE handler must return having built ZERO interval
	// merges — the whole burst is deferred to the frame, so the event loop is
	// never blocked (a throttled coverage-only keepalive is cheap and allowed).
	assert.equal(
		h.calls.slice(before).filter((c) => c.data.intervals).length,
		0,
		'the run handler merged intervals synchronously — the burst was not deferred (this is the 15s freeze)',
	);

	await settle(); // the single animation frame fires

	const merges = h.calls.slice(before).filter((c) => c.kind === 'mergeData' && c.data.intervals);
	assert.equal(merges.length, 1, `a burst of ${N} deltas must coalesce into ONE merge, got ${merges.length}`);
	const ids = new Set(merges[0].data.intervals.map((iv) => iv.id));
	for (let i = 0; i < N; i++) {
		assert.ok(ids.has(mkid(i)), `run ${mkid(i)} missing from the coalesced merge`);
	}
	// The batched merge still vouches trailing coverage (the live-stream hatch
	// contract — see timeline-coverage.test.ts).
	assert.ok(merges[0].data.coverage, 'the coalesced merge dropped its coverage claim');
	assert.ok(Math.abs(merges[0].data.coverage.end - Date.now()) < 5_000, 'coalesced-merge coverage must end ~now');
});

test('repeated deltas for one run in a burst merge once — last state wins', async () => {
	const h = await bootSeeded([run('seedseedseedseedseedseedse', 'pr-minder', Date.now() - 60_000, 'success', Date.now() - 50_000)]);
	const before = h.calls.length;

	const ID = 'churnchurnchurnchurnchurnc';
	h.sources[0].emit('run', run(ID, 'gha-runner', Date.now(), 'running'));
	h.sources[0].emit('run', run(ID, 'gha-runner', Date.now(), 'running'));
	h.sources[0].emit('run', run(ID, 'gha-runner', Date.now() - 1_000, 'success', Date.now()));
	await settle();

	const merges = h.calls.slice(before).filter((c) => c.kind === 'mergeData' && c.data.intervals);
	assert.equal(merges.length, 1, 'three deltas for one run must coalesce into one merge');
	const forRun = merges[0].data.intervals.filter((iv) => iv.id === ID);
	assert.equal(forRun.length, 1, 'the churning run appears exactly once in the coalesced merge');
	assert.ok(forRun[0].end != null, 'the merged interval reflects the terminal (last-seen) state, not an earlier running one');
});

test('fan-out: one whr:run-delta per changed run fires from the flush (deduped), not per raw delta', async () => {
	const h = await bootSeeded([run('seedseedseedseedseedseedse', 'pr-minder', Date.now() - 60_000, 'success', Date.now() - 50_000)]);

	const seen = [];
	h.sandbox.window.addEventListener('whr:run-delta', (e) => seen.push(e.detail.id));

	const ID = 'churnchurnchurnchurnchurnc';
	// Two distinct runs, one of them changing twice in the same burst.
	h.sources[0].emit('run', run(ID, 'gha-runner', Date.now(), 'running'));
	h.sources[0].emit('run', run(ID, 'gha-runner', Date.now() - 1_000, 'success', Date.now()));
	h.sources[0].emit('run', run('otherotherotherotherotherz', 'gha-runner', Date.now(), 'running'));
	assert.equal(seen.length, 0, 'the fan-out fired synchronously on the handler — it must ride the flush');

	await settle();
	assert.deepEqual([...seen].sort(), [ID, 'otherotherotherotherotherz'].sort(), 'expected one deduped fan-out event per changed run');
});

test('the fan-out survives a component that never loads (the feed is not the chart)', async () => {
	// The <timeline-view> component is imported at RUNTIME from js-snippets;
	// that fetch can fail for a while. The feed's OTHER consumers — the runs
	// table and the open run modal — are this module's contract and must not
	// go dark with it. (Pre-fix, flushDeltas returned early on a null chart,
	// swallowing every event AND growing pendingDeltas without bound.)
	const h = makeSandbox();
	// A component fetch that never settles: the load parks (as it does while
	// the site is unreachable) and `chart` stays null for the whole test.
	h.sandbox.__import = () => new Promise(() => {});
	new vm.Script(scriptSrc, { filename: 'timeline.js' }).runInContext(h.sandbox);
	await settle();
	assert.equal(h.sources.length, 1, 'the feed must open its stream without waiting on the component');

	const seen = [];
	h.sandbox.window.addEventListener('whr:run-delta', (e) => seen.push(e.detail.id));
	const ID = 'nochartnochartnochartnocha';
	h.sources[0].emit('run', run(ID, 'gha-runner', Date.now(), 'running'));
	await settle();

	assert.deepEqual(seen, [ID], 'deltas must still fan out with no chart attached');
	assert.equal(
		h.calls.filter((c) => c.kind === 'mergeData').length,
		0,
		'…and no chart work may be attempted while the component is missing',
	);
});
