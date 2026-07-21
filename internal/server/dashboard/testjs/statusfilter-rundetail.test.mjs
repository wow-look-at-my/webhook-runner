// Focused harness for two dashboard behaviors, driving the REAL committed
// assets/dashboard.js in a vm sandbox (the harness.test.mjs approach, with
// a leaner stub DOM that records enough structure to assert on):
//
//   1. The app runs table's status filter: filterAppRuns is a pure
//      selection over the run data (skipped hidden by default, counts
//      preserved), and the persisted choice round-trips localStorage.
//   2. The run-detail modal ALWAYS opens on a run-link click — a
//      /runs/{id} fetch that 404s (a run the server no longer tracks)
//      renders an explanatory state instead of silently doing nothing.
//   3. renderConcurrency accepts both response shapes: the {global,
//      groups} document and the older bare array.
//
// Run: node --test internal/server/dashboard/testjs/*.test.mjs

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

// -- Stub DOM (recording variant) -------------------------------------------
//
// Like harness.test.mjs's element stub, but appendChild RECORDS children
// (so a rendered message is assertable) and <dialog> methods actually
// track open state — the whole point of test 2 is "did the modal open".
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
		dataset: {},
		style: {},
		children: [],
		showModalCalls: 0,
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
						t.showModalCalls++;
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
				case 'getContext':
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

// Depth-first text extraction over the recorded child tree (createTextNode
// children are {text}; element children carry .children).
function textOf(node) {
	if (!node) return '';
	if (typeof node.text === 'string') return node.text;
	const kids = node.children || [];
	let out = typeof node.textContent === 'string' ? node.textContent : '';
	for (const c of kids) out += textOf(c);
	return out;
}

function makeSandbox() {
	const elements = new Map();
	const winTarget = new EventTarget();
	const docTarget = new EventTarget();
	const stored = new Map(); // localStorage backing

	// Canned routes; per-URL status so /runs/{id} can 404.
	const routes = (url) => {
		if (url.startsWith('/health')) return { status: 200, body: { status: 'ok' } };
		if (url.startsWith('/attention')) return { status: 200, body: { count: 0, entries: [] } };
		if (url.startsWith('/hooks')) return { status: 200, body: [{ id: 'a' }] };
		if (url.startsWith('/runs/live1')) {
			return {
				status: 200,
				body: { id: 'live1', hook_id: 'a', status: 'running', exit_code: 0, started: '2026-07-21T00:00:00Z', output: ['hello'] },
			};
		}
		if (url.startsWith('/runs/')) return { status: 404, body: { error: 'no such run' } };
		if (url.startsWith('/runs')) return { status: 200, body: [] };
		if (url.startsWith('/images')) return { status: 200, body: [] };
		if (url.startsWith('/events')) return { status: 200, body: [] };
		if (url.startsWith('/kv')) return { status: 200, body: [] };
		if (url.startsWith('/concurrency')) return { status: 200, body: { groups: [] } };
		if (url.startsWith('/config')) return { status: 200, body: {} };
		if (url.startsWith('/version')) return { status: 200, body: { version: 'test' } };
		return { status: 200, body: {} };
	};

	const sandbox = {
		console: { log() {}, info() {}, warn() {}, error() {} },
		CustomEvent,
		Event,
		URLSearchParams,
		JSON,
		Math,
		Date,
		Promise,
		Set,
		Map,
		AbortSignal: { timeout: () => undefined },
		location: { hash: '' },
		localStorage: {
			getItem: (k) => (stored.has(k) ? stored.get(k) : null),
			setItem: (k, v) => void stored.set(k, String(v)),
			removeItem: (k) => void stored.delete(k),
		},
		navigator: {},
		isSecureContext: false,
		setTimeout: (fn) => (void fn, 0),
		clearTimeout: () => {},
		setInterval: () => 0,
		clearInterval: () => {},
		fetch: async (url) => {
			const { status, body } = routes(url);
			return {
				ok: status >= 200 && status < 300,
				status,
				json: async () => body,
				text: async () => JSON.stringify(body),
			};
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
	return { sandbox, elements, stored };
}

const settle = async () => {
	for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
};

// -- 1. The status filter's pure selection ----------------------------------

test('filterAppRuns hides exactly the hidden statuses and counts them', () => {
	const { sandbox } = makeSandbox();
	const runs = [
		{ id: '1', status: 'success' },
		{ id: '2', status: 'skipped' },
		{ id: '3', status: 'skipped' },
		{ id: '4', status: 'failure' },
		{ id: '5', status: 'running' },
	];
	const out = sandbox.filterAppRuns(runs, new Set(['skipped']));
	// join(): the returned arrays live in the vm realm, so compare values,
	// not (cross-realm) array identity.
	assert.equal(out.shown.map((r) => r.id).join(','), '1,4,5', 'skipped rows are filtered from the data');
	assert.equal(out.hiddenCounts.get('skipped'), 2, 'the hidden count is preserved for the chip');

	const none = sandbox.filterAppRuns(runs, new Set());
	assert.equal(none.shown.length, 5, 'an empty hidden set shows everything');
	assert.equal(none.hiddenCounts.size, 0);

	const all = sandbox.filterAppRuns(runs, new Set(['success', 'skipped', 'failure', 'running']));
	assert.equal(all.shown.length, 0);
});

test('the hidden-status set defaults to skipped and round-trips persistence', () => {
	const { sandbox } = makeSandbox();
	// Never saved: skipped is hidden by default.
	assert.deepEqual([...sandbox.appRunsHiddenStatuses()], ['skipped']);

	// The operator shows skipped runs (empty set): the choice persists and
	// is NOT confused with "never chose".
	sandbox.saveAppRunsHiddenStatuses(new Set());
	assert.deepEqual([...sandbox.appRunsHiddenStatuses()], [], 'an explicit show-all sticks');

	sandbox.saveAppRunsHiddenStatuses(new Set(['skipped', 'success']));
	assert.deepEqual([...sandbox.appRunsHiddenStatuses()].sort(), ['skipped', 'success']);
});

// -- 2. The run-detail modal always answers a click ---------------------------

test('a run-detail open that 404s still opens the modal with an explanation', async () => {
	const { sandbox, elements } = makeSandbox();
	await settle();
	await sandbox.showRun('gonegonegonegonegonegonego');
	await settle();

	const dlg = elements.get('run-detail');
	assert.ok(dlg, 'the dialog element was reached');
	assert.equal(dlg.open, true, 'the modal must OPEN even when /runs/{id} 404s — a dead click is the bug');
	assert.ok(dlg.showModalCalls >= 1);

	const out = elements.get('run-detail-output');
	assert.match(textOf(out), /no longer tracked/, 'the modal explains the missing run instead of showing nothing');
});

test('a live run still renders the normal detail view', async () => {
	const { sandbox, elements } = makeSandbox();
	await settle();
	await sandbox.showRun('live1');
	await settle();

	const dlg = elements.get('run-detail');
	assert.equal(dlg.open, true);
	assert.doesNotMatch(textOf(elements.get('run-detail-output')), /no longer tracked/);
	assert.equal(elements.get('run-detail-id').textContent, 'live1');
});

// -- 3. /concurrency shape tolerance ----------------------------------------

test('renderConcurrency accepts the {global, groups} document and the legacy array', async () => {
	const { sandbox, elements } = makeSandbox();
	await settle();

	// New shape with a cap: the global block renders (un-hidden).
	sandbox.renderConcurrency({
		global: { limit: 64, default: 64, overridden: false, active: 3, waiting: 1 },
		groups: [{ name: 'g', limit: 2, declared: 2, overridden: false, active: 0, waiting: 0 }],
	});
	assert.equal(elements.get('concurrency-global-cap').hidden, false, 'a reported cap shows the block');

	// New shape without a cap, and the legacy array: block hidden, no throw.
	sandbox.renderConcurrency({ groups: [] });
	assert.equal(elements.get('concurrency-global-cap').hidden, true);
	sandbox.renderConcurrency([]);
	assert.equal(elements.get('concurrency-global-cap').hidden, true);
});
