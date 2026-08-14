// hb truth-reconcile harness for the timeline adapter: evaluates the REAL
// assets/timeline.js in a vm sandbox (stub <timeline-view>, stub
// EventSource, recorded setData/mergeData calls, recorded fetches) and
// proves the positively-recovering contract:
//
//   1. A local non-terminal run ABSENT from an hb {"active":[...]} payload
//      is dropped on that beat — one setData rebuild, no cap, no per-run
//      probes (mode-2 zombie kill: missed terminal deltas / restarts).
//   2. An active id this page never saw triggers ONE bounded truth fetch
//      (/runs/{id} for a single miss, /runs?live=1 for several) and the
//      span appears (mode-1 heal: missed starts / out-of-window spans).
//   3. A legacy server's hb `{}` (no active array) changes nothing —
//      feature detection keeps old-server behavior byte-identical.
//   4. Coverage claims still ride the reconcile paths (the 2026-07-15
//      trailing-hatch contract — see timeline-coverage.test.ts).
//
// Run: node --experimental-strip-types --test internal/server/dashboard/testjs/*.test.ts

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
	const calls = []; // recorded { kind: 'setData'|'mergeData', data }
	const fetched = []; // every URL the module fetched
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

	// Per-test extra routes (exact-prefix match) checked before the defaults.
	const extraRoutes = new Map();
	const routes = (url) => {
		for (const [prefix, body] of extraRoutes) {
			if (url.startsWith(prefix)) return body;
		}
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
		requestAnimationFrame: (fn) => { setImmediate(fn); return 1; }, // flush drains via settle()'s setImmediate loop
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
			fetched.push(url);
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
	return { sandbox, calls, fetched, sources, chartEl, extraRoutes };
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

/** interval ids of the LAST call of the given kind. */
function lastIntervalIds(calls, kind) {
	const of = calls.filter((c) => c.kind === kind && c.data.intervals);
	assert.ok(of.length > 0, `no ${kind} call carrying intervals`);
	return of[of.length - 1].data.intervals.map((i) => i.id);
}

test('a local live run absent from the hb active set is dropped on that beat', async () => {
	const t = Date.now();
	const h = await bootSeeded([
		run('zombiezombiezombiezombiezz', 'h', t - 120_000, 'running'),
		run('finefinefinefinefinefinef', 'h', t - 60_000, 'running'),
		run('dddddddddddddddddddddddddd', 'h', t - 90_000, 'success', t - 80_000),
	]);
	const before = h.calls.length;

	// The server's truth beat: only "fine..." is active — the zombie's
	// terminal delta was missed (or the server restarted). No probes.
	h.sources[0].emit('hb', { active: ['finefinefinefinefinefinef'] });
	await settle();

	const ids = lastIntervalIds(h.calls, 'setData');
	assert.ok(!ids.includes('zombiezombiezombiezombiezz'), 'zombie span survived the truth beat');
	assert.ok(ids.includes('finefinefinefinefinefinef'), 'the genuinely-active run must survive');
	assert.ok(ids.includes('dddddddddddddddddddddddddd'), 'terminal history is untouched by the active diff');
	// The rebuild claims trailing coverage through now (contract 4).
	const rebuild = h.calls.slice(before).find((c) => c.kind === 'setData');
	assert.ok(rebuild.data.coverage, 'the reconcile rebuild must carry a coverage claim');
	assert.ok(Math.abs(rebuild.data.coverage.end - Date.now()) < 5_000, 'rebuild coverage must end ~now');
	assert.equal(h.fetched.filter((u) => u.startsWith('/runs/')).length, 0, 'drops must not probe per run');
});

test('an unknown single active id is truth-fetched via /runs/{id} and appears', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('knownknownknownknownknownk', 'h', t - 60_000, 'running')]);
	h.extraRoutes.set(
		'/runs/misssedmisssedmisssedmisss?tail=0',
		run('misssedmisssedmisssedmisss', 'jit', t - 300_000, 'running'),
	);

	h.sources[0].emit('hb', {
		active: ['knownknownknownknownknownk', 'misssedmisssedmisssedmisss'],
	});
	await settle();

	assert.ok(
		h.fetched.some((u) => u === '/runs/misssedmisssedmisssedmisss?tail=0'),
		'a single missing id must fetch /runs/{id}',
	);
	const merged = h.calls.filter((c) => c.kind === 'mergeData' && c.data.intervals).flatMap((c) => c.data.intervals);
	assert.ok(
		merged.some((i) => i.id === 'misssedmisssedmisssedmisss'),
		'the missed span must appear after the truth fetch',
	);
});

test('several unknown active ids are truth-fetched via one /runs?live=1', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('knownknownknownknownknownk', 'h', t - 60_000, 'running')]);
	h.extraRoutes.set('/runs?live=1', [
		run('knownknownknownknownknownk', 'h', t - 60_000, 'running'),
		run('lostoneaaaaaaaaaaaaaaaaaaa', 'jit', t - 200_000, 'running'),
		run('losttwobbbbbbbbbbbbbbbbbbb', 'jit', t - 100_000, 'running'),
	]);

	h.sources[0].emit('hb', {
		active: ['knownknownknownknownknownk', 'lostoneaaaaaaaaaaaaaaaaaaa', 'losttwobbbbbbbbbbbbbbbbbbb'],
	});
	await settle();

	assert.equal(h.fetched.filter((u) => u === '/runs?live=1').length, 1, 'several misses = ONE live=1 fetch');
	assert.equal(h.fetched.filter((u) => u.startsWith('/runs/')).length, 0, 'no per-run probes');
	const merged = h.calls.filter((c) => c.data.intervals).flatMap((c) => c.data.intervals);
	assert.ok(merged.some((i) => i.id === 'lostoneaaaaaaaaaaaaaaaaaaa'));
	assert.ok(merged.some((i) => i.id === 'losttwobbbbbbbbbbbbbbbbbbb'));
});

test('a legacy hb {} (no active array) changes nothing — feature detection', async () => {
	const t = Date.now();
	const h = await bootSeeded([run('zombiezombiezombiezombiezz', 'h', t - 120_000, 'running')]);
	const before = h.calls.length;
	const fetchedBefore = h.fetched.length;

	h.sources[0].emit('hb', {}); // an old server's heartbeat payload
	await settle();

	// No new calls = no drop happened (a drop always rebuilds via setData)
	// and no truth fetch was attempted: absence can only be ruled by a REAL
	// active array, never by a legacy heartbeat.
	assert.equal(h.calls.length, before, 'a payload-less heartbeat must not rebuild or merge');
	assert.equal(h.fetched.length, fetchedBefore, 'a payload-less heartbeat must not fetch');
});

test('an empty-but-real active set drops every local live run', async () => {
	const t = Date.now();
	const h = await bootSeeded([
		run('zombiezombiezombiezombiezz', 'h', t - 120_000, 'running'),
		run('dddddddddddddddddddddddddd', 'h', t - 90_000, 'success', t - 80_000),
	]);

	h.sources[0].emit('hb', { active: [] }); // "nothing is active" — a real verdict
	await settle();

	const ids = lastIntervalIds(h.calls, 'setData');
	assert.ok(!ids.includes('zombiezombiezombiezombiezz'), 'the [] verdict must retire live locals');
	assert.ok(ids.includes('dddddddddddddddddddddddddd'), 'terminal history stays');
});
