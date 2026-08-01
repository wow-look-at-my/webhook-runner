// The needs-attention banner, driven through the REAL section fetcher in a vm
// sandbox — so the test sees exactly what the browser sees: the bytes
// GET /attention actually returns, handed to the code that renders them.
//
// The bug this pins: `/attention` answers the OBJECT `{count, entries}`, and
// the renderer took a bare array. Reading `.length` off the object gave
// `undefined`, so the banner read "undefined problems need attention" — and,
// because `undefined === 0` is false, it could never hide. Underneath it the
// table got an object it could not iterate and said "Nothing needs
// attention." A healthy server therefore showed a permanent red banner
// contradicting its own empty table.
//
// Nothing else could have caught it: the Go handler's tests assert the JSON,
// the component's tests assert rendering of an array, and the seam between
// them belonged to neither. That seam is what this file owns — which is why
// the assertions are on the RESPONSE SHAPE from the endpoint, never on a
// hand-built array.
//
// Run: node --experimental-strip-types --test internal/server/dashboard/testjs/*.test.ts

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';

const here = path.dirname(fileURLToPath(import.meta.url));
const dashboardSrc = readFileSync(path.join(here, '..', 'assets', 'dashboard.js'), 'utf8');

function makeElement(id) {
	const target = {
		id: id ?? '',
		hidden: false,
		textContent: '',
		className: '',
		dataset: {},
		style: {},
		children: [],
		classList: { toggle() {}, add() {}, remove() {}, contains: () => false },
	};
	return new Proxy(target, {
		get(t, prop) {
			if (prop === 'innerHTML') return '';
			if (prop in t) return t[prop];
			switch (prop) {
				case 'appendChild':
					return (x) => (t.children.push(x), x);
				case 'replaceChildren':
					return (...xs) => void (t.children = xs);
				case 'querySelectorAll':
					return () => [];
				case 'querySelector':
					return () => makeElement();
				case 'closest':
					return () => null;
				case 'addEventListener':
					return () => undefined;
				default:
					return () => undefined;
			}
		},
		set(t, prop, v) {
			t[prop] = v;
			return true;
		},
	});
}

// A sandbox whose fetch answers /attention with `body`, verbatim.
function makeSandbox(body) {
	const elements = new Map();
	const winTarget = new EventTarget();
	const docTarget = new EventTarget();
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
		localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
		navigator: {},
		isSecureContext: false,
		// Immediate-but-async: the section work is scheduled through
		// setTimeout, so a stub that dropped the callback would make every
		// assertion below vacuously pass.
		setTimeout: (fn) => (Promise.resolve().then(fn), 0),
		clearTimeout: () => {},
		setInterval: () => 0,
		clearInterval: () => {},
		fetch: async (url) => ({
			ok: true,
			status: 200,
			json: async () => (String(url).startsWith('/attention') ? body : {}),
			text: async () => '{}',
		}),
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
							return (t) => makeElement(t);
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
	return { sandbox, elements };
}

// Drive the REAL push path: the stream signals a changed section, exactly as
// timeline.js does, and dashboard.js fetches and renders it. `sectionFetchers`
// is a top-level const (script-lexical, never a sandbox property), so going
// through the event is also the only honest way in.
async function signalAttention(sandbox) {
	// Section work is skipped while the stream is down (the fallback poll owns
	// that path), so the push case needs a live stream.
	sandbox.whrStreamLive = true;
	sandbox.dispatchEvent(new CustomEvent('whr:sections-changed', { detail: { sections: ['attention'] } }));
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

// The EXACT response shape internal/server/attention.go writes.
function attentionResponse(entries) {
	return { count: entries.length, entries };
}

const ENTRY = {
	source: 'load',
	hook: 'pr-minder',
	key: 'load',
	message: 'pr-minder: decode hook.json: json: unknown field "env"',
	since: '2026-08-01T10:00:00Z',
};

test('a healthy server HIDES the banner (the object response is not read as an array)', async () => {
	const { sandbox, elements } = makeSandbox(attentionResponse([]));
	await signalAttention(sandbox);

	const banner = elements.get('attention-banner');
	const text = elements.get('attention-banner-text');
	assert.equal(banner.hidden, true, 'no problems means no banner — this is the regression');
	assert.ok(
		!String(text.textContent).includes('undefined'),
		`the banner text must never say "undefined": got ${JSON.stringify(text.textContent)}`,
	);
	assert.equal(elements.get('attention-section').hidden, true);
});

test('one problem shows the banner with a real count', async () => {
	const { sandbox, elements } = makeSandbox(attentionResponse([ENTRY]));
	await signalAttention(sandbox);

	assert.equal(elements.get('attention-banner').hidden, false);
	assert.equal(elements.get('attention-banner-text').textContent, '1 problem needs attention');
	assert.equal(elements.get('attention-section').hidden, false);
});

test('several problems pluralize off the entry count, not the wrapper', async () => {
	const entries = [ENTRY, { ...ENTRY, hook: 'pr-resolve' }, { ...ENTRY, hook: 'license-block' }];
	const { sandbox, elements } = makeSandbox(attentionResponse(entries));
	await signalAttention(sandbox);

	assert.equal(elements.get('attention-banner-text').textContent, '3 problems need attention');
});

// The rows must reach the table too: a banner that counts correctly above an
// empty table is the same contradiction in a different place.
test('the entries reach the attention table', async () => {
	const { sandbox, elements } = makeSandbox(attentionResponse([ENTRY]));
	await signalAttention(sandbox);

	const table = elements.get('attention-table');
	assert.ok(Array.isArray(table.rows), `the table must be given an array of rows; got ${typeof table.rows}`);
	assert.equal(table.rows.length, 1);
	assert.equal(table.rows[0].hook, 'pr-minder');
});
