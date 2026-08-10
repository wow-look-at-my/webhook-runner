// The settings editor's RENDERING and its writes, against a minimal fake DOM.
//
// These pin the promises an operator actually experiences, none of which the
// model tests can see: that an enum's per-value prose is both a tooltip AND
// on the page, that an overridden field says so and offers a revert naming
// the manifest value, that a pin the manifest dropped is visible enough to
// remove, and — the one that matters most — that changing a control sends
// exactly one write with the right pointer while an invalid one sends none.
//
// Run: node --test internal/server/dashboard/testjs/*.test.ts

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';

// ---- a fake DOM, only as deep as the form actually goes --------------------

class FakeElement {
	tag: string;
	attrs: Record<string, unknown> = {};
	children: FakeElement[] = [];
	text = '';
	listeners: Record<string, Array<(e?: unknown) => void>> = {};
	hidden = false;
	value = '';
	type = '';
	title = '';
	checked = false;
	selected = false;
	min = '';
	max = '';
	className = '';
	classList = {
		add: (c: string) => {
			this.className = `${this.className} ${c}`.trim();
		},
		remove: (c: string) => {
			this.className = this.className.split(' ').filter((x) => x !== c).join(' ');
		},
	};

	constructor(tag: string) {
		this.tag = tag;
	}
	get textContent(): string {
		return this.text;
	}
	set textContent(v: string) {
		this.text = v;
		if (v === '') this.children = [];
	}
	appendChild(child: FakeElement): FakeElement {
		this.children.push(child);
		return child;
	}
	addEventListener(name: string, fn: (e?: unknown) => void): void {
		(this.listeners[name] ??= []).push(fn);
	}
	fire(name: string, e?: unknown): void {
		for (const fn of this.listeners[name] ?? []) fn(e);
	}
	blur(): void {
		this.fire('change');
	}
	/** Depth-first walk; supports the ".class" selectors the form uses. */
	querySelector(sel: string): FakeElement | null {
		return this.all().find((n) => n.matches(sel)) ?? null;
	}
	matches(sel: string): boolean {
		if (sel.startsWith('.')) {
			const want = sel.slice(1);
			const cls = String(this.attrs.class ?? '').split(' ').concat(this.className.split(' '));
			return cls.includes(want);
		}
		return this.tag === sel;
	}
	all(): FakeElement[] {
		return this.children.flatMap((c) => [c, ...c.all()]);
	}
	find(sel: string): FakeElement[] {
		return this.all().filter((n) => n.matches(sel));
	}
	/** Every bit of text under this node, for "does the page say X" checks. */
	fullText(): string {
		return [this.text, ...this.children.map((c) => c.fullText())].join(' ');
	}
}

function fakeEl(tag: string, attrs?: Record<string, unknown> | null, ...children: unknown[]): FakeElement {
	const node = new FakeElement(tag);
	for (const [k, v] of Object.entries(attrs ?? {})) {
		node.attrs[k] = v;
		if (k === 'type') node.type = String(v);
		if (k === 'value') node.value = String(v);
		if (k === 'title') node.title = String(v);
		if (k === 'hidden') node.hidden = true;
		if (k === 'class') node.className = String(v);
	}
	for (const c of children) {
		if (c === null || c === undefined) continue;
		if (c instanceof FakeElement) node.appendChild(c);
		else node.text += String(c);
	}
	return node;
}

interface Sent {
	url: string;
	method: string;
	body: unknown;
}
let sent: Sent[] = [];
let nextResponse: unknown = null;

beforeEach(() => {
	sent = [];
	nextResponse = { hook: 'h', effective: {}, overrides: [] };
	(globalThis as Record<string, unknown>).el = fakeEl;
	(globalThis as Record<string, unknown>).window = { setTimeout: () => 0 };
	(globalThis as Record<string, unknown>).fetch = async (url: string, init: Record<string, unknown>) => {
		sent.push({ url, method: String(init?.method ?? 'GET'), body: init?.body ? JSON.parse(String(init.body)) : undefined });
		return { ok: true, status: 200, statusText: 'OK', json: async () => nextResponse };
	};
});

const { renderSettings } = await import('../ts/settingsform.ts');

const schema = {
	type: 'object',
	properties: {
		ai: {
			type: 'object',
			properties: {
				model: { type: 'string', description: 'Gateway alias.' },
				reasoning: {
					description: 'Whether to ask the gateway to think.',
					oneOf: [
						{ const: 'auto', title: 'auto', description: 'Take the model default.' },
						{ const: 'off', title: 'off', description: 'Ask the backend not to think.' },
					],
				},
				retries: { type: 'integer', minimum: 0, maximum: 10 },
			},
		},
		token: { type: 'string', writeOnly: true },
	},
};

const view = {
	hook: 'h',
	schema,
	effective: { ai: { model: 'm', reasoning: 'auto', retries: 3 }, token: 't' },
	overrides: [],
};

const render = (v: unknown = view) => renderSettings(v as never, () => {}) as unknown as FakeElement;

test('every field carries its schema description', () => {
	const out = render();
	assert.match(out.fullText(), /Gateway alias\./);
	assert.match(out.fullText(), /Whether to ask the gateway to think\./);
});

// The user-facing point of enums: a tooltip per option, and the chosen one's
// prose visible without hovering anything.
test('enum options carry their descriptions as tooltips', () => {
	const options = render().find('option');
	assert.deepEqual(options.map((o) => o.textContent), ['auto', 'off']);
	assert.equal(options[0].title, 'Take the model default.');
	assert.equal(options[1].title, 'Ask the backend not to think.');
});

test('the SELECTED enum value has its description on the page', () => {
	const out = render();
	const desc = out.querySelector('.setting-choice-desc')!;
	assert.equal(desc.textContent, 'Take the model default.');
	assert.equal(desc.hidden, false);
});

test('choosing another enum value swaps the shown description and writes once', () => {
	const out = render();
	const select = out.find('select')[0];
	select.value = '1'; // the second choice
	select.fire('change');

	assert.equal(out.querySelector('.setting-choice-desc')!.textContent, 'Ask the backend not to think.');
	assert.equal(sent.length, 1);
	assert.equal(sent[0].method, 'PUT');
	assert.equal(sent[0].url, '/hooks/h/settings');
	assert.deepEqual(sent[0].body, { pointer: '/ai/reasoning', value: 'off' });
});

test('a bounded integer gets a slider AND a box, both carrying the bounds', () => {
	const out = render();
	const slider = out.querySelector('.setting-slider')!;
	assert.equal(slider.attrs.min, '0');
	assert.equal(slider.attrs.max, '10');
	assert.equal(slider.attrs.step, '1');
	const box = out.querySelector('.setting-number-box')!;
	assert.equal(box.min, '0');
	assert.equal(box.max, '10');
	assert.equal(box.value, '3');
});

test('an unbounded number gets no slider — a slider with no ends is a lie', () => {
	const out = render({
		hook: 'h', effective: { n: 1 }, overrides: [],
		schema: { type: 'object', properties: { n: { type: 'integer' } } },
	});
	assert.equal(out.querySelector('.setting-slider'), null);
	assert.notEqual(out.querySelector('.setting-number-box'), null);
});

test('a writeOnly field is masked and offers a reveal', () => {
	const out = render();
	const secret = out.find('.setting-input').find((n) => n.type === 'password');
	assert.notEqual(secret, undefined);
	assert.notEqual(out.querySelector('.setting-reveal'), null);
});

// A value the schema forbids must never reach the server: the operator gets
// the reason on the field instead of a round trip and a validation error.
test('an out-of-range value is refused locally and sends nothing', () => {
	const out = render();
	const box = out.querySelector('.setting-number-box')!;
	box.value = '99';
	box.fire('change');

	assert.equal(sent.length, 0, 'no write left the page');
	const status = out.find('.setting-status').find((n) => !n.hidden)!;
	assert.match(status.textContent, /at most 10/);
});

test('a valid number writes the pointer and a real number, not a string', () => {
	const out = render();
	const box = out.querySelector('.setting-number-box')!;
	box.value = '7';
	box.fire('change');
	assert.deepEqual(sent[0].body, { pointer: '/ai/retries', value: 7 });
});

test('an overridden field is badged and its revert names the manifest value', () => {
	const out = render({
		...view,
		effective: { ai: { model: 'm', reasoning: 'off', retries: 3 }, token: 't' },
		overrides: [{ pointer: '/ai/reasoning', value: 'off', manifest: 'auto' }],
	});
	const badge = out.querySelector('.setting-badge')!;
	assert.equal(badge.textContent, 'overridden');
	const revert = out.querySelector('.setting-revert')!;
	assert.match(revert.title, /revert to the manifest value: auto/);
});

test('revert DELETEs exactly that pointer', () => {
	const out = render({
		...view,
		overrides: [{ pointer: '/ai/reasoning', value: 'off', manifest: 'auto' }],
	});
	out.querySelector('.setting-revert')!.fire('click');
	assert.equal(sent.length, 1);
	assert.equal(sent[0].method, 'DELETE');
	assert.match(sent[0].url, /pointer=%2Fai%2Freasoning/);
});

// A stale pin does nothing at all, so it must not read as a live setting —
// and it needs to be removable, which means visible.
test('a stale pin is badged as stale and listed for removal', () => {
	const out = render({
		hook: 'h',
		schema,
		effective: { ai: { model: 'm', retries: 3 }, token: 't' },
		overrides: [{ pointer: '/ai/reasoning', value: 'off', stale: true }],
		rejected: 'override /ai/reasoning: the manifest has no field',
	});
	assert.equal(out.querySelector('.setting-badge-stale')!.textContent, 'stale override');
	assert.notEqual(out.querySelector('.setting-orphans'), null);
	assert.match(out.fullText(), /no longer declares/);
});

test('a refused override says so at the top, in the warning voice', () => {
	const out = render({ ...view, rejected: 'settings does not match settings.schema.json' });
	const line = out.querySelector('.attention-line')!;
	assert.match(line.textContent, /REFUSED at the last load/);
	assert.match(line.textContent, /settings does not match/);
});

test('a hook with no schema says it takes no configuration', () => {
	const out = render({ hook: 'h', effective: {}, overrides: [] });
	assert.match(out.fullText(), /ships no settings\.schema\.json/);
	assert.equal(out.querySelector('.setting-row'), null);
});

test('a schema with no properties says so rather than rendering an empty form', () => {
	const out = render({ hook: 'h', schema: { type: 'object' }, effective: {}, overrides: [] });
	assert.match(out.fullText(), /no editable properties/);
});

test('a server refusal is shown on the field, not swallowed', async () => {
	(globalThis as Record<string, unknown>).fetch = async () => ({
		ok: false, status: 400, statusText: 'Bad Request',
		json: async () => ({ error: 'settings does not match settings.schema.json' }),
	});
	const out = render();
	const select = out.find('select')[0];
	select.value = '1';
	select.fire('change');
	await new Promise((r) => setImmediate(r));

	const status = out.find('.setting-status').find((n) => !n.hidden)!;
	assert.match(status.textContent, /settings does not match/);
});

// An object is a GROUP, not a setting: it becomes a card with a header, and
// only its leaves get rows. A row for the object itself would be a line with
// a name and nothing to set.
test('an object becomes a card and only leaves get rows', () => {
	const out = render();
	const pointers = out.find('.setting-row').map((r) => r.attrs['data-pointer']);
	assert.deepEqual(pointers, ['/ai/model', '/ai/reasoning', '/ai/retries', '/token']);
	assert.equal(out.find('.setting-card').length, 2, 'one card for the ai object, one for the loose token');
	assert.match(out.querySelector('.setting-card-title')!.textContent, /ai/);
});

// Every row is the two-column layout: what it is on the left, the control on
// the right. Stacking them in one column is what made the first cut read as
// an undifferentiated list.
test('each row splits into a text column and a control column', () => {
	const row = render().find('.setting-row')[0];
	assert.notEqual(row.querySelector('.setting-text'), null);
	assert.notEqual(row.querySelector('.setting-field'), null);
	// The description belongs with the name, the control with the value.
	assert.notEqual(row.querySelector('.setting-text')!.querySelector('.setting-desc'), null);
	assert.notEqual(row.querySelector('.setting-field')!.querySelector('.setting-input'), null);
});
