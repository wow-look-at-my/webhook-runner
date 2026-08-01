// The dashboard's side of the <activity-feed> migration, driving the REAL
// committed assets/dashboard.js in a vm sandbox.
//
// Both activity feeds (the overview Activity page and the per-hook section)
// are now js-snippets' <activity-feed> component, imported at runtime by the
// module script at the bottom of index.html. The rendering, the kind badges
// and the filtering live upstream and are tested there; what belongs HERE is
// the consumer contract — that dashboard.js hands the elements the right
// data and the right host hooks, and keeps doing so when the component has
// not loaded yet.
//
// That last part is the subtle one. The component is fetched cross-origin at
// runtime, so on every page load dashboard.js sets properties on elements
// that are still inert HTMLElements. The values must land on the element
// anyway (the component's connectedCallback upgrades them on definition) —
// a dashboard.js that skipped unupgraded elements, or stashed the data
// somewhere else, would show permanently empty feeds and no test of the
// component upstream would catch it.
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
const indexHtml = readFileSync(path.join(here, '..', 'assets', 'index.html'), 'utf8');
const dashboardCss = readFileSync(path.join(here, '..', 'assets', 'dashboard.css'), 'utf8');
const timelineTs = readFileSync(path.join(here, '..', 'ts', 'timeline.ts'), 'utf8');
const timelineJs = readFileSync(path.join(here, '..', 'assets', 'timeline.js'), 'utf8');

// A stand-in for the not-yet-upgraded custom element: a plain object that
// records whatever is assigned, exactly like a real HTMLElement would.
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

function makeSandbox() {
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
		setTimeout: (fn) => (void fn, 0),
		clearTimeout: () => {},
		setInterval: () => 0,
		clearInterval: () => {},
		fetch: async () => ({ ok: true, status: 200, json: async () => ({}), text: async () => '{}' }),
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

const EVENTS = [
	{ time: '2026-07-31T10:00:00Z', kind: 'manager.exited', msg: 'gha-coordinator: instance exited (code 0)', fields: { hook: 'gha-coordinator' } },
	{ time: '2026-07-31T10:00:01Z', kind: 'run.queued', msg: 'queued run for wow-look-at-my/webhooks#41' },
];

test('both feeds are fed the entries verbatim, in order', () => {
	const { sandbox, elements } = makeSandbox();

	sandbox.renderEvents(EVENTS);
	const overview = elements.get('events-feed');
	assert.ok(overview, 'the overview feed element was reached');
	assert.equal(overview.entries.length, 2);
	assert.equal(overview.entries.map((e) => e.kind).join(','), 'manager.exited,run.queued',
		'order is the server\'s (newest-first), never re-sorted here');

	sandbox.renderEventsInto('app-events-feed', EVENTS);
	assert.equal(elements.get('app-events-feed').entries.length, 2, 'the per-hook feed is the same control');
});

test('a nil recorder (null events) becomes an empty feed, not a crash', () => {
	const { sandbox, elements } = makeSandbox();
	// The values live in the vm realm, so compare CONTENT, not cross-realm
	// object identity (deepEqual would fail on the prototype alone).
	sandbox.renderEvents(null);
	assert.equal(elements.get('events-feed').entries.length, 0, 'null must normalize to [] for the component');
	assert.ok(Array.isArray([...elements.get('events-feed').entries]), 'and it must be an array, not null');
	sandbox.renderEvents(undefined);
	assert.equal(elements.get('events-feed').entries.length, 0);
});

test('the host hooks are wired: GitHub slugs stay clickable and times match the rest of the UI', () => {
	const { sandbox, elements } = makeSandbox();
	sandbox.renderEvents(EVENTS);
	const feed = elements.get('events-feed');

	// messageRenderer must produce NODES (linkifyGH's fragment), not a
	// string — that is the whole reason the component exposes the hook.
	assert.equal(typeof feed.messageRenderer, 'function');
	const rendered = feed.messageRenderer('see wow-look-at-my/webhooks#41');
	assert.ok(rendered && typeof rendered === 'object', 'linkified messages come back as DOM, not text');

	// timeFormatter must be the dashboard's own fmtTime, so feed timestamps
	// read identically to every other table on the page.
	assert.equal(typeof feed.timeFormatter, 'function');
	assert.equal(feed.timeFormatter('2026-07-31T10:00:00Z'), sandbox.fmtTime('2026-07-31T10:00:00Z'));
	assert.equal(feed.timeFormatter(''), '', 'a missing time stays blank, never "Invalid Date"');

	// The Go side records both hooks.* and hook.*; they are one subsystem.
	assert.equal(JSON.stringify(feed.familyAliases), JSON.stringify({ hooks: 'hook' }));
});

test('properties are set even though the element has not upgraded yet', () => {
	// The sandbox never loads the component, so these elements are inert —
	// exactly the state of the page between first paint and the runtime
	// import resolving. The values must still land on the element, because
	// the component's connectedCallback upgrade is what replays them.
	const { sandbox, elements } = makeSandbox();
	sandbox.renderEvents(EVENTS);
	const feed = elements.get('events-feed');
	assert.ok(Object.prototype.hasOwnProperty.call(feed, 'entries'),
		'entries must be assigned onto the element, not stashed in module state');
	assert.equal(feed.entries[0].kind, 'manager.exited');
});

// -- The migration itself ----------------------------------------------------

test('the old hand-rolled feed is gone from every asset', () => {
	// The local renderer, the kind classifier and the per-kind stylesheet all
	// moved into the component. Leaving any of them behind means two feeds
	// that drift apart — the exact failure this migration removes.
	for (const dead of ['renderEventRows', 'eventKindClass', 'eventSeverity(', 'EVENT_SEVERITY_RULES']) {
		assert.ok(!dashboardSrc.includes(dead), `dashboard.js still defines ${dead}`);
	}
	assert.ok(!/^\.kind[\s.{]/m.test(dashboardCss), 'dashboard.css still styles .kind — the component owns that badge now');
	assert.ok(!indexHtml.includes('id="events-table"'), 'index.html still has the hand-rolled overview table');
	assert.ok(!indexHtml.includes('id="app-events-table"'), 'index.html still has the hand-rolled per-hook table');
});

test('index.html mounts both feeds; the component load lives in the ts0 bundle', () => {
	assert.ok(indexHtml.includes('id="events-feed"'), 'the overview feed element is mounted');
	assert.ok(indexHtml.includes('id="app-events-feed"'), 'the per-hook feed element is mounted');

	// Light-DOM fallback: what the operator sees while the cross-origin
	// import is in flight (or failing). Without it a slow fetch reads as an
	// empty panel, which on THIS page means "nothing has happened".
	const mounts = indexHtml.match(/<activity-feed[\s\S]*?<\/activity-feed>/g) ?? [];
	assert.equal(mounts.length, 2, 'exactly the two feeds');
	for (const m of mounts) {
		assert.match(m, /loading activity feed/, 'each feed carries its light-DOM loading line');
		assert.match(m, /storage-key="whr\./, 'each feed persists its own filter under its own key');
	}
	assert.notEqual(
		mounts[0].match(/storage-key="([^"]+)"/)?.[1],
		mounts[1].match(/storage-key="([^"]+)"/)?.[1],
		'the two feeds must not share one persisted filter',
	);

	// The import belongs in the ts0-compiled bundle, NOT hand-written into
	// the page. index.html must carry exactly ONE module script (timeline.js);
	// an inline one would be uncompiled, un-type-checked JavaScript living
	// outside the ts/ tree the dashboard-assets CI job gates.
	// Strip HTML comments first: the comment above the script tag NAMES the
	// thing it forbids ("do not add a hand-written <script type=module>"), and
	// matching that prose would fail the test for documenting the rule.
	const markup = indexHtml.replace(/<!--[\s\S]*?-->/g, '');
	const moduleScripts = markup.match(/<script[^>]*type="module"[^>]*>/g) ?? [];
	assert.equal(moduleScripts.length, 1, `expected only timeline.js as a module, got ${moduleScripts.join(' ')}`);
	assert.match(moduleScripts[0], /src="timeline\.js"/, 'the one module script is the generated bundle');

	// The retry-forever loader and the URL live in the bundle's source.
	assert.ok(
		timelineTs.includes("const ACTIVITY_FEED_URL = 'https://sites.pazer.build/js-snippets/branch/library/ui/activity-feed.js'"),
		'ts/timeline.ts declares the buildhost component URL',
	);
	assert.ok(timelineTs.includes('loadActivityFeedForever'), 'ts/timeline.ts owns the never-give-up loader');
	assert.ok(
		timelineTs.includes('void loadActivityFeedForever()'),
		'the feed load is fired, not awaited — a failing feed component must not block the chart',
	);
	assert.ok(!timelineTs.includes('github.io/js-snippets'), 'the unpublished GitHub Pages origin must not be referenced');

	// And the committed bundle actually carries it (the generated artifact is
	// what ships; a stale bundle is exactly what dashboard-assets guards).
	assert.ok(
		timelineJs.includes('https://sites.pazer.build/js-snippets/branch/library/ui/activity-feed.js'),
		'the committed assets/timeline.js is a build of the current ts/ source',
	);
});
