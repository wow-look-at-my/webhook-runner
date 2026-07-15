// Client harness for the dashboard's push-first section feed: evaluates the
// REAL assets/dashboard.js inside a vm sandbox (stub DOM, recorded fetches,
// fake timers) and drives it exactly like timeline.js does — via
// window.whrStreamLive and the whr:* CustomEvents — to prove:
//
//   1. /hooks is fetched exactly ONCE at page load (the double-fetch fix).
//   2. An idle dashboard with the stream LIVE makes ZERO requests (60s).
//   3. A changed{section} signal refetches exactly that section, once,
//      and signal bursts coalesce (leading edge + one trailing pass).
//   4. With the stream DOWN, the fixed-cadence fallback poll takes over —
//      and it never grows and never gives up.
//   5. Stream recovery does ONE full resync, then goes silent again.
//
// Run: node --test internal/server/dashboard/testjs/*.test.mjs
// No dependencies; node's built-in test runner.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';

const dashboardSrc = readFileSync(
	path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'assets', 'dashboard.js'),
	'utf8',
);

// -- Stub DOM -------------------------------------------------------------------

/** A permissive element stub: real data properties where code branches on
 * them, name-aware method fallbacks everywhere else. */
function makeElement(id) {
	const target = {
		id: id ?? '',
		hidden: false,
		open: false,
		textContent: '',
		innerHTML: '',
		className: '',
		title: '',
		value: '',
		scrollTop: 0,
		dataset: {},
		style: {},
		children: [],
		classList: { toggle() {}, add() {}, remove() {}, contains: () => false },
	};
	return new Proxy(target, {
		get(t, prop) {
			if (prop in t) return t[prop];
			switch (prop) {
				case 'querySelectorAll':
					return () => [];
				case 'querySelector':
					return () => makeElement();
				case 'closest':
					return () => null;
				case 'appendChild':
				case 'before':
				case 'after':
					return (x) => x;
				case 'getContext':
					return () => null;
				default:
					// Unknown method (addEventListener, replaceChildren,
					// setAttribute, remove, close, ...): swallow.
					return () => undefined;
			}
		},
		set(t, prop, v) {
			t[prop] = v;
			return true;
		},
	});
}

// -- The sandbox ------------------------------------------------------------------

function makeSandbox() {
	const requests = []; // { url, t }
	const elements = new Map(); // id -> element (stable identity)
	const winTarget = new EventTarget();
	const docTarget = new EventTarget();

	// Fake clock + timers (setTimeout/setInterval), fired in time order.
	let now = 1_000_000;
	let nextTimerID = 1;
	const timers = new Map(); // id -> { at, fn, every | undefined }
	async function settle() {
		// Let pending fetch/promise chains run to completion.
		for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
	}
	async function advance(ms) {
		const deadline = now + ms;
		for (;;) {
			let due = null;
			for (const [id, tm] of timers) {
				if (tm.at <= deadline && (due === null || tm.at < due.tm.at)) due = { id, tm };
			}
			if (due === null) break;
			now = Math.max(now, due.tm.at);
			if (due.tm.every !== undefined) due.tm.at = now + due.tm.every;
			else timers.delete(due.id);
			try {
				due.tm.fn();
			} catch {
				/* handler errors are the page's own problem */
			}
			await settle();
		}
		now = deadline;
		await settle();
	}

	// Canned responses; failure mode switchable per test.
	let serverUp = true;
	const routes = (url) => {
		if (url.startsWith('/health')) return { status: 'ok', version: 'test' };
		if (url.startsWith('/attention')) return { count: 0, entries: [] };
		if (url.startsWith('/hooks')) return [{ id: 'a', description: 'alpha' }, { id: 'b' }];
		if (url.startsWith('/runs')) return [];
		if (url.startsWith('/images')) return [];
		if (url.startsWith('/events')) return [];
		if (url.startsWith('/kv')) return [];
		if (url.startsWith('/concurrency')) return [];
		if (url.startsWith('/config')) return {};
		if (url.startsWith('/version')) return { version: 'test' };
		return {};
	};

	const FakeDate = class extends Date {
		static now() {
			return now;
		}
	};

	const sandbox = {
		console: { log() {}, info() {}, warn() {}, error() {} },
		CustomEvent,
		Event,
		URLSearchParams,
		JSON,
		Math,
		Date: FakeDate,
		Promise,
		Set,
		Map,
		AbortSignal: { timeout: () => undefined }, // fetch stub ignores signals
		location: { hash: '' },
		localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
		navigator: {},
		isSecureContext: false,
		setTimeout: (fn, ms) => {
			const id = nextTimerID++;
			timers.set(id, { at: now + (ms || 0), fn });
			return id;
		},
		clearTimeout: (id) => void timers.delete(id),
		setInterval: (fn, ms) => {
			const id = nextTimerID++;
			timers.set(id, { at: now + (ms || 0), fn, every: ms || 0 });
			return id;
		},
		clearInterval: (id) => void timers.delete(id),
		fetch: async (url) => {
			requests.push({ url, t: now });
			if (!serverUp) throw new Error(`${url}: connection refused`);
			const body = routes(url);
			return { ok: true, status: 200, json: async () => body };
		},
		document: new Proxy(
			{},
			{
				get(_t, prop) {
					switch (prop) {
						case 'getElementById':
							return (id) => {
								if (!elements.has(id)) elements.set(id, makeElement(id));
								return elements.get(id);
							};
						case 'createElement':
							return () => makeElement();
						case 'createTextNode':
							return (s) => ({ text: s });
						case 'querySelectorAll':
							return () => [];
						case 'querySelector':
							return () => makeElement();
						case 'addEventListener':
							return docTarget.addEventListener.bind(docTarget);
						case 'removeEventListener':
							return docTarget.removeEventListener.bind(docTarget);
						case 'dispatchEvent':
							return docTarget.dispatchEvent.bind(docTarget);
						case 'body':
							return makeElement('body');
						case 'hidden':
							return false;
						default:
							return () => undefined;
					}
				},
			},
		),
	};
	sandbox.window = sandbox;
	sandbox.globalThis = sandbox;
	sandbox.addEventListener = winTarget.addEventListener.bind(winTarget);
	sandbox.removeEventListener = winTarget.removeEventListener.bind(winTarget);
	sandbox.dispatchEvent = winTarget.dispatchEvent.bind(winTarget);

	vm.createContext(sandbox);
	vm.runInContext(dashboardSrc, sandbox, { filename: 'dashboard.js' });

	return {
		sandbox,
		requests,
		elements,
		settle,
		advance,
		setServerUp: (up) => {
			serverUp = up;
		},
		// Drive the page the way timeline.js does.
		streamState(live) {
			sandbox.whrStreamLive = live;
			sandbox.dispatchEvent(new CustomEvent('whr:stream-state', { detail: { live } }));
		},
		signal(sections) {
			sandbox.dispatchEvent(new CustomEvent('whr:sections-changed', { detail: { sections } }));
		},
		runDelta(run) {
			sandbox.dispatchEvent(new CustomEvent('whr:run-delta', { detail: { id: run.id, run } }));
		},
		urls: () => requests.map((r) => r.url),
		clear: () => {
			requests.length = 0;
		},
	};
}

async function boot() {
	const h = makeSandbox();
	await h.settle();
	return h;
}

// -- The tests ---------------------------------------------------------------------

test('page load fetches /hooks exactly once and paints every section', async () => {
	const h = await boot();
	const hookFetches = h.urls().filter((u) => u === '/hooks');
	assert.equal(hookFetches.length, 1, `/hooks must be fetched exactly once at boot, saw: ${h.urls().join(', ')}`);
	for (const want of ['/health', '/hooks', '/attention', '/images', '/events?max=100', '/kv', '/concurrency']) {
		assert.ok(h.urls().includes(want), `boot must fetch ${want}`);
	}
	// The single fetch is REPUBLISHED for timeline.js instead of refetched.
	assert.ok(Array.isArray(h.sandbox.whrHooks), 'boot must publish window.whrHooks');
	assert.equal(h.sandbox.whrHooks[0].id, 'a');
});

test('idle dashboard with the stream live makes ZERO requests for 60s', async () => {
	const h = await boot();
	h.streamState(true); // triggers the one-per-connect full resync
	await h.settle();
	h.clear();
	await h.advance(60_000);
	assert.deepEqual(h.urls(), [], 'no requests may leave an idle dashboard while the stream is live');
});

test('a changed signal refetches exactly the named section, once', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	h.clear();
	h.signal(['kv']);
	await h.advance(0);
	assert.deepEqual(h.urls(), ['/kv'], 'one kv signal = one /kv refetch, nothing else');
	h.clear();
	await h.advance(60_000);
	assert.deepEqual(h.urls(), [], 'and silence again afterwards');
});

test('signal bursts coalesce: leading edge + one trailing pass', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	await h.advance(2_000); // stand clear of the resync's coalesce window
	h.clear();
	// A storm of signals inside one coalesce window.
	for (let i = 0; i < 25; i++) h.signal(['kv', 'events']);
	await h.advance(0); // leading pass
	await h.advance(1_000); // trailing pass window
	const kv = h.urls().filter((u) => u === '/kv').length;
	const ev = h.urls().filter((u) => u.startsWith('/events')).length;
	assert.ok(kv >= 1 && kv <= 2, `25 kv signals must coalesce to 1-2 fetches, saw ${kv}`);
	assert.ok(ev >= 1 && ev <= 2, `25 events signals must coalesce to 1-2 fetches, saw ${ev}`);
	h.clear();
	await h.advance(60_000);
	assert.deepEqual(h.urls(), [], 'a drained burst leaves no residual polling');
});

test('an attention signal refetches /attention on BOTH views (the banner is global)', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	await h.advance(2_000); // stand clear of the resync's coalesce window
	h.clear();
	// Overview: the attention section refetches like any other.
	h.signal(['attention']);
	await h.advance(1_000);
	assert.deepEqual(h.urls(), ['/attention'], 'one attention signal = one /attention refetch');
	// App view (#hook=…): granular sections normally fold into "app", but
	// attention stays granular — the banner renders on this view too.
	h.sandbox.location.hash = '#hook=a';
	h.clear();
	h.signal(['attention']);
	await h.advance(1_000);
	assert.deepEqual(h.urls(), ['/attention'], 'the app view must refetch /attention, nothing else');
	h.sandbox.location.hash = '';
	h.clear();
	await h.advance(60_000);
	assert.deepEqual(h.urls(), [], 'and silence again afterwards');
});

test('hooks signal republishes the roster for timeline.js', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	await h.advance(2_000); // stand clear of the resync's coalesce window
	h.clear();
	let republished = 0;
	h.sandbox.addEventListener('whr:hooks-data', () => republished++);
	h.signal(['hooks']);
	await h.advance(1_000);
	assert.equal(h.urls().filter((u) => u === '/hooks').length, 1);
	assert.equal(republished, 1, 'the refetched roster must be republished exactly once');
});

test('stream down: the fixed-cadence fallback poll takes over, and never gives up', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	h.streamState(false);
	h.clear();
	await h.advance(5_000);
	const first = h.urls().length;
	assert.ok(first >= 6, `a fallback tick must do a full refresh, saw only: ${h.urls().join(', ')}`);
	// A dead server does not stop it (fixed cadence forever, no backoff).
	h.setServerUp(false);
	h.clear();
	await h.advance(30_000);
	const healthProbes = h.urls().filter((u) => u === '/health').length;
	assert.equal(healthProbes, 6, `6 ticks in 30s at a FIXED 5s cadence, saw ${healthProbes}`);
	h.setServerUp(true);
});

test('push signals are ignored while the stream is down (the fallback owns recovery)', async () => {
	const h = await boot();
	h.streamState(false);
	await h.settle();
	h.clear();
	h.signal(['kv']);
	await h.advance(0);
	assert.deepEqual(h.urls(), [], 'a stale signal with the stream down must not fetch');
});

test('stream recovery does one full resync, then goes silent', async () => {
	const h = await boot();
	h.streamState(false);
	await h.advance(10_000); // fallback polling…
	h.clear();
	h.streamState(true);
	await h.settle();
	assert.equal(h.urls().filter((u) => u === '/hooks').length, 1, 'recovery = one full refresh');
	h.clear();
	await h.advance(60_000);
	assert.deepEqual(h.urls(), [], 'push-only after the resync');
});

test('run deltas refresh the overview runs table only while it is visible', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	await h.advance(2_000);
	h.clear();
	// Hidden table (the default): a delta costs nothing.
	h.elements.get('runs-section').hidden = true;
	h.runDelta({ id: 'r1', hook_id: 'a', status: 'running' });
	await h.advance(1_500);
	assert.deepEqual(h.urls(), [], 'deltas must not fetch for a hidden table');
	// Visible table: a delta refills it once.
	h.elements.get('runs-section').hidden = false;
	h.runDelta({ id: 'r1', hook_id: 'a', status: 'success' });
	await h.advance(1_500);
	assert.deepEqual(h.urls(), ['/runs?max=50'], 'one delta = one table refill');
});

test('a revealed runs table refills immediately', async () => {
	const h = await boot();
	h.streamState(true);
	await h.settle();
	await h.advance(2_000);
	h.clear();
	h.elements.get('runs-section').hidden = false;
	h.sandbox.dispatchEvent(new CustomEvent('whr:runs-table-shown'));
	await h.advance(1_000);
	assert.deepEqual(h.urls(), ['/runs?max=50']);
});
