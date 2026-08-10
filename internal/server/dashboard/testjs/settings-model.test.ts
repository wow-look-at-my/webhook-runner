// The settings editor's MODEL: schema -> form fields, the two enum-description
// conventions, and the client-side validation that mirrors what the runner
// will enforce. Pure functions, so no DOM and no sandbox — the rendering half
// is pinned separately in settings-render.test.ts.
//
// What these protect: the form is generated from a hook's own
// settings.schema.json, so a schema construct this file mis-reads becomes a
// control that offers values the loader rejects, or — worse — a field that
// silently does not appear at all.
//
// Run: node --test internal/server/dashboard/testjs/*.test.ts

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
	choicesOf,
	pointerToken,
	schemaFields,
	summarize,
	validateValue,
	valueAt,
	checkValue,
	type Field,
	type JSONSchema,
} from '../ts/settings.ts';

const schema: JSONSchema = {
	type: 'object',
	required: ['ai'],
	properties: {
		ai: {
			type: 'object',
			title: 'Model gateway',
			description: 'Where generations are sent.',
			required: ['model'],
			properties: {
				model: { type: 'string', minLength: 1, description: 'Gateway alias.' },
				reasoning: {
					description: 'Whether to ask the gateway to think.',
					oneOf: [
						{ const: 'auto', title: 'auto', description: 'Take the model default.' },
						{ const: 'off', title: 'off', description: 'Ask the backend not to think.' },
					],
				},
				retries: { type: 'integer', minimum: 0, maximum: 10, description: 'Flat retry legs.' },
			},
		},
		enabled: { type: 'boolean' },
		tags: { type: 'array', items: { type: 'string' } },
		token: { type: 'string', writeOnly: true },
		weird: { anyOf: [{ type: 'string' }, { type: 'number' }] },
	},
};

const doc = {
	ai: { model: 'm', reasoning: 'auto', retries: 3 },
	enabled: true,
	tags: ['a', 'b'],
	token: 'sekrit',
	weird: { deep: 1 },
};

function byPointer(fields: Field[], pointer: string): Field {
	for (const f of fields) {
		if (f.pointer === pointer) return f;
		if (f.children) {
			const hit = f.children.find((c) => c.pointer === pointer);
			if (hit) return hit;
		}
	}
	throw new Error(`no field at ${pointer}`);
}

test('every declared property becomes a field, nested ones included', () => {
	const fields = schemaFields(schema, doc);
	assert.deepEqual(fields.map((f) => f.pointer), ['/ai', '/enabled', '/tags', '/token', '/weird']);
	assert.deepEqual(byPointer(fields, '/ai').children?.map((f) => f.pointer),
		['/ai/model', '/ai/reasoning', '/ai/retries']);
});

test('field order follows the SCHEMA, not the document', () => {
	// The document deliberately lists its keys in a different order.
	const shuffled = { weird: 1, token: 't', tags: [], enabled: false, ai: { model: 'm' } };
	assert.deepEqual(schemaFields(schema, shuffled).map((f) => f.key),
		['ai', 'enabled', 'tags', 'token', 'weird'],
		'the schema is authored, the document is serialized — read down the authored order');
});

test('kinds are derived from the schema, and values come from the document', () => {
	const fields = schemaFields(schema, doc);
	assert.equal(byPointer(fields, '/ai').kind, 'object');
	assert.equal(byPointer(fields, '/ai/model').kind, 'string');
	assert.equal(byPointer(fields, '/ai/model').value, 'm');
	assert.equal(byPointer(fields, '/ai/reasoning').kind, 'enum');
	assert.equal(byPointer(fields, '/ai/retries').kind, 'integer');
	assert.equal(byPointer(fields, '/enabled').kind, 'boolean');
	assert.equal(byPointer(fields, '/tags').kind, 'array');
	assert.equal(byPointer(fields, '/token').kind, 'string');
});

test('an unmodeled construct falls back to a raw JSON editor rather than vanishing', () => {
	const fields = schemaFields(schema, doc);
	assert.equal(byPointer(fields, '/weird').kind, 'raw',
		'a field the form cannot model must still be reachable');
});

test('an array of objects is raw, an array of primitives is a list', () => {
	const s: JSONSchema = {
		type: 'object',
		properties: {
			people: { type: 'array', items: { type: 'object', properties: {} } },
			names: { type: 'array', items: { type: 'string' } },
		},
	};
	const fields = schemaFields(s, { people: [], names: [] });
	assert.equal(byPointer(fields, '/people').kind, 'raw');
	assert.equal(byPointer(fields, '/names').kind, 'array');
});

test('required and bounds reach the field', () => {
	const fields = schemaFields(schema, doc);
	assert.equal(byPointer(fields, '/ai').required, true);
	assert.equal(byPointer(fields, '/enabled').required, false);
	const retries = byPointer(fields, '/ai/retries');
	assert.equal(retries.min, 0);
	assert.equal(retries.max, 10);
	assert.equal(retries.step, 1, 'integers step by 1 so the slider cannot land between values');
});

test('writeOnly and credential-shaped names are masked', () => {
	const fields = schemaFields(schema, doc);
	assert.equal(byPointer(fields, '/token').secret, true);
	assert.equal(byPointer(fields, '/ai/model').secret, false);
	const named = schemaFields({ type: 'object', properties: { api_key: { type: 'string' } } }, {});
	assert.equal(named[0].secret, true);
});

// Per-value prose is the whole reason enums are worth special-casing: the
// operator has to know what they are choosing, not just that it is allowed.
test('enum descriptions are read from oneOf/const', () => {
	const choices = choicesOf(schema.properties!.ai.properties!.reasoning)!;
	assert.deepEqual(choices, [
		{ value: 'auto', label: 'auto', description: 'Take the model default.' },
		{ value: 'off', label: 'off', description: 'Ask the backend not to think.' },
	]);
});

test('enum descriptions are also read from enum + enumDescriptions', () => {
	const choices = choicesOf({ enum: ['a', 'b'], enumDescriptions: ['first', 'second'] })!;
	assert.deepEqual(choices.map((c) => [c.value, c.description]), [['a', 'first'], ['b', 'second']]);
});

test('a bare enum still yields choices, just without prose', () => {
	const choices = choicesOf({ enum: [1, 2] })!;
	assert.deepEqual(choices.map((c) => c.value), [1, 2]);
	assert.equal(choices[0].description, undefined);
});

test('anyOf that is not a closed const set is not an enum', () => {
	assert.equal(choicesOf({ anyOf: [{ type: 'string' }, { type: 'number' }] }), undefined);
});

// ---- validation ------------------------------------------------------------

test('validation mirrors the numeric constraints the runner enforces', () => {
	const retries = byPointer(schemaFields(schema, doc), '/ai/retries');
	assert.equal(validateValue(retries, 5), null);
	assert.match(validateValue(retries, 11)!, /at most 10/);
	assert.match(validateValue(retries, -1)!, /at least 0/);
	assert.match(validateValue(retries, 2.5)!, /whole number/);
	assert.match(validateValue(retries, 'x' as unknown)!, /must be a number/);
});

test('validation mirrors the string constraints', () => {
	const model = byPointer(schemaFields(schema, doc), '/ai/model');
	assert.equal(validateValue(model, 'gpt'), null);
	assert.match(validateValue(model, '')!, /must not be empty/);
});

test('a pattern is checked, and a malformed one defers to the server', () => {
	const ok = schemaFields({ type: 'object', properties: { s: { type: 'string', pattern: '^ab+$' } } }, {});
	assert.equal(validateValue(ok[0], 'abb'), null);
	assert.match(validateValue(ok[0], 'zz')!, /must match/);

	const broken = schemaFields({ type: 'object', properties: { s: { type: 'string', pattern: '([' } } }, {});
	assert.equal(validateValue(broken[0], 'anything'), null,
		'an uncheckable pattern must not block the field — the server has the real validator');
});

test('an enum refuses a value outside its choices', () => {
	const reasoning = byPointer(schemaFields(schema, doc), '/ai/reasoning');
	assert.equal(validateValue(reasoning, 'off'), null);
	assert.match(validateValue(reasoning, 'sometimes')!, /one of the listed/);
});

test('a required field refuses being cleared, an optional one allows it', () => {
	const fields = schemaFields(schema, doc);
	assert.match(validateValue(byPointer(fields, '/ai'), undefined)!, /required/);
	assert.equal(validateValue(byPointer(fields, '/enabled'), undefined), null);
});

// The server refuses these too; catching them here means the operator learns
// before the round trip, with an explanation instead of a validation error.
test('reference syntax is refused with an explanation', () => {
	const model = byPointer(schemaFields(schema, doc), '/ai/model');
	assert.match(checkValue(model, '${env:MODEL}')!, /literal value/);
	assert.match(checkValue(model, 'x${settings:a}y')!, /literal value/);
	assert.equal(checkValue(model, 'plain'), null);
});

// ---- pointers --------------------------------------------------------------

test('pointer tokens escape in the order RFC 6901 requires', () => {
	assert.equal(pointerToken('a/b'), 'a~1b');
	assert.equal(pointerToken('a~b'), 'a~0b');
	assert.equal(pointerToken('a~1b'), 'a~01b', '"~" is escaped before "/", so this round-trips');
});

test('a key containing a slash still addresses its own field', () => {
	const fields = schemaFields({ type: 'object', properties: { 'a/b': { type: 'string' } } }, { 'a/b': 'v' });
	assert.equal(fields[0].pointer, '/a~1b');
	assert.equal(fields[0].value, 'v');
});

test('valueAt walks objects and arrays and reports misses as undefined', () => {
	assert.equal(valueAt(doc, '/ai/model'), 'm');
	assert.equal(valueAt(doc, '/tags/1'), 'b');
	assert.equal(valueAt(doc, '/tags/9'), undefined);
	assert.equal(valueAt(doc, '/nope/deep'), undefined);
	assert.deepEqual(valueAt(doc, ''), doc);
});

// ---- robustness ------------------------------------------------------------

test('a missing, empty or property-less schema yields no fields instead of throwing', () => {
	assert.deepEqual(schemaFields(null, {}), []);
	assert.deepEqual(schemaFields(undefined, {}), []);
	assert.deepEqual(schemaFields({}, {}), []);
	assert.deepEqual(schemaFields({ type: 'object' }, {}), []);
});

test('a property whose schema is junk still produces a reachable field', () => {
	const fields = schemaFields({ type: 'object', properties: { x: null as unknown as JSONSchema } }, { x: 'v' });
	assert.equal(fields.length, 1);
	assert.equal(fields[0].kind, 'string', 'inferred from the value when the schema says nothing');
});

test('a nullable type still gets its real control', () => {
	const fields = schemaFields({ type: 'object', properties: { s: { type: ['string', 'null'] } } }, {});
	assert.equal(fields[0].kind, 'string');
});

test('summarize is compact and never leaks a secret', () => {
	assert.equal(summarize('v'), 'v');
	assert.equal(summarize(''), '(empty)');
	assert.equal(summarize(undefined), '—');
	assert.equal(summarize(3), '3');
	assert.equal(summarize([1, 2]), '[2 items]');
	assert.equal(summarize([1]), '[1 item]');
	assert.equal(summarize({ a: 1 }), '{1 field}');
	assert.equal(summarize('hunter2', true), '••••••••');
});
