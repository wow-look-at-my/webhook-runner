// Focused harness for two dashboard behaviors, driving the REAL committed
// assets/dashboard.js in a vm sandbox (the harness.test.ts approach, with
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

// -- Stub DOM (recording variant) -------------------------------------------
//
// Like harness.test.ts's element stub, but appendChild RECORDS children
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
		// Recorded handlers, so a test can fire an event the way the
		// component does (el.listeners['table-filter-change'](ev)) instead
		// of reaching into the renderer.
		listeners: {},
		// Recorded attributes: a custom element's configuration (empty-text,
		// storage-key) is set through setAttribute and is otherwise
		// invisible to a stub that swallows it.
		attributes: {},
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
				case 'addEventListener':
					return (ev, fn) => {
						t.listeners[ev] = fn;
					};
				case 'setAttribute':
					return (k, v) => {
						t.attributes[k] = String(v);
					};
				case 'getAttribute':
					return (k) => (k in t.attributes ? t.attributes[k] : null);
				case 'hasAttribute':
					return (k) => k in t.attributes;
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
	const requests = []; // every fetch URL, in order

	// Canned routes; per-URL status so /runs/{id} can 404.
	const routes = (url) => {
		if (url.startsWith('/health')) return { status: 200, body: { status: 'ok' } };
		if (url.startsWith('/attention')) return { status: 200, body: { count: 0, entries: [] } };
		// One hook's app-page detail. by_status is the RETENTION-WINDOW count
		// the filter chips read — deliberately much larger than any page.
		if (url.startsWith('/hooks/a')) {
			return {
				status: 200,
				body: {
					info: { id: 'a', timeout: '5m', state: false },
					disabled: false,
					stats: { tracked: 4213, by_status: { skipped: 4200, success: 11, failure: 2 }, retention: '48h' },
					image: { built: true },
				},
			};
		}
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
			requests.push(String(url));
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
	return { sandbox, elements, stored, requests };
}

const settle = async () => {
	for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
};

// -- 1. The status filter's pure selection ----------------------------------

// The rows and chips are rendered by js-snippets' <data-table>, which does
// not exist in this stub DOM — so what is asserted here is the CONTRACT the
// dashboard hands that component, which is the part this repo owns.
test('the status facet is declared host-filtered, never local', async () => {
	const { sandbox, elements } = makeSandbox();
	sandbox.location.hash = '#hook=a';
	await settle();
	await sandbox.refreshApp('a');
	await settle();

	const table = elements.get('app-runs-table');
	const status = table.facets.find((f) => f.key === 'status');
	assert.ok(status, 'the runs table declares a status facet');
	// THE invariant. The rows in hand are a server-filtered window (the
	// exclusion is applied before the row cap), so a component that also
	// filtered locally would empty a table whose remaining rows all sit
	// past the cap — the exact bug the server-side filter exists to fix.
	assert.equal(status.local, false, 'the status facet must never filter client-side');
	assert.ok(status.counts, 'a host-filtered facet must supply its own counts');
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

// -- 4. Filter first, then limit --------------------------------------------
//
// The status filter is applied SERVER-SIDE now. The bug: the page fetched
// the newest 50 runs and hid statuses locally, so a hook whose recent
// history is all skips showed an empty table ("All 50 recent run(s) are
// hidden by the status filter above") with its real runs just past the
// window, unreachable at any limit.

const appRunsFetches = (requests) => requests.filter((u) => u.startsWith('/runs?hook='));

test('the app page sends the hidden statuses as ?exclude= so the limit counts visible runs', async () => {
	const { sandbox, requests } = makeSandbox();
	await settle();

	// Default hidden set is {skipped}: it must reach the SERVER, not just
	// the renderer.
	await sandbox.refreshApp('a');
	await settle();
	const first = appRunsFetches(requests);
	assert.equal(first.length, 1, 'one runs fetch per app refresh');
	assert.match(first[0], /[?&]max=50(&|$)/);
	assert.match(first[0], /[?&]exclude=skipped(&|$)/, 'the filter must be pushed to the server');

	// Showing everything drops the parameter entirely — an unfiltered fetch,
	// not exclude= with an empty value.
	sandbox.saveAppRunsHiddenStatuses(new Set());
	await sandbox.refreshApp('a');
	await settle();
	const second = appRunsFetches(requests);
	assert.equal(second.length, 2);
	assert.doesNotMatch(second[1], /exclude/, 'an empty hidden set sends no exclude at all');

	// Several hidden statuses travel as one csv, sorted for a stable URL.
	sandbox.saveAppRunsHiddenStatuses(new Set(['success', 'skipped']));
	await sandbox.refreshApp('a');
	await settle();
	const third = appRunsFetches(requests);
	assert.equal(third.length, 3);
	assert.match(third[2], /[?&]exclude=skipped%2Csuccess(&|$)/);
});

test('a chip click refetches — a different filter is a different page of runs', async () => {
	const { sandbox, elements, requests } = makeSandbox();
	sandbox.location.hash = '#hook=a';
	await settle();

	await sandbox.refreshApp('a');
	await settle();
	const before = appRunsFetches(requests).length;

	// A chip lives in the component's shadow root, so drive its public
	// contract instead: the component emits table-filter-change with the
	// new hidden set, and the dashboard must react by REFETCHING.
	const table = elements.get('app-runs-table');
	table.listeners['table-filter-change']({ detail: { query: '', hidden: { status: [] }, sort: null } });
	await settle();

	const after = appRunsFetches(requests);
	assert.equal(after.length, before + 1, 'toggling a chip must REFETCH, not re-select the page in hand');
	assert.doesNotMatch(after[after.length - 1], /exclude/, 'showing skipped drops the exclusion server-side');
});

test('chip counts come from the retention window, not the fetched page', async () => {
	const { sandbox, elements } = makeSandbox();
	await settle();

	// /runs answers [] for this hook while stats.by_status reports thousands:
	// exactly the post-filter state, where the page cannot supply the counts.
	await sandbox.refreshApp('a');
	await settle();

	const table = elements.get('app-runs-table');
	const counts = table.facets.find((f) => f.key === 'status').counts;
	assert.equal(counts.skipped, 4200, `window count expected, got ${JSON.stringify(counts)}`);
	assert.equal(counts.success, 11, `window count expected, got ${JSON.stringify(counts)}`);
	// The default-hidden status keeps a chip even at zero, or the control
	// is undiscoverable on a hook that has never skipped.
	assert.ok(table.facets.find((f) => f.key === 'status').always.includes('skipped'));

	// And the empty text names the filter instead of counting a page that
	// no longer contains the hidden rows.
	const empty = table.attributes['empty-text'];
	assert.match(empty, /status filter/);
	assert.match(empty, /skipped/);
	assert.doesNotMatch(empty, /All 0 recent/);
});
