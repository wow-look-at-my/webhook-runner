// The per-hook settings editor: a form built from the entity's own
// settings.schema.json, so the controls an operator sees are generated from
// the same document the loader validates against. Nothing about the form is
// described server-side — a second description would be a second schema, and
// the moment the two disagreed the UI would offer values the runner rejects.
//
// The model half (schemaFields, choicesOf, validateValue) is pure and lives
// apart from the DOM half so testjs can drive it under `node --test` without
// a browser. Everything here degrades rather than throws: a schema construct
// this file does not model becomes a raw-JSON editor, never a missing field
// and never an exception that blanks the section.

export interface JSONSchema {
	type?: string | string[];
	title?: string;
	description?: string;
	properties?: Record<string, JSONSchema>;
	required?: string[];
	items?: JSONSchema;
	enum?: unknown[];
	/** Parallel to `enum`, the VS Code convention for per-value prose. */
	enumDescriptions?: string[];
	oneOf?: JSONSchema[];
	anyOf?: JSONSchema[];
	const?: unknown;
	default?: unknown;
	minimum?: number;
	maximum?: number;
	exclusiveMinimum?: number;
	exclusiveMaximum?: number;
	multipleOf?: number;
	minLength?: number;
	maxLength?: number;
	pattern?: string;
	format?: string;
	/** JSON Schema's own "this is write-only" marker — masked in the UI. */
	writeOnly?: boolean;
	[k: string]: unknown;
}

export interface SettingsField {
	pointer: string;
	value: unknown;
	manifest?: unknown;
	stale?: boolean;
}

export interface SettingsView {
	hook: string;
	schema?: JSONSchema | null;
	effective: unknown;
	overrides?: SettingsField[];
	rejected?: string;
}

export type FieldKind = 'string' | 'number' | 'integer' | 'boolean' | 'enum' | 'array' | 'object' | 'raw';

export interface EnumChoice {
	value: unknown;
	label: string;
	/** Shown as the option's tooltip AND, for the selected one, on the page. */
	description?: string;
}

export interface Field {
	/** RFC 6901 pointer into the settings document — the API's write key. */
	pointer: string;
	key: string;
	title: string;
	description?: string;
	kind: FieldKind;
	schema: JSONSchema;
	value: unknown;
	required: boolean;
	choices?: EnumChoice[];
	min?: number;
	max?: number;
	step?: number;
	secret: boolean;
	/** Populated for kind 'object' — the form nests rather than flattens. */
	children?: Field[];
}

// ---- pointer plumbing (RFC 6901) -------------------------------------------

/** Escape order is specified: "~" -> ~0 BEFORE "/" -> ~1. */
export function pointerToken(key: string): string {
	return key.replace(/~/g, '~0').replace(/\//g, '~1');
}

export function valueAt(doc: unknown, pointer: string): unknown {
	if (pointer === '') return doc;
	let cur: unknown = doc;
	for (const raw of pointer.slice(1).split('/')) {
		const tok = raw.replace(/~1/g, '/').replace(/~0/g, '~');
		if (Array.isArray(cur)) {
			const i = Number(tok);
			if (!Number.isInteger(i) || i < 0 || i >= cur.length) return undefined;
			cur = cur[i];
		} else if (cur && typeof cur === 'object') {
			cur = (cur as Record<string, unknown>)[tok];
		} else {
			return undefined;
		}
	}
	return cur;
}

// ---- schema -> fields ------------------------------------------------------

/**
 * Per-value descriptions, from either convention:
 *   - `oneOf`/`anyOf` of {const, title?, description?} — canonical JSON Schema
 *   - `enum` + `enumDescriptions` — the parallel-array convention
 * Returns undefined when the schema declares no closed set of values.
 */
export function choicesOf(schema: JSONSchema): EnumChoice[] | undefined {
	const branches = schema.oneOf ?? schema.anyOf;
	if (Array.isArray(branches)) {
		const out: EnumChoice[] = [];
		for (const b of branches) {
			if (!b || typeof b !== 'object' || !('const' in b)) return undefined;
			out.push({
				value: b.const,
				label: b.title ?? String(b.const),
				description: b.description,
			});
		}
		if (out.length) return out;
	}
	if (Array.isArray(schema.enum) && schema.enum.length) {
		const descs = Array.isArray(schema.enumDescriptions) ? schema.enumDescriptions : [];
		return schema.enum.map((v, i) => ({ value: v, label: String(v), description: descs[i] }));
	}
	return undefined;
}

function schemaType(schema: JSONSchema): string | undefined {
	const t = schema.type;
	if (typeof t === 'string') return t;
	// A union type ("string" or "null") is only modelable when exactly one
	// non-null member remains; anything else falls back to raw JSON.
	if (Array.isArray(t)) {
		const real = t.filter((x) => x !== 'null');
		if (real.length === 1) return real[0];
	}
	return undefined;
}

function kindOf(schema: JSONSchema, value: unknown): FieldKind {
	if (choicesOf(schema)) return 'enum';
	switch (schemaType(schema)) {
		case 'string':
			return 'string';
		case 'integer':
			return 'integer';
		case 'number':
			return 'number';
		case 'boolean':
			return 'boolean';
		case 'object':
			return schema.properties ? 'object' : 'raw';
		case 'array':
			// Only a homogeneous array of primitives gets the list editor;
			// arrays of objects are honestly easier to edit as JSON than
			// through a nested widget nobody can keep track of.
			return isPrimitiveSchema(schema.items) ? 'array' : 'raw';
	}
	// No usable `type`. Infer from the current value so a schemaless-but-real
	// field still gets a sensible control instead of a JSON blob.
	if (typeof value === 'boolean') return 'boolean';
	if (typeof value === 'number') return Number.isInteger(value) ? 'integer' : 'number';
	if (typeof value === 'string') return 'string';
	return 'raw';
}

function isPrimitiveSchema(schema: JSONSchema | undefined): boolean {
	if (!schema) return false;
	const t = schemaType(schema);
	return t === 'string' || t === 'number' || t === 'integer' || t === 'boolean';
}

/**
 * A field is treated as secret when the schema says writeOnly, or when its
 * name is one of the unmistakable credential words. The masking is a
 * shoulder-surfing guard, not a security boundary: this port already serves
 * KV values and the manifest is plaintext in a private repo.
 */
function isSecret(key: string, schema: JSONSchema): boolean {
	if (schema.writeOnly === true) return true;
	return /^(token|secret|password|api_key|apikey|private_key)$/i.test(key);
}

function boundsOf(schema: JSONSchema): { min?: number; max?: number; step?: number } {
	const min = typeof schema.minimum === 'number'
		? schema.minimum
		: typeof schema.exclusiveMinimum === 'number'
			? schema.exclusiveMinimum + (schemaType(schema) === 'integer' ? 1 : 0)
			: undefined;
	const max = typeof schema.maximum === 'number'
		? schema.maximum
		: typeof schema.exclusiveMaximum === 'number'
			? schema.exclusiveMaximum - (schemaType(schema) === 'integer' ? 1 : 0)
			: undefined;
	const step = typeof schema.multipleOf === 'number'
		? schema.multipleOf
		: schemaType(schema) === 'integer'
			? 1
			: undefined;
	return { min, max, step };
}

/**
 * Walk a schema against the effective document and produce the form model.
 * Ordering follows the SCHEMA's property order, not the document's: the
 * schema is authored, the document is serialized, and an operator reading
 * down the form should see the fields in the order someone chose.
 */
export function schemaFields(schema: JSONSchema | null | undefined, doc: unknown, base = ''): Field[] {
	if (!schema || typeof schema !== 'object' || !schema.properties) return [];
	const required = new Set(Array.isArray(schema.required) ? schema.required : []);
	const out: Field[] = [];
	for (const [key, propRaw] of Object.entries(schema.properties)) {
		const prop: JSONSchema = propRaw && typeof propRaw === 'object' ? propRaw : {};
		const pointer = `${base}/${pointerToken(key)}`;
		const value = valueAt(doc, pointer);
		const kind = kindOf(prop, value);
		const field: Field = {
			pointer,
			key,
			title: prop.title ?? key,
			description: prop.description,
			kind,
			schema: prop,
			value,
			required: required.has(key),
			secret: isSecret(key, prop),
			...boundsOf(prop),
		};
		if (kind === 'enum') field.choices = choicesOf(prop);
		if (kind === 'object') field.children = schemaFields(prop, doc, pointer);
		out.push(field);
	}
	return out;
}

// ---- client-side validation ------------------------------------------------

/**
 * Mirrors the constraints the runner will enforce, so a bad value is caught
 * as the operator types instead of after a round trip. The SERVER remains
 * authoritative — this never decides a value is good, only that it is
 * obviously bad.
 */
export function validateValue(field: Field, value: unknown): string | null {
	const s = field.schema;
	if (value === undefined) return field.required ? `${field.title} is required` : null;

	if (field.choices) {
		return field.choices.some((c) => c.value === value) ? null : 'pick one of the listed values';
	}
	if (field.kind === 'integer' || field.kind === 'number') {
		if (typeof value !== 'number' || Number.isNaN(value)) return 'must be a number';
		if (field.kind === 'integer' && !Number.isInteger(value)) return 'must be a whole number';
		if (typeof s.minimum === 'number' && value < s.minimum) return `must be at least ${s.minimum}`;
		if (typeof s.maximum === 'number' && value > s.maximum) return `must be at most ${s.maximum}`;
		if (typeof s.exclusiveMinimum === 'number' && value <= s.exclusiveMinimum) return `must be greater than ${s.exclusiveMinimum}`;
		if (typeof s.exclusiveMaximum === 'number' && value >= s.exclusiveMaximum) return `must be less than ${s.exclusiveMaximum}`;
		if (typeof s.multipleOf === 'number' && s.multipleOf > 0) {
			const q = value / s.multipleOf;
			if (Math.abs(q - Math.round(q)) > 1e-9) return `must be a multiple of ${s.multipleOf}`;
		}
		return null;
	}
	if (field.kind === 'string') {
		if (typeof value !== 'string') return 'must be text';
		if (typeof s.minLength === 'number' && value.length < s.minLength) {
			return s.minLength === 1 ? 'must not be empty' : `must be at least ${s.minLength} characters`;
		}
		if (typeof s.maxLength === 'number' && value.length > s.maxLength) return `must be at most ${s.maxLength} characters`;
		if (typeof s.pattern === 'string' && s.pattern) {
			// A schema pattern is ECMA-262 here, but a malformed one must not
			// take the form down — an uncheckable pattern just goes to the
			// server, which has the authoritative validator.
			try {
				if (!new RegExp(s.pattern).test(value)) return `must match ${s.pattern}`;
			} catch {
				return null;
			}
		}
		return null;
	}
	if (field.kind === 'boolean' && typeof value !== 'boolean') return 'must be true or false';
	return null;
}

/**
 * References resolve at load (`${settings:…}`) or at container start
 * (`${env:…}`), so an override carrying either would not be the value that
 * was typed. The server refuses them; saying so here means the operator
 * learns it before the round trip.
 */
export function referenceError(value: unknown): string | null {
	if (typeof value === 'string' && value.includes('${')) {
		return 'an override is a literal value — ${…} references belong in hook.json, where their resolution is reviewed';
	}
	if (value && typeof value === 'object' && JSON.stringify(value).includes('${')) {
		return 'an override is a literal value — ${…} references belong in hook.json, where their resolution is reviewed';
	}
	return null;
}

/** The one gate every save goes through. */
export function checkValue(field: Field, value: unknown): string | null {
	return referenceError(value) ?? validateValue(field, value);
}

// ---- summary line ----------------------------------------------------------

/** Compact one-line rendering of a value, for the collapsed/override chips. */
export function summarize(value: unknown, secret = false): string {
	if (value === undefined) return '—';
	if (secret) return '••••••••';
	if (typeof value === 'string') return value === '' ? '(empty)' : value;
	if (value === null || typeof value !== 'object') return String(value);
	if (Array.isArray(value)) return `[${value.length} item${value.length === 1 ? '' : 's'}]`;
	return `{${Object.keys(value as object).length} field${Object.keys(value as object).length === 1 ? '' : 's'}}`;
}
