// Renders the settings model (settings.ts) into the per-hook page and wires
// each control to the admin API. One field = one write: PUT pins it, DELETE
// reverts it, and the response carries the fresh effective document, so the
// page never has to guess what the server did with a value.
//
// Saving is deliberately per field and immediate. A form-wide Save button
// would let an operator queue three edits, hit one failure, and be left
// wondering which of the three landed — with per-field writes the answer is
// always on the field itself.

import {
	checkValue,
	schemaFields,
	summarize,
	valueAt,
	type Field,
	type SettingsField,
	type SettingsView,
} from './settings.ts';

/** Write path. fetchJSON (dashboard.js) is GET-only, so this is its sibling. */
async function sendJSON(url: string, method: string, body?: unknown): Promise<Response> {
	return fetch(url, {
		method,
		headers: body === undefined ? {} : { 'content-type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body),
	});
}

interface FormContext {
	hook: string;
	overrides: Map<string, SettingsField>;
	/** Re-render from a fresh view (every write answers with one). */
	apply: (view: SettingsView) => void;
}

/** Reads the error text the admin API writes, falling back to the status. */
async function errorText(res: Response): Promise<string> {
	try {
		const body = await res.json();
		if (body && typeof body === 'object' && typeof (body as { error?: string }).error === 'string') {
			return (body as { error: string }).error;
		}
	} catch {
		// Not JSON — fall through to the status line.
	}
	return `${res.status} ${res.statusText}`;
}

// ---- one field -------------------------------------------------------------

function fieldRow(field: Field, ctx: FormContext): HTMLElement {
	const row = el('div', { class: 'setting-row', 'data-pointer': field.pointer });
	const override = ctx.overrides.get(field.pointer);

	const label = el('label', { class: 'setting-label' }, field.title);
	if (field.required) label.appendChild(el('span', { class: 'setting-required', title: 'required by the schema' }, '*'));
	const head = el('div', { class: 'setting-head' }, label);
	if (override) {
		head.appendChild(el('span', {
			class: override.stale ? 'setting-badge setting-badge-stale' : 'setting-badge',
			title: override.stale
				? 'the manifest no longer declares this field, so this pin does nothing — revert it'
				: `overridden by you; hook.json says ${summarize(override.manifest, field.secret)}`,
		}, override.stale ? 'stale override' : 'overridden'));
		const revert = el('button', {
			class: 'setting-revert', type: 'button',
			title: override.stale
				? 'remove this pin'
				: `revert to the manifest value: ${summarize(override.manifest, field.secret)}`,
		}, 'revert');
		revert.addEventListener('click', () => {
			void writeField(row, ctx, field, undefined);
		});
		head.appendChild(revert);
	}
	row.appendChild(head);

	if (field.description) row.appendChild(el('p', { class: 'setting-desc' }, field.description));

	const status = el('p', { class: 'setting-status', hidden: 'hidden' });
	const commit = (value: unknown) => {
		const problem = checkValue(field, value);
		if (problem) {
			showStatus(status, problem, 'bad');
			return;
		}
		void writeField(row, ctx, field, value);
	};

	row.appendChild(control(field, commit, status));
	row.appendChild(status);
	return row;
}

function showStatus(status: HTMLElement, text: string, kind: 'ok' | 'bad'): void {
	status.textContent = text;
	status.className = `setting-status setting-status-${kind}`;
	status.hidden = false;
	if (kind === 'ok') {
		window.setTimeout(() => {
			status.hidden = true;
		}, 2500);
	}
}

/** PUT a value, or DELETE when value is undefined (revert). */
async function writeField(row: HTMLElement, ctx: FormContext, field: Field, value: unknown): Promise<void> {
	const status = row.querySelector('.setting-status') as HTMLElement | null;
	row.classList.add('setting-saving');
	try {
		const base = `/hooks/${encodeURIComponent(ctx.hook)}/settings`;
		const res = value === undefined
			? await sendJSON(`${base}?pointer=${encodeURIComponent(field.pointer)}`, 'DELETE')
			: await sendJSON(base, 'PUT', { pointer: field.pointer, value });
		if (!res.ok) {
			if (status) showStatus(status, await errorText(res), 'bad');
			return;
		}
		const view = (await res.json()) as SettingsView;
		ctx.apply(view);
	} catch (e) {
		if (status) showStatus(status, `could not reach the server: ${String(e)}`, 'bad');
	} finally {
		row.classList.remove('setting-saving');
	}
}

// ---- controls --------------------------------------------------------------

function control(field: Field, commit: (v: unknown) => void, status: HTMLElement): HTMLElement {
	switch (field.kind) {
		case 'enum':
			return enumControl(field, commit);
		case 'boolean':
			return booleanControl(field, commit);
		case 'integer':
		case 'number':
			return numberControl(field, commit);
		case 'array':
			return arrayControl(field, commit);
		case 'object':
			return objectControl(field);
		case 'string':
			return stringControl(field, commit);
		default:
			return rawControl(field, commit, status);
	}
}

/**
 * Enums get a real <select> whose options carry their own descriptions as
 * tooltips, plus the selected option's description rendered underneath —
 * a tooltip alone is invisible until you go hunting for it, and the whole
 * point of per-value prose is knowing what you are choosing.
 */
function enumControl(field: Field, commit: (v: unknown) => void): HTMLElement {
	const wrap = el('div', { class: 'setting-control' });
	const select = el('select', { class: 'setting-input' }) as HTMLSelectElement;
	const choices = field.choices ?? [];
	choices.forEach((choice, i) => {
		const opt = el('option', { value: String(i) }, choice.label) as HTMLOptionElement;
		if (choice.description) opt.title = choice.description;
		if (choice.value === field.value) opt.selected = true;
		select.appendChild(opt);
	});
	const chosen = el('p', { class: 'setting-choice-desc' });
	const paint = () => {
		const choice = choices[Number(select.value)];
		chosen.textContent = choice?.description ?? '';
		chosen.hidden = !choice?.description;
	};
	select.addEventListener('change', () => {
		paint();
		commit(choices[Number(select.value)]?.value);
	});
	paint();
	wrap.appendChild(select);
	wrap.appendChild(chosen);
	return wrap;
}

function booleanControl(field: Field, commit: (v: unknown) => void): HTMLElement {
	const wrap = el('div', { class: 'setting-control' });
	const box = el('input', { type: 'checkbox', class: 'setting-toggle' }) as HTMLInputElement;
	box.checked = field.value === true;
	const state = el('span', { class: 'setting-inline-value' }, box.checked ? 'on' : 'off');
	box.addEventListener('change', () => {
		state.textContent = box.checked ? 'on' : 'off';
		commit(box.checked);
	});
	wrap.appendChild(el('label', { class: 'setting-toggle-wrap' }, box, state));
	return wrap;
}

/**
 * A bounded number gets a slider AND a number box, kept in sync: the slider
 * makes the range obvious at a glance, the box makes an exact value typeable.
 * Unbounded numbers get the box alone — a slider with no ends is a lie.
 */
function numberControl(field: Field, commit: (v: unknown) => void): HTMLElement {
	const wrap = el('div', { class: 'setting-control setting-number' });
	const step = field.step ?? (field.kind === 'integer' ? 1 : 'any');
	const box = el('input', { type: 'number', class: 'setting-input setting-number-box', step: String(step) }) as HTMLInputElement;
	if (field.min !== undefined) box.min = String(field.min);
	if (field.max !== undefined) box.max = String(field.max);
	box.value = typeof field.value === 'number' ? String(field.value) : '';

	let slider: HTMLInputElement | null = null;
	if (field.min !== undefined && field.max !== undefined) {
		slider = el('input', {
			type: 'range', class: 'setting-slider',
			min: String(field.min), max: String(field.max), step: String(step),
		}) as HTMLInputElement;
		slider.value = box.value || String(field.min);
		slider.addEventListener('input', () => {
			box.value = slider!.value;
		});
		slider.addEventListener('change', () => commit(Number(slider!.value)));
		wrap.appendChild(el('span', { class: 'setting-bound' }, String(field.min)));
		wrap.appendChild(slider);
		wrap.appendChild(el('span', { class: 'setting-bound' }, String(field.max)));
	}
	box.addEventListener('input', () => {
		if (slider && box.value !== '') slider.value = box.value;
	});
	// Commit on blur and on Enter, never per keystroke: "3" on the way to
	// "30" is a legal value that would otherwise be saved and reloaded.
	box.addEventListener('change', () => commit(box.value === '' ? undefined : Number(box.value)));
	box.addEventListener('keydown', (e) => {
		if ((e as KeyboardEvent).key === 'Enter') box.blur();
	});
	wrap.appendChild(box);
	return wrap;
}

function stringControl(field: Field, commit: (v: unknown) => void): HTMLElement {
	const wrap = el('div', { class: 'setting-control' });
	const type = field.secret ? 'password' : field.schema.format === 'uri' ? 'url' : 'text';
	const input = el('input', { type, class: 'setting-input', spellcheck: 'false' }) as HTMLInputElement;
	input.value = typeof field.value === 'string' ? field.value : '';
	if (typeof field.schema.pattern === 'string') input.title = `must match ${field.schema.pattern}`;
	input.addEventListener('change', () => commit(input.value));
	input.addEventListener('keydown', (e) => {
		if ((e as KeyboardEvent).key === 'Enter') input.blur();
	});
	wrap.appendChild(input);
	if (field.secret) {
		const reveal = el('button', { type: 'button', class: 'setting-reveal', title: 'show or hide the value' }, 'show');
		reveal.addEventListener('click', () => {
			const hidden = input.type === 'password';
			input.type = hidden ? 'text' : 'password';
			reveal.textContent = hidden ? 'hide' : 'show';
		});
		wrap.appendChild(reveal);
	}
	return wrap;
}

/** A list of primitives: edit in place, remove, append. */
function arrayControl(field: Field, commit: (v: unknown) => void): HTMLElement {
	const wrap = el('div', { class: 'setting-control setting-array' });
	const items: unknown[] = Array.isArray(field.value) ? [...field.value] : [];
	const list = el('div', { class: 'setting-array-items' });
	const itemType = field.schema.items?.type;
	const coerce = (raw: string): unknown => {
		if (itemType === 'number' || itemType === 'integer') return raw === '' ? undefined : Number(raw);
		if (itemType === 'boolean') return raw === 'true';
		return raw;
	};

	const paint = () => {
		list.textContent = '';
		items.forEach((item, i) => {
			const input = el('input', {
				type: itemType === 'number' || itemType === 'integer' ? 'number' : 'text',
				class: 'setting-input', spellcheck: 'false',
			}) as HTMLInputElement;
			input.value = item === undefined || item === null ? '' : String(item);
			input.addEventListener('change', () => {
				items[i] = coerce(input.value);
				commit(items);
			});
			const drop = el('button', { type: 'button', class: 'setting-array-drop', title: 'remove this entry' }, '×');
			drop.addEventListener('click', () => {
				items.splice(i, 1);
				paint();
				commit(items);
			});
			list.appendChild(el('div', { class: 'setting-array-row' }, input, drop));
		});
	};
	paint();

	const add = el('button', { type: 'button', class: 'setting-array-add' }, '+ add');
	add.addEventListener('click', () => {
		items.push(itemType === 'number' || itemType === 'integer' ? 0 : itemType === 'boolean' ? false : '');
		paint();
	});
	wrap.appendChild(list);
	wrap.appendChild(add);
	return wrap;
}

/** Nested objects render as a nested group; their leaves are real fields. */
function objectControl(field: Field): HTMLElement {
	const wrap = el('div', { class: 'setting-control setting-object-note' });
	wrap.appendChild(el('span', { class: 'window-note' }, `${field.children?.length ?? 0} setting(s) below`));
	return wrap;
}

/**
 * Anything the form does not model — an array of objects, a composed schema,
 * a typeless field — is editable as JSON rather than hidden. A field the
 * operator cannot reach is worse than one they must type carefully.
 */
function rawControl(field: Field, commit: (v: unknown) => void, status: HTMLElement): HTMLElement {
	const wrap = el('div', { class: 'setting-control' });
	const area = el('textarea', { class: 'setting-input setting-json', rows: '4', spellcheck: 'false' }) as HTMLTextAreaElement;
	area.value = field.value === undefined ? '' : JSON.stringify(field.value, null, 2);
	area.addEventListener('change', () => {
		if (area.value.trim() === '') return;
		try {
			commit(JSON.parse(area.value));
		} catch (e) {
			showStatus(status, `not valid JSON: ${String(e)}`, 'bad');
		}
	});
	wrap.appendChild(area);
	wrap.appendChild(el('p', { class: 'window-note' }, 'edited as JSON — this field’s schema has no simpler control'));
	return wrap;
}

// ---- the section -----------------------------------------------------------

function renderGroup(fields: Field[], ctx: FormContext, depth = 0): HTMLElement {
	const group = el('div', { class: depth === 0 ? 'setting-group' : 'setting-group setting-group-nested' });
	for (const field of fields) {
		group.appendChild(fieldRow(field, ctx));
		if (field.kind === 'object' && field.children?.length) {
			group.appendChild(renderGroup(field.children, ctx, depth + 1));
		}
	}
	return group;
}

/** Build the whole section body for a view. Exported for the DOM tests. */
export function renderSettings(view: SettingsView, apply: (v: SettingsView) => void): HTMLElement {
	const body = el('div', { class: 'settings-body' });

	if (view.rejected) {
		body.appendChild(el('p', { class: 'attention-line' },
			`These overrides were REFUSED at the last load, so this hook is running its hook.json values: ${view.rejected}`));
	}
	if (!view.schema) {
		body.appendChild(el('p', { class: 'empty' },
			'This hook ships no settings.schema.json, so it takes no configuration.'));
		return body;
	}

	const ctx: FormContext = {
		hook: view.hook,
		overrides: new Map((view.overrides ?? []).map((o) => [o.pointer, o])),
		apply,
	};
	const fields = schemaFields(view.schema, view.effective);
	if (!fields.length) {
		body.appendChild(el('p', { class: 'empty' }, 'This hook’s schema declares no editable properties.'));
		return body;
	}

	body.appendChild(el('p', { class: 'window-note' },
		'Changes are saved as you make them and take effect on the next run. ' +
		'Each one pins a single field; everything else keeps coming from hook.json.'));
	body.appendChild(renderGroup(fields, ctx));

	// A pin whose field the manifest dropped has no row to live on, so it
	// would be invisible — and invisible is exactly how a stale override
	// survives forever. List them separately, each with its revert.
	const orphans = (view.overrides ?? []).filter((o) => valueAt(view.effective, o.pointer) === undefined && o.stale);
	if (orphans.length) {
		const box = el('div', { class: 'setting-orphans' });
		box.appendChild(el('h3', null, 'Pinned fields the manifest no longer declares'));
		for (const o of orphans) {
			const drop = el('button', { type: 'button', class: 'setting-revert' }, 'remove');
			drop.addEventListener('click', () => {
				void sendJSON(`/hooks/${encodeURIComponent(view.hook)}/settings?pointer=${encodeURIComponent(o.pointer)}`, 'DELETE')
					.then(async (res) => {
						if (res.ok) apply((await res.json()) as SettingsView);
					});
			});
			box.appendChild(el('div', { class: 'setting-row' },
				el('code', null, o.pointer), el('span', { class: 'setting-inline-value' }, summarize(o.value)), drop));
		}
		body.appendChild(box);
	}
	return body;
}

/**
 * Mount the editor for one hook into a container. Re-entrant: every write
 * re-renders from the view the server just returned, so what is on screen is
 * always what the runner will hand the next container.
 */
export async function mountSettings(container: HTMLElement, hookID: string): Promise<void> {
	const paint = (view: SettingsView) => {
		container.textContent = '';
		container.appendChild(renderSettings(view, paint));
	};
	container.textContent = '';
	container.appendChild(el('p', { class: 'empty' }, 'loading settings…'));
	try {
		const view = await fetchJSON<SettingsView>(`/hooks/${encodeURIComponent(hookID)}/settings`);
		paint(view);
	} catch (e) {
		container.textContent = '';
		container.appendChild(el('p', { class: 'attention-line' }, `could not load settings: ${String(e)}`));
	}
}
