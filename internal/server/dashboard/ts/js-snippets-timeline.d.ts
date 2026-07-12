// INTERIM type shim for the Pages-served <timeline-view> module — temporary
// until js-snippets publishes .d.ts to Pages next to the .js (already
// queued: ts0 grows a declarations option, and the //go:generate step will
// then FETCH upstream's declarations into committed, freshness-gated files
// so an upstream API change turns CI red instead of drifting). This file is
// the stopgap, not the convention.
//
// The component is NOT vendored: the browser imports it at runtime from
// js-snippets' GitHub Pages (live at master head), and the built bundle
// keeps the URL verbatim (esbuild `external`). TypeScript can't fetch types
// from a URL, so this ambient declaration provides them — TYPES ONLY, no
// implementation. It declares exactly the public surface the adapter
// consumes (plus the element's small public API), derived from
// wow-look-at-my/js-snippets src/ui/timeline-view.ts + timeline-view-math.ts.
// Until the mechanical replacement lands: keep it minimal — never mirror
// upstream internals or helpers the adapter doesn't touch.
declare module 'https://wow-look-at-my.github.io/js-snippets/ui/timeline-view.js' {
	// -- Data shapes ---------------------------------------------------------

	/** A swimlane: one labeled horizontal band of the timeline. */
	export interface TimelineLane {
		/** Unique lane id — intervals reference it via `laneId`. */
		id: string;
		/** Text drawn in the left gutter (ellipsized; full text via tooltip). */
		label: string;
		/** Optional grouping key — the default color category for intervals that set none. */
		group?: string;
	}

	/** A phase within an interval, rendered as a sub-span of the bar. */
	export interface TimelineSegment {
		start: number | Date;
		/** null/undefined = runs to the parent interval's end. */
		end?: number | Date | null;
		/** Style-map key for this phase (e.g. 'queued', 'hatch'). */
		kind: string;
	}

	/** One bar on a lane: [start, end] on the shared time axis. */
	export interface TimelineInterval {
		/** Unique id — connectors reference it, mergeData dedupes on it. */
		id: string;
		laneId: string;
		start: number | Date;
		/** null/undefined = ongoing (renders to the live "now" edge). */
		end?: number | Date | null;
		label?: string;
		/** Color key: same category = same hue. Defaults to lane.group, then laneId. */
		category?: string;
		/** Style-map key: rendering treatment (e.g. 'failed', 'dim', 'hatch'). */
		state?: string;
		segments?: TimelineSegment[];
		/** Opaque consumer payload — echoed back in events and tooltip callbacks. */
		data?: unknown;
	}

	/** A line between two intervals (e.g. a lock waiter → holder). */
	export interface TimelineConnector {
		fromIntervalId: string;
		toIntervalId: string;
		kind?: string;
		label?: string;
	}

	/** A vertical time marker across all lanes. */
	export interface TimelineMarker {
		time: number | Date;
		label?: string;
		/** 'emphasis' renders in the emphasis color; anything else is muted. */
		kind?: string;
	}

	/** The visible time window (ms since epoch). */
	export interface TimeView {
		start: number;
		end: number;
	}

	/** A time range [start, end] used by coverage bookkeeping (ms since epoch). */
	export interface TimeRange {
		start: number;
		end: number;
	}

	/** Fill pattern for an interval state / segment kind. */
	export type StylePattern = 'solid' | 'hatch' | 'stipple' | 'outline';

	/** Rendering treatment for one interval `state` or segment `kind`. */
	export interface IntervalStyle {
		pattern?: StylePattern;
		alphaScale?: number;
		saturationScale?: number;
		lightnessScale?: number;
		border?: { width?: number; dash?: number[]; emphasis?: boolean };
		glyph?: 'none' | 'bang' | 'dot';
	}

	/** Named style map: interval `state` / segment `kind` → treatment. */
	export type StyleMap = Record<string, IntervalStyle>;

	/** The full data payload for setData / mergeData. */
	export interface TimelineData {
		lanes?: TimelineLane[];
		intervals?: TimelineInterval[];
		connectors?: TimelineConnector[];
		markers?: TimelineMarker[];
		/** Time range the supplied intervals fully cover (for async history). */
		coverage?: TimeRange;
	}

	/**
	 * Async history loader: invoked when the viewport reaches uncovered past.
	 * Supply the data via mergeData() before resolving; resolve
	 * `{ exhausted: true }` when nothing exists before this range.
	 */
	export type LoadRangeFn = (start: number, end: number) => Promise<{ exhausted?: boolean } | void>;

	/** What the pointer is over — handed to tooltipFor and hover/click events. */
	export type TimelineHit =
		| { type: 'interval'; interval: TimelineInterval; lane: TimelineLane }
		| { type: 'connector'; connector: TimelineConnector; missingEndpoint?: 'from' | 'to' }
		| { type: 'marker'; marker: TimelineMarker }
		| { type: 'lane'; lane: TimelineLane };

	/** Tooltip content callback: string or Node (never injected as HTML). */
	export type TooltipFn = (hit: TimelineHit) => string | Node | null | undefined;

	/** Color override callback: return a CSS color, or null for the default. */
	export type ColorFn = (interval: TimelineInterval, lane: TimelineLane) => string | null | undefined;

	// -- The element -----------------------------------------------------------

	/**
	 * The timeline custom element. Importing this module registers it as
	 * `<timeline-view>`. Data arrives via properties and methods — never
	 * attributes; the only attributes are scalar toggles: `no-live-pill`,
	 * `history-end-text`, `empty-text`.
	 *
	 * CustomEvents dispatched (listen with addEventListener):
	 *   'intervalclick'  detail: { interval: TimelineInterval; lane: TimelineLane }
	 *   'connectorclick' detail: { connector: TimelineConnector }
	 *   'laneclick'      detail: { lane: TimelineLane }
	 *   'intervalhover'  detail: { interval: TimelineInterval | null; lane: TimelineLane | null }
	 *   'viewportchange' detail: { start: number; end: number; followNow: boolean }
	 */
	export class TimelineViewElement extends HTMLElement {
		/** Replace everything supplied (omitted fields keep current data). */
		setData(data: TimelineData): void;
		/** Additive: upsert by id, union coverage — the polling/backfill path. */
		mergeData(data: TimelineData): void;
		setLanes(lanes: TimelineLane[]): void;
		setIntervals(intervals: TimelineInterval[]): void;
		setConnectors(connectors: TimelineConnector[]): void;
		setMarkers(markers: TimelineMarker[]): void;
		/** Drop all data, coverage, and view state. */
		clear(): void;
		/** Consumer style keys, spread over the built-in DEFAULT_STYLES. */
		get styles(): StyleMap;
		set styles(map: StyleMap | null | undefined);
		/** Async history loader; null disables paging into uncovered past. */
		get loadRange(): LoadRangeFn | null;
		set loadRange(fn: LoadRangeFn | null | undefined);
		get tooltipFor(): TooltipFn | null;
		set tooltipFor(fn: TooltipFn | null | undefined);
		get colorFor(): ColorFn | null;
		set colorFor(fn: ColorFn | null | undefined);
		/** The visible window (a copy). */
		get viewport(): TimeView;
		setViewport(start: number | Date, end: number | Date): void;
		/** Whether the view is docked to the live "now" edge. */
		get followNow(): boolean;
		set followNow(v: boolean);
		/** Snap back to the live edge (the jump pill's action). */
		jumpToNow(): void;
	}
}
