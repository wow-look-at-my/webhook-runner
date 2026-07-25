// Trailing-coverage regression harness for the timeline adapter: evaluates
// the REAL assets/timeline.js in a vm sandbox (stub <timeline-view>, stub
// EventSource, recorded setData/mergeData calls) and proves the
// stream-vouched trailing-coverage contract:
//
//   1. The seed page claims coverage ending ~now (the baseline).
//   2. EVERY live run delta's merge carries a coverage claim ending ~now —
//      the 2026-07-15 regression: #73 deleted the skip-driven rebuilds that
//      incidentally kept re-registering coverage, so nothing extended the
//      trailing edge on a live stream and the component hatched
//      [connect snapshot, now] as unknown history OVER live bars.
//   3. hb keepalives extend coverage on a quiet stream (throttled ≥1s).
//   4. Claims are CONTIGUOUS: each starts at (or before) the previous end,
//      so the tracker merges them into one range — no seam gaps to hatch.
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

// The bundle is an ES module only by virtue of ONE dynamic import (the
// component's Pages URL — external at build time). Rewriting `import(` to a
// sandbox-provided `__import(` lets vm.Script evaluate the otherwise
// import/export-free source without --experimental-vm-modules, and makes
// the component load resolve instantly (the element itself is stubbed).
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
		// The feed supervisor arms a forever setInterval; a REAL one would
		// keep the node event loop (and the test run) alive. Never fires —
		// these tests drive the stream directly.
		setInterval: () => 0,
		clearInterval: () => {},
		requestAnimationFrame: (fn) => { setImmediate(fn); return 1; }, // flush drains via settle()'s setImmediate loop
		localStorage: { getItem: () => null, setItem: () => {} },
		// Page globals dashboard.js provides at runtime (ts/globals.d.ts):
		// tsPresent gates every interval build; the rest are render/click
		// helpers these tests never hit but the module may reach for.
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

/** Boot the real bundle and drive it to the seeded state (connect snapshot applied). */
async function bootSeeded(seedPage) {
	const h = makeSandbox();
	new vm.Script(scriptSrc, { filename: 'timeline.js' }).runInContext(h.sandbox);
	await settle(); // boot(): seed fetch (empty), stream open, component "load"
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

const claimsOf = (calls) => calls.filter((c) => c.data.coverage).map((c) => c.data.coverage);

test('seed page claims trailing coverage through now', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('aaaa', 'pr-minder', t - 60_000, 'success', t - 50_000)]);
	const claims = claimsOf(h.calls);
	assert.ok(claims.length >= 1, 'seed produced no coverage claim');
	const last = claims[claims.length - 1];
	assert.ok(Math.abs(last.end - Date.now()) < 5_000, `seed claim end ${last.end} is not ~now`);
});

test('every live run delta extends trailing coverage to now (the full-window-hatch regression)', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('aaaa', 'pr-minder', t - 60_000, 'success', t - 50_000)]);
	const before = h.calls.length;

	// A burst of deltas — running, terminal, and crucially a SKIPPED run
	// (pre-#73 the skip path was the accidental coverage refresher; post-#73
	// it is an ordinary delta and must carry the claim like the rest).
	h.sources[0].emit('run', run('bbbb', 'required-builds', Date.now(), 'running'));
	await settle();
	h.sources[0].emit('run', run('bbbb', 'required-builds', Date.now() - 5_000, 'success', Date.now()));
	await settle();
	h.sources[0].emit('run', run('cccc', 'required-builds', Date.now(), 'skipped', Date.now()));
	await settle();

	const deltaMerges = h.calls.slice(before).filter((c) => c.kind === 'mergeData' && c.data.intervals);
	assert.equal(deltaMerges.length, 3, 'expected one interval merge per delta');
	for (const m of deltaMerges) {
		assert.ok(m.data.coverage, 'a live delta merged intervals WITHOUT a coverage claim — the trailing hatch regression');
		assert.ok(Math.abs(m.data.coverage.end - Date.now()) < 5_000, 'delta coverage claim does not end ~now');
	}
});

test('hb keepalives extend trailing coverage on a quiet stream (throttled)', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('aaaa', 'pr-minder', t - 60_000, 'success', t - 50_000)]);
	const before = h.calls.length;

	h.sources[0].emit('hb'); // immediately after the seed claim: throttled, no new claim
	await settle();
	assert.equal(claimsOf(h.calls.slice(before)).length, 0, 'hb claim was not throttled');

	await new Promise((r) => setTimeout(r, 1_100)); // pass LIVE_COVERAGE_MIN_STEP_MS
	h.sources[0].emit('hb');
	await settle();
	const claims = claimsOf(h.calls.slice(before));
	assert.equal(claims.length, 1, 'a quiet-stream hb after the throttle window must claim coverage');
	assert.ok(Math.abs(claims[0].end - Date.now()) < 5_000, 'hb coverage claim does not end ~now');
});

test('claims are contiguous — no seam gap for the component to hatch', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('aaaa', 'pr-minder', t - 60_000, 'success', t - 50_000)]);

	h.sources[0].emit('run', run('bbbb', 'state-demo', Date.now(), 'running'));
	await settle();
	await new Promise((r) => setTimeout(r, 1_100));
	h.sources[0].emit('hb');
	await settle();
	h.sources[0].emit('run', run('bbbb', 'state-demo', Date.now() - 1_000, 'success', Date.now()));
	await settle();

	const claims = claimsOf(h.calls);
	assert.ok(claims.length >= 3, `expected >= 3 claims, got ${claims.length}`);
	for (let i = 1; i < claims.length; i++) {
		assert.ok(
			claims[i].start <= claims[i - 1].end,
			`claim ${i} starts at ${claims[i].start}, after the previous end ${claims[i - 1].end} — seam gap`,
		);
	}
});
