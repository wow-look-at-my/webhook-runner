// The activity feed's kind badge, driving the REAL committed
// assets/dashboard.js in a vm sandbox (the harness.test.ts approach with a
// throwaway stub DOM — these tests exercise pure classifiers, so the stub
// only has to let the file EVALUATE).
//
// Why this suite exists: the badge's colors used to come from a hand-listed
// set of kinds in dashboard.css. The Go recorder emits 77 kinds and the list
// covered 26, so two thirds of the feed rendered on the same default grey
// pill — a failure like manager.inbox_dropped was visually identical to
// routine chatter. The classes are derived now, and the DRIFT GUARD below is
// the part that keeps them honest: it reads every kind out of the Go sources
// and asserts the styling actually covers them.
//
// Run: node --experimental-strip-types --test internal/server/dashboard/testjs/*.test.ts

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import vm from 'node:vm';

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.join(here, '..', '..', '..', '..');
const dashboardSrc = readFileSync(path.join(here, '..', 'assets', 'dashboard.js'), 'utf8');
const dashboardCss = readFileSync(path.join(here, '..', 'assets', 'dashboard.css'), 'utf8');

// -- Stub DOM (evaluate-only) -----------------------------------------------
//
// Every property resolves to a no-op function, so dashboard.js's top-level
// wiring (getElementById(...).addEventListener, setInterval, the initial
// fetches) runs to completion without a real DOM.
function makeElement(id) {
	const target = { id: id ?? '', hidden: false, open: false, textContent: '', className: '', value: '', dataset: {}, style: {}, children: [], classList: { toggle() {}, add() {}, remove() {}, contains: () => false } };
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
							return (id) => makeElement(id);
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
	return sandbox;
}

// Every kind the Go side records, read from the source of truth: the literal
// first argument of each events Record(...) call. Mirrors the shell audit
// (grep -rhoP '(?<=Record\(")[a-z._]+') that found the 26-of-77 gap.
function goEventKinds() {
	const kinds = new Set();
	const walk = (dir) => {
		for (const ent of readdirSync(dir, { withFileTypes: true })) {
			const p = path.join(dir, ent.name);
			if (ent.isDirectory()) {
				if (ent.name === '.git' || ent.name === 'node_modules') continue;
				walk(p);
				continue;
			}
			if (!ent.name.endsWith('.go') || ent.name.endsWith('_test.go')) continue;
			for (const m of readFileSync(p, 'utf8').matchAll(/Record\(\s*"([a-z][a-z0-9_]*\.[a-z0-9_]+)"/g)) {
				kinds.add(m[1]);
			}
		}
	};
	walk(path.join(repoRoot, 'internal'));
	return [...kinds].sort();
}

test('severity is read off the action half, with the ordering traps pinned', () => {
	const s = makeSandbox();
	const sev = (k) => s.eventSeverity(k);

	assert.equal(sev('manager.inbox_dropped'), 'bad', 'a dropped inbox event is a failure, not chatter');
	assert.equal(sev('image.build_failed'), 'bad');
	assert.equal(sev('spawn.denied'), 'bad');
	assert.equal(sev('env.unresolved'), 'bad');
	assert.equal(sev('reload.held_red'), 'bad', 'the gate seeing a RED status is bad news, not a mere hold');

	// Ordering traps: an earlier rule must win over a later substring match.
	assert.equal(sev('hook.disabled_rejected'), 'bad', 'bad(rejected) must beat warn(disabled)');
	assert.equal(sev('reload.unverified'), 'warn', 'warn(unverified) must beat good(verified)');
	assert.equal(sev('concurrency.override_cleared'), 'good', 'good(cleared) must not trip warn(overridden)');

	assert.equal(sev('hook.disabled'), 'warn');
	assert.equal(sev('reload.held'), 'warn');
	assert.equal(sev('manager.lease_waiting'), 'warn');
	assert.equal(sev('lock.ttl_reaped'), 'warn');
	assert.equal(sev('spool.parked'), 'warn');

	assert.equal(sev('image.built'), 'good');
	assert.equal(sev('hooks.reloaded'), 'good');
	assert.equal(sev('manager.leased'), 'good');
	assert.equal(sev('spool.replayed'), 'good');

	assert.equal(sev('run.skipped'), 'skip');
	assert.equal(sev('schedule.skipped'), 'skip');

	// Unclaimed = ordinary lifecycle chatter, which is most of the feed.
	assert.equal(sev('run.queued'), 'info');
	assert.equal(sev('github.push'), 'info');
	assert.equal(sev('reload.requested'), 'info');
});

test('family is the namespace half, with hooks folded onto hook', () => {
	const s = makeSandbox();
	assert.equal(s.eventFamily('manager.inbox_dropped'), 'manager');
	assert.equal(s.eventFamily('run.queued'), 'run');
	assert.equal(s.eventFamily('hooks.reloaded'), 'hook', 'the one spelling collision the Go side has');
	assert.equal(s.eventKindClass('manager.inbox_dropped'), 'kind fam-manager sev-bad');
});

// -- The drift guard ---------------------------------------------------------

test('every kind the Go side records classifies, and every family it uses has a hue', () => {
	const s = makeSandbox();
	const kinds = goEventKinds();
	// Sanity: the walk found the real vocabulary, not an empty set from a
	// moved directory. The count only grows; the floor is deliberately loose.
	assert.ok(kinds.length >= 70, `expected the Go sources to yield the full kind vocabulary, got ${kinds.length}`);
	assert.ok(kinds.includes('manager.inbox_dropped'), 'the walk reached internal/managers');

	const families = new Set();
	for (const k of kinds) {
		const cls = s.eventKindClass(k);
		assert.match(cls, /^kind fam-[a-z]+ sev-(bad|warn|good|skip|info)$/, `${k} produced "${cls}"`);
		families.add(s.eventFamily(k));
	}

	// The severity rules must actually DISCRIMINATE — the bug this replaced
	// was one bucket swallowing everything. Assert the feed's two loudest
	// buckets are populated and that no single bucket owns the vocabulary.
	const counts = new Map();
	for (const k of kinds) counts.set(s.eventSeverity(k), (counts.get(s.eventSeverity(k)) || 0) + 1);
	assert.ok((counts.get('bad') || 0) >= 10, `expected a populated bad bucket, got ${counts.get('bad') || 0}`);
	assert.ok((counts.get('good') || 0) >= 5, `expected a populated good bucket, got ${counts.get('good') || 0}`);
	for (const [sev, n] of counts) {
		assert.ok(n < kinds.length * 0.7, `severity "${sev}" swallows ${n}/${kinds.length} kinds — the rules stopped discriminating`);
	}

	// The family dot degrades to a muted default, so a missing hue is not a
	// broken badge — but it IS drift, and this is where it gets caught.
	const missing = [...families].filter((f) => !dashboardCss.includes(`.kind.fam-${f} {`)).sort();
	assert.deepEqual(missing, [], `dashboard.css has no hue for these event families: ${missing.join(', ')}`);
});
