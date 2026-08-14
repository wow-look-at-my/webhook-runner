// The run-detail modal's LIVE refresh, driving the REAL committed
// assets/dashboard.js in a vm sandbox (harness.test.ts's approach: fake
// clock, recorded fetches, stub DOM).
//
// The invariant under test: an open modal on a NON-TERMINAL run keeps
// refetching /runs/{id} on its fixed cadence EVEN WHILE THE STREAM IS
// LIVE. Stream deltas cannot carry output — runs.Run.AppendOutput
// deliberately does not fire the tracker's OnChange seam — so a run that
// is merely logging emits no deltas at all, and the old
// `if (window.whrStreamLive === true) return;` stand-down froze the modal
// for the whole run precisely when the dashboard was healthiest.
//
// The counterweights matter as much as the poll: it must stop at a
// terminal render and when the dialog closes, and a delta-driven refresh
// must defer the next tick rather than doubling the request rate.
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

const RUN_ID = 'aaaabbbbccccddddeeeeffffgg';

function makeElement(id) {
	const target = {
		id: id ?? '',
		hidden: false,
		open: false,
		disabled: false,
		textContent: '',
		className: '',
		title: '',
		value: '',
		scrollTop: 0,
		scrollHeight: 0,
		clientHeight: 0,
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
				case 'showModal':
					return () => {
						t.open = true;
					};
				case 'close':
					return () => {
						t.open = false;
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

function makeSandbox() {
	const requests = [];
	const elements = new Map();
	const winTarget = new EventTarget();
	const docTarget = new EventTarget();

	let now = 1_800_000_000_000;
	let nextTimerID = 1;
	const timers = new Map();

	// The run under the modal: starts RUNNING and merely logs (no state
	// change, hence no delta) until a test flips it terminal.
	const run = {
		id: RUN_ID,
		hook_id: 'a',
		status: 'running',
		exit_code: 0,
		started: '2026-07-21T00:00:00Z',
		output: ['line 1'],
	};

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
		if (url.startsWith('/managers')) return [];
		if (url.startsWith(`/runs/${RUN_ID}`)) return { ...run, output: run.output.slice() };
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
		run,
		settle,
		advance,
		streamState(live) {
			sandbox.whrStreamLive = live;
			sandbox.dispatchEvent(new CustomEvent('whr:stream-state', { detail: { live } }));
		},
		delta(r) {
			sandbox.dispatchEvent(new CustomEvent('whr:run-delta', { detail: { id: r.id, run: r } }));
		},
		detailFetches: () => requests.filter((u) => u.startsWith(`/runs/${RUN_ID}`)).length,
		clear: () => {
			requests.length = 0;
		},
	};
}

// The modal is opened with the stream LIVE and the run merely logging.
async function openModalOnLiveRun() {
	const h = makeSandbox();
	await h.settle();
	h.streamState(true);
	await h.settle();
	await h.sandbox.showRun(RUN_ID);
	await h.settle();
	assert.equal(h.elements.get('run-detail').open, true, 'the modal must be open');
	h.clear();
	return h;
}

test('an open modal keeps refreshing a running run WHILE THE STREAM IS LIVE', async () => {
	const h = await openModalOnLiveRun();

	// The run logs; nothing about it changes state, so no delta will ever
	// arrive. Only the poll can bring the new lines in.
	h.run.output.push('line 2');
	await h.advance(10_000);

	assert.ok(
		h.detailFetches() >= 3,
		`~3s cadence over 10s must refetch the run at least 3 times, saw ${h.detailFetches()}`,
	);
	assert.ok(
		h.elements.get('run-detail-output').children.length > 0 ||
			h.elements.get('run-detail-output').textContent !== '',
		'the refreshed output must be rendered',
	);
});

test('a delta refresh defers the next poll instead of doubling the rate', async () => {
	const h = await openModalOnLiveRun();
	// A delta lands right away (a status change the stream DOES carry).
	h.delta({ ...h.run, status: 'running' });
	await h.settle();
	assert.equal(h.detailFetches(), 1, 'the delta refetches the modal once');

	// Just under one cadence later the poll must not add a second fetch.
	await h.advance(2_000);
	assert.equal(h.detailFetches(), 1, 'a poll tick covered by the delta must be skipped');

	// Past the cadence it resumes on its own.
	await h.advance(4_000);
	assert.ok(h.detailFetches() >= 2, 'the poll resumes after the deferred interval');
});

test('the refresh stops at a terminal render and when the modal closes', async () => {
	const h = await openModalOnLiveRun();

	h.run.status = 'success';
	h.run.finished = '2026-07-21T00:00:30Z';
	await h.advance(4_000); // one poll picks up the terminal state
	const afterTerminal = h.detailFetches();
	assert.ok(afterTerminal >= 1, 'the terminal state is fetched once');
	await h.advance(30_000);
	assert.equal(h.detailFetches(), afterTerminal, 'a terminal run is final: no further polling');

	// And a closed dialog stops it regardless of status.
	const h2 = await openModalOnLiveRun();
	h2.elements.get('run-detail').open = false;
	await h2.advance(30_000);
	assert.equal(h2.detailFetches(), 0, 'a closed modal must not poll');
});
