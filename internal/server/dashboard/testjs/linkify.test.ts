// Client harness for the dashboard's GitHub-slug linkification: evaluates the
// REAL assets/dashboard.js inside a vm sandbox whose DOM stub builds an
// INSPECTABLE node tree, then calls linkifyGH()/linkifyTitle() directly to
// prove the contract the dashboard renders everywhere:
//
//   1. "owner/repo#41" becomes ONE link to the issues URL (GitHub redirects
//      it to the PR when the number is one), opening in a new tab.
//   2. The surrounding text survives byte-for-byte, as TEXT NODES — a payload
//      can never inject markup, because nothing is ever parsed as HTML.
//   3. Paths and prose that merely LOOK like a slug ("src/hooks/pr-minder",
//      "true/false", "24/7") are left alone — the false positives that would
//      make log output unreadable.
//   3b. A bare http(s) URL becomes ONE link to itself, and the "owner/repo"
//      inside it is never ALSO slug-linked (URLs are matched first).
//   4. A bare "owner/repo" links only in title mode (run titles, manager
//      titles), never in free-form text like a log line.
//
// Run: node --experimental-strip-types --test internal/server/dashboard/testjs/*.test.ts
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

// -- An inspectable DOM stub ----------------------------------------------------
//
// Real enough for the tree linkifyGH() builds (elements, attributes, text
// nodes, fragments) and permissive everywhere else, so dashboard.js's
// top-level boot code runs to completion without a browser.

interface Node {
	nodeType: number;
	tag?: string;
	data?: string;
	attrs?: Record<string, string>;
	children?: Node[];
	listeners?: string[];
}

function makeNode(tag) {
	const target = {
		nodeType: 1,
		tag,
		id: '',
		hidden: false,
		open: false,
		textContent: '',
		innerHTML: '',
		className: '',
		title: '',
		value: '',
		scrollTop: 0,
		attrs: {},
		listeners: [],
		dataset: {},
		style: {},
		children: [],
		classList: { toggle() {}, add() {}, remove() {}, contains: () => false },
		appendChild(c) {
			target.children.push(c);
			return c;
		},
		replaceChildren(...cs) {
			target.children.length = 0;
			for (const c of cs) target.children.push(c);
		},
		setAttribute(k, v) {
			target.attrs[k] = String(v);
		},
		addEventListener(type) {
			target.listeners.push(type);
		},
	};
	return new Proxy(target, {
		get(t, prop) {
			if (prop in t) return t[prop];
			switch (prop) {
				case 'querySelectorAll':
					return () => [];
				case 'querySelector':
					return () => makeNode('div');
				case 'closest':
					return () => null;
				case 'getContext':
					return () => null;
				default:
					return () => undefined;
			}
		},
		set(t, prop, v) {
			// The page clears a container with innerHTML = "" — the stub's
			// recorded children must clear with it.
			if (prop === 'innerHTML' && v === '') t.children.length = 0;
			t[prop] = v;
			return true;
		},
	});
}

function makeFragment() {
	const frag = {
		nodeType: 11,
		children: [],
		appendChild(c) {
			frag.children.push(c);
			return c;
		},
	};
	return frag;
}

function makeSandbox() {
	const elements = new Map();
	const winTarget = new EventTarget();
	const docTarget = new EventTarget();
	const sandbox: Record<string, unknown> = {
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
		setTimeout: () => 0,
		clearTimeout: () => undefined,
		setInterval: () => 0,
		clearInterval: () => undefined,
		// No network in this harness: every boot fetch simply never settles.
		fetch: () => new Promise(() => {}),
		document: new Proxy(
			{},
			{
				get(_t, prop) {
					switch (prop) {
						case 'getElementById':
							return (id) => {
								if (!elements.has(id)) elements.set(id, makeNode('div'));
								return elements.get(id);
							};
						case 'createElement':
							return (tag) => makeNode(tag);
						case 'createTextNode':
							return (s) => ({ nodeType: 3, data: String(s) });
						case 'createDocumentFragment':
							return () => makeFragment();
						case 'querySelectorAll':
							return () => [];
						case 'querySelector':
							return () => makeNode('div');
						case 'addEventListener':
							return docTarget.addEventListener.bind(docTarget);
						case 'removeEventListener':
							return docTarget.removeEventListener.bind(docTarget);
						case 'dispatchEvent':
							return docTarget.dispatchEvent.bind(docTarget);
						case 'body':
							return makeNode('body');
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
	return sandbox;
}

const sandbox = makeSandbox();
const linkifyGH = sandbox.linkifyGH as (t: string, o?: { bareRepo?: boolean }) => Node;
const linkifyTitle = sandbox.linkifyTitle as (t: string) => Node;

// -- Serializers over the produced tree -------------------------------------------

/** Depth-first leaves: text nodes and <a> elements, in document order. */
function leaves(node: Node, out: Node[] = []): Node[] {
	if (node.nodeType === 3 || node.tag === 'a') out.push(node);
	else for (const c of node.children || []) leaves(c, out);
	return out;
}

/** The rendered text — what an operator actually reads. */
function textOf(node: Node): string {
	return leaves(node)
		.map((n) => (n.nodeType === 3 ? n.data : textOf({ nodeType: 1, children: n.children } as Node)))
		.join('');
}

function linksOf(node: Node): Array<{ href: string; label: string; target: string; rel: string }> {
	return leaves(node)
		.filter((n) => n.tag === 'a')
		.map((a) => ({
			href: a.attrs.href,
			label: (a.children[0] || {}).data,
			target: a.attrs.target,
			rel: a.attrs.rel,
		}));
}

// -- The tests ---------------------------------------------------------------------

test('the loaded dashboard exposes the linkifiers', () => {
	assert.equal(typeof linkifyGH, 'function');
	assert.equal(typeof linkifyTitle, 'function');
});

test('owner/repo#N becomes one new-tab link to the issues URL', () => {
	const frag = linkifyGH('wow-look-at-my/go-s3-server#41');
	const links = linksOf(frag);
	assert.equal(links.length, 1);
	assert.deepEqual(links[0], {
		href: 'https://github.com/wow-look-at-my/go-s3-server/issues/41',
		label: 'wow-look-at-my/go-s3-server#41',
		// GitHub redirects /issues/N to /pull/N when N is a PR, so the issues
		// form covers both without the dashboard knowing which it is.
		target: '_blank',
		rel: 'noopener noreferrer',
	});
});

test('a slug click never also fires the row behind it', () => {
	const a = leaves(linkifyGH('wow-look-at-my/webhooks#7')).find((n) => n.tag === 'a');
	assert.ok(a, 'expected a link');
	assert.ok(a.listeners.includes('click'), 'the link must stop the row click from propagating');
});

test('surrounding text survives verbatim, as text nodes', () => {
	const line = 'wow-look-at-my/webhooks#41: reconciled (2 checks) — see wow-look-at-my/webhook-runner#116.';
	const frag = linkifyGH(line);
	assert.equal(textOf(frag), line, 'the rendered text must equal the input exactly');
	assert.deepEqual(
		linksOf(frag).map((l) => l.label),
		['wow-look-at-my/webhooks#41', 'wow-look-at-my/webhook-runner#116'],
	);
});

test('markup in the text stays text — nothing is ever parsed as HTML', () => {
	const nasty = '<img src=x onerror=alert(1)> wow-look-at-my/webhooks#1';
	const frag = linkifyGH(nasty);
	assert.equal(textOf(frag), nasty);
	const first = leaves(frag)[0];
	assert.equal(first.nodeType, 3, 'the markup must land in a TEXT node');
	assert.equal(first.data, '<img src=x onerror=alert(1)> ');
});

test('paths and prose that only look like slugs are left alone', () => {
	for (const s of [
		'src/hooks/pr-minder/hook.json',
		'internal/server/dashboard',
		'POST /repos/wow-look-at-my/webhooks/pulls',
		'24/7',
		'2026/07',
		'wow-look-at-my/webhooks#abc',
	]) {
		assert.deepEqual(linksOf(linkifyTitle(s)), [], `must not linkify: ${s}`);
		assert.equal(textOf(linkifyTitle(s)), s);
	}
});

test('a bare URL becomes exactly one link to itself', () => {
	// The inner "wow-look-at-my/webhooks" must NOT also become a slug link:
	// URLs are matched first, so the whole URL is consumed as one unit.
	const url = 'https://github.com/wow-look-at-my/webhooks/pull/41';
	for (const frag of [linkifyGH(url), linkifyTitle(url)]) {
		assert.deepEqual(linksOf(frag).map((l) => [l.label, l.href]), [[url, url]]);
		assert.equal(textOf(frag), url, 'the URL must render verbatim');
	}
});

test('a URL link opens in a new tab and swallows the row click', () => {
	const a = leaves(linkifyGH('see https://github.com/o/r/commit/abc/checks')).find((n) => n.tag === 'a');
	assert.ok(a, 'expected a link');
	assert.equal(a.attrs.target, '_blank');
	assert.equal(a.attrs.rel, 'noopener noreferrer');
	assert.ok(a.listeners.includes('click'), 'the link must stop the row click from propagating');
});

test('trailing sentence punctuation stays out of the URL', () => {
	for (const [line, want] of [
		['see https://github.com/o/r/commit/abc/checks.', 'https://github.com/o/r/commit/abc/checks'],
		['(https://github.com/o/r/commit/abc/checks)', 'https://github.com/o/r/commit/abc/checks'],
		['https://github.com/o/r/commit/abc/checks, then retry', 'https://github.com/o/r/commit/abc/checks'],
	] as Array<[string, string]>) {
		const frag = linkifyGH(line);
		assert.deepEqual(linksOf(frag).map((l) => l.href), [want], `bad URL boundary in: ${line}`);
		assert.equal(textOf(frag), line, 'the surrounding text must survive verbatim');
	}
});

test("the reload gate's held-commit message renders its run-details link", () => {
	// The exact shape internal/reloadgate stamps via Gate.withChecks — an
	// operator reading the red banner clicks straight through to the CI run
	// instead of hand-assembling the URL from a 12-char abbreviation.
	const sha = '7ba043715036c8a9f0d1e2b3a4c5d6e7f8091a2b';
	const msg =
		`all-builds failure for 7ba043715036; serving 2077c6b3a03f unchanged` +
		` — https://github.com/wow-look-at-my/webhooks/commit/${sha}/checks`;
	const frag = linkifyGH(msg);
	assert.deepEqual(
		linksOf(frag).map((l) => l.href),
		[`https://github.com/wow-look-at-my/webhooks/commit/${sha}/checks`],
	);
	assert.equal(textOf(frag), msg, 'the message must still read exactly as stamped');
});

test('a bare owner/repo links in titles only', () => {
	// A run title ("wow-look-at-my/webhooks · CI / build") is GitHub-derived.
	const title = linkifyTitle('wow-look-at-my/webhooks · CI / build');
	assert.deepEqual(linksOf(title).map((l) => [l.label, l.href]), [
		['wow-look-at-my/webhooks', 'https://github.com/wow-look-at-my/webhooks'],
	]);
	// The same string in free-form text (a log line) stays plain: "and/or"
	// and "true/false" are indistinguishable from a repo slug there.
	assert.deepEqual(linksOf(linkifyGH('wow-look-at-my/webhooks · CI / build')), []);
	for (const s of ['read/write and/or true/false', 'building image for hooks/gha-runner', 'docker/dockerd exited']) {
		assert.deepEqual(linksOf(linkifyGH(s)), [], `free-form text must stay plain: ${s}`);
		assert.equal(textOf(linkifyGH(s)), s);
	}
});

test('a #N slug links in free-form text too, title mode or not', () => {
	for (const frag of [linkifyGH('pr-describe wow-look-at-my/webhooks#41 done'), linkifyTitle('wow-look-at-my/webhooks#41')]) {
		assert.deepEqual(
			linksOf(frag).map((l) => l.href),
			['https://github.com/wow-look-at-my/webhooks/issues/41'],
		);
	}
});

// -- Wiring: the renderers actually call it ---------------------------------

test('a run row renders its title with the slug clickable', () => {
	const td = (sandbox.runCell as (r: unknown) => Node)({
		id: 'abcdefghij234567abcdefghij',
		title: 'wow-look-at-my/go-s3-server#41',
		status: 'success',
	});
	assert.deepEqual(
		linksOf(td).map((l) => l.href),
		['https://github.com/wow-look-at-my/go-s3-server/issues/41'],
		'the runs tables must render the title as a link',
	);
	// The run id still rides along as the (unlinked) second line.
	assert.match(textOf(td), /abcdefghij234567abcdefghij/);
});

test('the raw log view renders each line with its slugs clickable', () => {
	// Driven through the modal's real renderer, so the wiring under test is
	// the one the dashboard uses (the log lines live in page-local state).
	(sandbox.renderRunDetail as (r: unknown, open: boolean) => void)(
		{
			id: 'abcdefghij234567abcdefghij',
			hook_id: 'pr-minder',
			status: 'success',
			exit_code: 0,
			started: '2026-07-21T00:00:00Z',
			output: ['wow-look-at-my/webhooks#41: opened', 'writing src/hooks/pr-minder/state.json'],
			output_times: ['2026-07-21T00:00:01Z', '2026-07-21T00:00:02Z'],
		},
		false,
	);
	const out = (sandbox.document as { getElementById(id: string): Node }).getElementById('run-detail-output');
	assert.deepEqual(
		linksOf(out).map((l) => l.href),
		['https://github.com/wow-look-at-my/webhooks/issues/41'],
		'exactly the slug links — the file path must stay plain text',
	);
});

test('empty and null render nothing rather than throwing', () => {
	assert.equal(textOf(linkifyGH('')), '');
	assert.equal(textOf(linkifyGH(null as unknown as string)), '');
	assert.deepEqual(linksOf(linkifyGH(undefined as unknown as string)), []);
});
