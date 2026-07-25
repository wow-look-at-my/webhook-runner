// The manager page's live refresh, driving the REAL committed
// assets/dashboard.js in a vm sandbox (harness.test.ts's approach: fake
// clock, recorded fetches, stub DOM):
//
//   1. On #manager=<id>, a pushed changed{managers} signal refetches the
//      roster AND that manager's drill-down — once each, then silence.
//      This is the whole fix: instance output, inbox depth and supervision
//      state record no activity event, so before the server-side seam the
//      page only moved when the operator hit F5.
//   2. The output tail FOLLOWS only while the operator is at the bottom —
//      a refresh once per pushed line must not yank a scrolled-back reader
//      down.
//   3. The instance-uptime row advances locally between refreshes, with
//      ZERO extra requests (a silent manager emits nothing to push).
//
// Run: node --experimental-strip-types --test internal/server/dashboard/testjs/*.test.ts

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

const MANAGER_ROSTER = [
	{
		id: 'coord',
		state: 'running',
		disabled: false,
		instance_id: 'abcdefghij234567',
		instance_started: '2026-07-21T00:00:00Z',
		restarts: 2,
		inbox_depth: 1,
	},
];

const MANAGER_DETAIL = {
	...MANAGER_ROSTER[0],
	enabled_by_default: true,
	output: ['2026-07-21T00:00:01Z hello', '2026-07-21T00:00:02Z world'],
};

// -- Stub DOM ---------------------------------------------------------------

function makeElement(id) {
	const target = {
		id: id ?? '',
		hidden: false,
		open: false,
		textContent: '',
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
			if (prop === 'innerHTML') return t._innerHTML ?? '';
			if (prop in t) return t[prop];
			switch (prop) {
				case 'appendChild':
					return (x) => {
						t.children.push(x);
						return x;
					};
				case 'replaceChildren':
					return (...xs) => {
						t.children = xs;
					};
				case 'querySelectorAll':
					return () => [];
				case 'querySelector':
					return () => makeElement();
				case 'closest':
					return () => null;
				default:
					return () => undefined;
			}
		},
		set(t, prop, v) {
			if (prop === 'innerHTML') {
				t._innerHTML = v;
				if (v === '') t.children = [];
				return true;
			}
			t[prop] = v;
			return true;
		},
	});
}

// -- The sandbox ------------------------------------------------------------

function makeSandbox() {
	const requests = [];
	const elements = new Map();
	const winTarget = new EventTarget();
	const docTarget = new EventTarget();

	let now = 1_800_000_000_000;
	let nextTimerID = 1;
	const timers = new Map();
	async function settle() {
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

	const routes = (url) => {
		if (url.startsWith('/health')) return { status: 'ok', version: 'test' };
		if (url.startsWith('/attention')) return { count: 0, entries: [] };
		if (url.startsWith('/hooks')) return [{ id: 'a' }];
		if (url.startsWith('/managers/')) return MANAGER_DETAIL;
		if (url.startsWith('/managers')) return MANAGER_ROSTER;
		if (url.startsWith('/runs')) return [];
		if (url.startsWith('/images')) return [];
		if (url.startsWith('/events')) return [];
		if (url.startsWith('/kv')) return [];
		if (url.startsWith('/concurrency')) return { groups: [] };
		if (url.startsWith('/reload')) return { mode: 'none' };
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
		Number,
		AbortSignal: { timeout: () => undefined },
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
			requests.push(url);
			const body = routes(url);
			return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) };
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
							return (s) => ({ text: String(s) });
						case 'createDocumentFragment':
							return () => makeElement();
						case 'querySelectorAll':
							return () => [];
						case 'querySelector':
							return () => makeElement();
						case 'addEventListener':
							return docTarget.addEventListener.bind(docTarget);
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
		elements,
		settle,
		advance,
		streamState(live) {
			sandbox.whrStreamLive = live;
			sandbox.dispatchEvent(new CustomEvent('whr:stream-state', { detail: { live } }));
		},
		signal(sections) {
			sandbox.dispatchEvent(new CustomEvent('whr:sections-changed', { detail: { sections } }));
		},
		urls: () => requests.slice(),
		clear: () => {
			requests.length = 0;
		},
	};
}

async function bootOnManagerPage() {
	const h = makeSandbox();
	h.sandbox.location.hash = '#manager=coord';
	await h.settle();
	h.streamState(true);
	await h.advance(2_000); // stand clear of the connect resync's coalesce window
	h.clear();
	return h;
}

// -- 1. The push refresh ------------------------------------------------------

test('a managers signal refetches the roster AND the open drill-down, once', async () => {
	const h = await bootOnManagerPage();
	h.signal(['managers']);
	await h.advance(1_000);

	const roster = h.urls().filter((u) => u === '/managers').length;
	const detail = h.urls().filter((u) => u === '/managers/coord').length;
	assert.equal(roster, 1, `one signal = one /managers refetch, saw: ${h.urls().join(', ')}`);
	assert.equal(detail, 1, `the open #manager= drill-down must refetch too, saw: ${h.urls().join(', ')}`);

	h.clear();
	await h.advance(60_000);
	assert.deepEqual(
		h.urls().filter((u) => u.startsWith('/managers')),
		[],
		'the page is push-fed: no polling once the signal is drained',
	);
});

test('the drill-down renders what the refetch returned', async () => {
	const h = await bootOnManagerPage();
	h.signal(['managers']);
	await h.advance(1_000);

	assert.equal(h.elements.get('manager-detail').hidden, false, 'the open drill-down must be visible');
	assert.match(
		h.elements.get('manager-detail-output').textContent,
		/hello\n.*world/,
		'the log tail renders the refetched lines',
	);
});

// -- 2. The log tail follows only from the bottom ----------------------------

test('the output tail follows the bottom, but never yanks a scrolled-back reader', async () => {
	const h = await bootOnManagerPage();
	const out = h.elements.get('manager-detail-output');

	// At the bottom (a fresh box has no geometry at all): follow.
	out.scrollHeight = 500;
	out.clientHeight = 100;
	out.scrollTop = 400;
	h.sandbox.renderManagerDetail(MANAGER_DETAIL);
	assert.equal(out.scrollTop, 500, 'at the tail, a refresh keeps following it');

	// Scrolled back to read history: leave the operator exactly where they are.
	out.scrollTop = 120;
	h.sandbox.renderManagerDetail(MANAGER_DETAIL);
	assert.equal(out.scrollTop, 120, 'a scrolled-back reader must not be dragged to the bottom');
});

// -- 3. The uptime advances without a request ---------------------------------

test('instance uptime ticks locally, with no extra requests', async () => {
	const h = await bootOnManagerPage();
	h.sandbox.renderManagerDetail(MANAGER_DETAIL);
	const cell = h.elements.get('manager-uptime');
	cell.textContent = '';

	await h.advance(5_000);
	assert.match(cell.textContent, /since/, 'the uptime row must be restamped by the local ticker');
	assert.deepEqual(h.urls(), [], 'the ticker is local: it must not fetch anything');
});
