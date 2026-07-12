// Vendored from wow-look-at-my/js-snippets src/ui/timeline-view.ts (PR #34, vendored 2026-07-12). Do not edit here — fix upstream and re-copy.
/**
 * <timeline-view> — a canvas-rendered, realtime swimlane timeline.
 *
 * A shared horizontal time axis across the full width; stacked labeled
 * lanes, each a band of interval bars (left = start, right = end; a lane
 * grows extra sub-tracks when its intervals overlap). Everything is data:
 * lanes {id, label, group?}, intervals {id, laneId, start, end, label?,
 * category?, state?, segments?, data?}, plus optional connectors between
 * intervals, and vertical time markers. Color encodes CATEGORY (stable hue
 * per category string); rendering STYLE encodes state/phase via a named
 * style map (hatching, desaturation, stipple, emphasis borders + glyphs —
 * never hue). Zero/near-zero-width intervals render as instant diamond
 * pips (still colored, styled, hoverable, clickable — never an invisible
 * sliver). Not a Gantt (many bars per row), not a flame chart (no nesting).
 *
 *   import 'https://…/js-snippets/ui/timeline-view.js'; // registers <timeline-view>
 *
 *   const tl = document.querySelector('timeline-view');
 *   tl.setData({
 *     lanes: [{ id: 'ci', label: 'ci pipeline' }],
 *     intervals: [{ id: 'r1', laneId: 'ci', start: Date.now() - 60_000, end: null,
 *                   label: 'build #42', category: 'build' }],
 *   });
 *
 * Follow-now mode (default) pins the right edge to a live "now"; scroll or
 * drag into the past and a jump-to-now pill appears (panning backward
 * disengages follow immediately; panning forward re-docks magnetically at
 * the live edge). Interaction is trackpad-first: two-finger pan (x = time,
 * y = lanes — a horizontal swipe always pans time, a diagonal one applies
 * both axes), ctrl/meta+wheel = smooth zoom anchored under the cursor
 * (discrete wheel steps glide), shift+wheel = time pan, drag = pan, pinch =
 * zoom, arrows/±/Home/End when focused. `loadRange` turns scrolling into
 * the past into async history requests — for BACKWARD gaps only; the live
 * forward edge always belongs to the consumer's own setData/mergeData
 * `coverage` — with uncovered regions visibly distinct from empty-but-known
 * ones and an explicit end-of-history boundary.
 *
 * Rendering is stability-first: the viewport origin is snapped to WHOLE
 * device pixels once per frame (bars keep exact relative offsets while
 * scrolling — no per-element rounding jiggle), bar-vs-pip shapes are
 * decided from data-space durations (never from rounded screen coords, so
 * shapes don't flicker during pans), and lane heights derive from the
 * parallelism visible in the CURRENT window (a historical burst stops
 * padding its lane once off-screen; height changes tween ~150ms, honoring
 * prefers-reduced-motion).
 *
 * Cheap by construction: draws only when dirty (one rAF at a time), a
 * continuous loop runs only while following/animating and the element is
 * visible, and idle animation is paced adaptively — full rate while
 * interacting (plus a short grace window), ~30fps idle, ~10fps idle on
 * battery (feature-detected via navigator.getBattery), paused while the
 * document is hidden; culled to the viewport; DPR-aware (capped at 2).
 * Theme via --timeline-* custom properties (see THEME_DEFAULTS); the DOM
 * chrome (tooltip, live pill, empty hint) is styled by timeline-view.css.
 * The pure math lives in ui/timeline-view-math.ts (node-tested) and is
 * re-exported here so one import serves both.
 */

import {
  toMs,
  timeToX,
  xToTime,
  panView,
  zoomView,
  zoomFactorForWheel,
  routeWheel,
  followAfterGesture,
  FOLLOW_LEAD_FRAC,
  snapViewToDevicePixels,
  MIN_SPAN_MS,
  MAX_SPAN_MS,
  timeTicks,
  timeTickStep,
  formatTimeTick,
  formatTimeFull,
  formatDuration,
  packVisibleTracks,
  layoutLanes,
  trackTop,
  fitText,
  isInstantWidth,
  durationWidthPx,
  MIN_BAR_PX,
  expandHitRect,
  hitTestPolyline,
  connectorRoute,
  categoryHue,
  categoryJitter,
  categoryColor,
  DEFAULT_STYLES,
  CoverageTracker,
  historyProbe,
  frameBudgetMs,
  shouldRender,
  INTERACT_GRACE_MS,
  type RenderTier,
  type TimelineLane,
  type TimelineInterval,
  type TimelineConnector,
  type TimelineMarker,
  type TimeView,
  type TimeRange,
  type IntervalStyle,
  type StyleMap,
  type LaneLayout,
  type HitRect,
} from './timeline-view-math.ts';

import TIMELINE_CSS from './timeline-view.css';

export * from './timeline-view-math.ts';

// -- Theme -----------------------------------------------------------------------

/**
 * Theme defaults — the dark "Scratch Proto" palette. Override per element /
 * ancestor / :root with the CSS custom properties named here.
 */
export const THEME_DEFAULTS = {
  /** --timeline-bg — plot background. */
  bg: '#0d0f14',
  /** --timeline-fg — bar labels and primary text. */
  fg: '#e8ecf4',
  /** --timeline-muted — axis ticks, lane labels, secondary text. */
  muted: '#6b7280',
  /** --timeline-grid — vertical time gridlines. */
  grid: 'rgba(200, 205, 216, 0.07)',
  /** --timeline-hairline — horizontal lane separators. */
  hairline: 'rgba(200, 205, 216, 0.10)',
  /** --timeline-row-tint — alternating lane tint ('none' disables). */
  rowTint: 'rgba(255, 255, 255, 0.015)',
  /** --timeline-now — the live now line + jump pill accent. */
  now: '#00e47a',
  /** --timeline-emphasis — emphasis/failed borders and glyphs. */
  emphasis: '#ff5c5c',
  /** --timeline-font — font family for all canvas text. */
  font: "'JetBrains Mono', 'SF Mono', 'Cascadia Code', 'Fira Code', monospace",
  /** --timeline-font-size — base font size in px. */
  fontSize: 11,
  /** --timeline-cat-lightness — oklch lightness for category fills (0..1). */
  catLightness: 0.62,
  /** --timeline-cat-chroma — oklch chroma for category fills. */
  catChroma: 0.11,
  /** --timeline-track-height — sub-track bar height in px (clamped 10..40). */
  trackHeight: 18,
  /** --timeline-gutter-width — lane-label gutter in px (0 = auto-size). */
  gutterWidth: 0,
};

type Theme = typeof THEME_DEFAULTS;

// -- Public data / callback types ----------------------------------------------------

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

// -- Internal shapes ---------------------------------------------------------------

interface NSeg {
  start: number;
  end: number | null;
  kind: string;
}

interface NInterval {
  src: TimelineInterval;
  id: string;
  laneIdx: number;
  start: number;
  end: number | null; // null = ongoing
  label: string;
  catKey: string;
  state: string;
  segs: NSeg[] | null;
  track: number;
}

interface ResolvedStyle {
  fill: string;
  border: string;
  borderWidth: number;
  dash: number[] | null;
  pattern: 'solid' | 'hatch' | 'stipple' | 'outline';
  glyph: 'none' | 'bang' | 'dot';
  labelColor: string;
}

const DEFAULT_SPAN_MS = 15 * 60_000;
const LAYOUT_TWEEN_MS = 150; // lane-height ease on visible-track-count change
const AXIS_H = 22;
const HIT_MIN_W = 9; // widened hit target for instants (px)
const CONNECTOR_TOL = 4;
const CLICK_SLOP = 4;
const EMPTY_DASH: number[] = [];
const MARKER_DASH = [4, 3];

// -- The custom element ----------------------------------------------------------------

/**
 * The timeline element. Auto-registered as `<timeline-view>` when this
 * module loads (unless the name is taken). Data arrives via properties and
 * methods — setData / mergeData / setLanes / setIntervals / setConnectors /
 * setMarkers — never attributes; the only attributes are scalar toggles:
 * `no-live-pill` (hide the jump-to-now pill), `history-end-text` (boundary
 * label), `empty-text` (empty-state hint).
 */
export class TimelineViewElement extends HTMLElement {
  static get observedAttributes(): string[] {
    return ['no-live-pill', 'history-end-text', 'empty-text'];
  }

  private canvas: HTMLCanvasElement;
  private ctx: CanvasRenderingContext2D | null = null;
  private tooltipEl: HTMLDivElement;
  private pillEl: HTMLButtonElement;
  private emptyEl: HTMLDivElement;

  // -- Data (normalized) --
  private lanes: TimelineLane[] = [];
  private laneIdxById = new Map<string, number>();
  private perLane: NInterval[][] = []; // sorted by (start, id)
  private byId = new Map<string, NInterval>();
  private connectors: TimelineConnector[] = [];
  private markers: { time: number; label: string; kind: string }[] = [];
  private layout: LaneLayout = { tops: [], heights: [], totalHeight: 0 };
  private styleMap: StyleMap = { ...DEFAULT_STYLES };

  // -- Viewport --
  private view: TimeView = { start: 0, end: 1 }; // set on connect/first data
  private following = true;
  private laneScroll = 0;

  // -- Async history --
  private coverage = new CoverageTracker();
  private loadRangeFn: LoadRangeFn | null = null;
  private loadTick = 0; // guards stale settles across data resets

  // -- Callbacks --
  private tooltipForFn: TooltipFn | null = null;
  private colorForFn: ColorFn | null = null;
  private nowFn: (() => number) | null = null;

  // -- Interaction state --
  private pointers = new Map<number, { x: number; y: number }>();
  private lastMouse: { x: number; y: number; cx: number; cy: number } | null = null;
  private dragTotal = 0;
  private downHit: TimelineHit | null = null;
  private hover: TimelineHit | null = null;
  private hoverIntervalId: string | null = null;
  private glidePx = 0; // pending discrete-wheel zoom, in wheel px
  private glideX = 0; // zoom anchor (canvas x) for the glide
  private lastFrame = 0;
  private lastInputTs = -Infinity; // last wheel/drag/key input (perf-clock)
  private lastRenderTs = -Infinity; // last RENDERED frame (adaptive pacing)
  private batteryDischarging = false;
  private batteryOff: (() => void) | null = null;

  // -- Visible-window lane layout --
  private packEpoch = 0; // bumped on data changes; forces a re-pack
  private packedEpoch = -1;
  private packedStart = NaN;
  private packedEnd = NaN;
  private targetCounts: number[] = []; // visible track count per lane
  private displayCounts: number[] = []; // animated (float) counts driving layout
  private layoutAnim: { from: number[]; start: number } | null = null;
  private rvCache = { start: NaN, end: NaN, w: NaN, dpr: NaN, out: { start: 0, end: 1 } as TimeView };

  // -- Rendering state --
  private cssW = 0;
  private cssH = 0;
  private dpr = 1;
  private theme: Theme = { ...THEME_DEFAULTS };
  private fontAxis = '';
  private fontBar = '';
  private charW = 6;
  private gutterW = 90;
  private oklch = true;
  private reducedMotion = false;
  private colorCache = new Map<string, ResolvedStyle>();
  private patternCache = new Map<string, CanvasPattern>();

  private raf = 0;
  private dirty = false;
  private connected = false;
  private inView = true;
  private ro: ResizeObserver | null = null;
  private io: IntersectionObserver | null = null;
  private motionMq: MediaQueryList | null = null;

  constructor() {
    super();
    const shadow = this.attachShadow({ mode: 'open' });
    try {
      const sheet = new CSSStyleSheet();
      sheet.replaceSync(TIMELINE_CSS);
      shadow.adoptedStyleSheets = [sheet];
    } catch {
      const style = document.createElement('style');
      style.textContent = TIMELINE_CSS;
      shadow.append(style);
    }
    this.canvas = document.createElement('canvas');
    this.tooltipEl = document.createElement('div');
    this.tooltipEl.className = 'tooltip';
    this.pillEl = document.createElement('button');
    this.pillEl.className = 'live-pill';
    this.pillEl.type = 'button';
    this.pillEl.textContent = '▸ now';
    this.pillEl.hidden = true;
    this.pillEl.addEventListener('click', () => this.jumpToNow());
    this.emptyEl = document.createElement('div');
    this.emptyEl.className = 'empty-hint';
    this.emptyEl.hidden = true;
    shadow.append(this.canvas, this.tooltipEl, this.pillEl, this.emptyEl);

    const now = this.nowMs();
    this.view = { start: now - DEFAULT_SPAN_MS, end: now };
  }

  // -- Lifecycle -------------------------------------------------------------

  connectedCallback(): void {
    this.connected = true;
    if (!this.hasAttribute('tabindex')) this.tabIndex = 0;
    this.readTheme();

    if (typeof ResizeObserver !== 'undefined') {
      this.ro = new ResizeObserver(() => this.resizeBackingStore());
      this.ro.observe(this);
    }
    if (typeof IntersectionObserver !== 'undefined') {
      this.io = new IntersectionObserver((entries) => {
        this.inView = entries[entries.length - 1].isIntersecting;
        if (this.inView) this.invalidate();
      });
      this.io.observe(this);
    }
    if (typeof matchMedia !== 'undefined') {
      this.motionMq = matchMedia('(prefers-reduced-motion: reduce)');
      this.reducedMotion = this.motionMq.matches;
      this.motionMq.addEventListener?.('change', this.onMotionPref);
    }
    document.addEventListener('visibilitychange', this.onVisibility);
    this.watchBattery();

    this.canvas.addEventListener('wheel', this.onWheel, { passive: false });
    this.canvas.addEventListener('pointerdown', this.onPointerDown);
    this.canvas.addEventListener('pointermove', this.onPointerMove);
    this.canvas.addEventListener('pointerup', this.onPointerUp);
    this.canvas.addEventListener('pointercancel', this.onPointerUp);
    this.canvas.addEventListener('pointerleave', this.onPointerLeave);
    this.addEventListener('keydown', this.onKeyDown);

    this.resizeBackingStore();
    this.syncChrome();
    this.invalidate();
  }

  disconnectedCallback(): void {
    this.connected = false;
    this.ro?.disconnect();
    this.ro = null;
    this.io?.disconnect();
    this.io = null;
    this.motionMq?.removeEventListener?.('change', this.onMotionPref);
    this.motionMq = null;
    document.removeEventListener('visibilitychange', this.onVisibility);
    this.batteryOff?.();
    this.batteryOff = null;
    this.canvas.removeEventListener('wheel', this.onWheel);
    this.canvas.removeEventListener('pointerdown', this.onPointerDown);
    this.canvas.removeEventListener('pointermove', this.onPointerMove);
    this.canvas.removeEventListener('pointerup', this.onPointerUp);
    this.canvas.removeEventListener('pointercancel', this.onPointerUp);
    this.canvas.removeEventListener('pointerleave', this.onPointerLeave);
    this.removeEventListener('keydown', this.onKeyDown);
    if (this.raf !== 0) {
      cancelAnimationFrame(this.raf);
      this.raf = 0;
    }
  }

  attributeChangedCallback(): void {
    this.syncChrome();
    this.invalidate();
  }

  // -- Public API: data --------------------------------------------------------

  /** Replace all data (lanes, intervals, connectors, markers, coverage). */
  setData(data: TimelineData): void {
    this.lanes = [];
    this.laneIdxById.clear();
    this.byId.clear();
    this.perLane = [];
    this.connectors = [];
    this.markers = [];
    this.coverage = new CoverageTracker();
    this.loadTick++;
    this.mergeData(data);
  }

  /**
   * Merge data into the current set: lanes/intervals/connectors dedupe by
   * id (intervals replace in place), markers append-and-dedupe by
   * (time, label, kind). The way loadRange results are supplied.
   */
  mergeData(data: TimelineData): void {
    if (data.lanes) {
      for (const lane of data.lanes) {
        const at = this.laneIdxById.get(lane.id);
        if (at !== undefined) {
          this.lanes[at] = lane;
        } else {
          this.laneIdxById.set(lane.id, this.lanes.length);
          this.lanes.push(lane);
          this.perLane.push([]);
        }
      }
    }
    if (data.intervals) {
      for (const iv of data.intervals) this.ingestInterval(iv);
    }
    if (data.connectors) {
      const key = (c: TimelineConnector): string => `${c.fromIntervalId} ${c.toIntervalId} ${c.kind ?? ''}`;
      const seen = new Map(this.connectors.map((c) => [key(c), c]));
      for (const c of data.connectors) seen.set(key(c), c);
      this.connectors = [...seen.values()];
    }
    if (data.markers) {
      const key = (m: { time: number; label: string; kind: string }): string => `${m.time} ${m.label} ${m.kind}`;
      const seen = new Set(this.markers.map(key));
      for (const m of data.markers) {
        const n = { time: toMs(m.time), label: m.label ?? '', kind: m.kind ?? '' };
        if (!seen.has(key(n))) {
          seen.add(key(n));
          this.markers.push(n);
        }
      }
      this.markers.sort((a, b) => a.time - b.time);
    }
    if (data.coverage) this.coverage.addCovered(toMs(data.coverage.start), toMs(data.coverage.end));
    this.rebuild();
  }

  /** Individual setters (each replaces just that slice of the data). */
  setLanes(lanes: TimelineLane[]): void {
    // Replace lane set but keep intervals whose lane survives.
    const kept: TimelineInterval[] = [];
    for (const per of this.perLane) for (const n of per) kept.push(n.src);
    const connectors = this.connectors;
    const markers = this.markers;
    this.lanes = [];
    this.laneIdxById.clear();
    this.byId.clear();
    this.perLane = [];
    this.connectors = [];
    this.markers = markers;
    this.mergeData({ lanes, intervals: kept, connectors });
  }

  setIntervals(intervals: TimelineInterval[]): void {
    this.byId.clear();
    this.perLane = this.lanes.map(() => []);
    for (const iv of intervals) this.ingestInterval(iv);
    this.rebuild();
  }

  setConnectors(connectors: TimelineConnector[]): void {
    this.connectors = [...connectors];
    this.invalidate();
  }

  setMarkers(markers: TimelineMarker[]): void {
    this.markers = markers.map((m) => ({ time: toMs(m.time), label: m.label ?? '', kind: m.kind ?? '' }));
    this.markers.sort((a, b) => a.time - b.time);
    this.invalidate();
  }

  /** Drop everything (data, coverage, viewport stays). */
  clear(): void {
    this.setData({});
  }

  /** Style map for interval `state` / segment `kind` (spread over the built-ins). */
  get styles(): StyleMap {
    return this.styleMap;
  }
  set styles(map: StyleMap | null | undefined) {
    this.styleMap = { ...DEFAULT_STYLES, ...(map ?? {}) };
    this.colorCache.clear();
    this.invalidate();
  }

  /** Async history loader (see LoadRangeFn); null disables. */
  get loadRange(): LoadRangeFn | null {
    return this.loadRangeFn;
  }
  set loadRange(fn: LoadRangeFn | null | undefined) {
    this.loadRangeFn = fn ?? null;
    this.invalidate();
  }

  /** Tooltip content override; null restores the built-in tooltip. */
  get tooltipFor(): TooltipFn | null {
    return this.tooltipForFn;
  }
  set tooltipFor(fn: TooltipFn | null | undefined) {
    this.tooltipForFn = fn ?? null;
  }

  /** Per-interval color override; null restores category colors. */
  get colorFor(): ColorFn | null {
    return this.colorForFn;
  }
  set colorFor(fn: ColorFn | null | undefined) {
    this.colorForFn = fn ?? null;
    this.colorCache.clear();
    this.invalidate();
  }

  /** Clock override (ms since epoch) for replay/testing; null = Date.now. */
  get nowProvider(): (() => number) | null {
    return this.nowFn;
  }
  set nowProvider(fn: (() => number) | null | undefined) {
    this.nowFn = fn ?? null;
    this.invalidate();
  }

  // -- Public API: viewport ------------------------------------------------------

  /** The visible time window (ms since epoch). */
  get viewport(): TimeView {
    return { start: this.view.start, end: this.view.end };
  }

  /** Jump/zoom to an explicit window (disengages follow unless it ends at now). */
  setViewport(start: number | Date, end: number | Date): void {
    const s = toMs(start);
    const e = toMs(end);
    if (!Number.isFinite(s) || !Number.isFinite(e) || !(e > s)) return;
    const span = Math.min(Math.max(e - s, MIN_SPAN_MS), MAX_SPAN_MS);
    this.applyUserView({ start: s, end: s + span });
  }

  /** Whether the right edge is pinned to live "now" (default true). */
  get followNow(): boolean {
    return this.following;
  }
  set followNow(v: boolean) {
    if (v === this.following) return;
    this.following = v;
    if (v) this.pinToNow();
    this.syncChrome();
    this.emitViewport();
    this.invalidate();
  }

  /** Re-engage follow mode, keeping the current span. */
  jumpToNow(): void {
    this.following = true;
    this.pinToNow();
    this.syncChrome();
    this.emitViewport();
    this.invalidate();
  }

  /** Re-read the --timeline-* custom properties (call after retheming). */
  refreshTheme(): void {
    this.readTheme();
    this.invalidate();
  }

  // -- Data normalization ---------------------------------------------------------

  private ingestInterval(iv: TimelineInterval): void {
    let laneIdx = this.laneIdxById.get(iv.laneId);
    if (laneIdx === undefined) {
      // Unknown lane: synthesize one so data never silently disappears.
      laneIdx = this.lanes.length;
      this.laneIdxById.set(iv.laneId, laneIdx);
      this.lanes.push({ id: iv.laneId, label: iv.laneId });
      this.perLane.push([]);
    }
    const lane = this.lanes[laneIdx];
    const start = toMs(iv.start);
    const end = iv.end == null ? null : toMs(iv.end);
    const n: NInterval = {
      src: iv,
      id: iv.id,
      laneIdx,
      start,
      end,
      label: iv.label ?? '',
      catKey: iv.category ?? lane.group ?? lane.id,
      state: iv.state ?? '',
      segs: iv.segments?.length
        ? iv.segments.map((s) => ({ start: toMs(s.start), end: s.end == null ? null : toMs(s.end), kind: s.kind }))
        : null,
      track: 0,
    };
    const prev = this.byId.get(iv.id);
    if (prev) {
      const arr = this.perLane[prev.laneIdx];
      arr.splice(arr.indexOf(prev), 1);
    }
    this.byId.set(iv.id, n);
    this.perLane[laneIdx].push(n);
  }

  /**
   * Re-sort and re-layout after any data change. Track assignment and lane
   * heights come from the VISIBLE window (updateVisibleLayout), so a
   * historical parallelism burst stops padding its lane once off-screen.
   */
  private rebuild(): void {
    for (const per of this.perLane) {
      per.sort((a, b) => a.start - b.start || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    }
    this.packEpoch++;
    this.updateVisibleLayout();
    this.autoGutter();
    this.clampLaneScroll();
    this.syncChrome();
    this.invalidate();
  }

  /**
   * Track assignment + lane heights from the intervals intersecting the
   * CURRENT viewport (partial overlap counts; a lane with nothing visible
   * collapses to one track). Deterministic given the visible data — a
   * merely-translating viewport over unchanged overlap recomputes to the
   * identical result, so nothing jitters frame to frame. Count CHANGES
   * ease over LAYOUT_TWEEN_MS (snapped under prefers-reduced-motion).
   * this.layout always reflects the CURRENT (possibly animating) heights,
   * and hit-testing shares it, so hovers stay aligned mid-tween.
   */
  private updateVisibleLayout(): void {
    const rv = this.renderView();
    const structure = this.targetCounts.length !== this.perLane.length;
    if (this.packedEpoch !== this.packEpoch || this.packedStart !== rv.start || this.packedEnd !== rv.end || structure) {
      this.packedEpoch = this.packEpoch;
      this.packedStart = rv.start;
      this.packedEnd = rv.end;
      const prev = this.targetCounts;
      const next = new Array<number>(this.perLane.length);
      let changed = structure;
      for (let i = 0; i < this.perLane.length; i++) {
        const per = this.perLane[i];
        const { tracks, trackCount } = packVisibleTracks(per, rv);
        for (let j = 0; j < per.length; j++) {
          if (tracks[j] >= 0) per[j].track = tracks[j];
        }
        next[i] = trackCount;
        if (!changed && prev[i] !== trackCount) changed = true;
      }
      this.targetCounts = next;
      if (changed) {
        if (this.reducedMotion || structure || this.displayCounts.length !== next.length) {
          this.displayCounts = next.slice();
          this.layoutAnim = null;
        } else {
          this.layoutAnim = { from: this.displayCounts.slice(), start: this.perfNow() };
        }
      }
    }
    if (this.layoutAnim) {
      const a = this.layoutAnim;
      const p = Math.min(1, (this.perfNow() - a.start) / LAYOUT_TWEEN_MS);
      const ease = p * (2 - p); // easeOutQuad
      const disp = new Array<number>(this.targetCounts.length);
      for (let i = 0; i < disp.length; i++) {
        const from = a.from[i] ?? this.targetCounts[i];
        disp[i] = from + (this.targetCounts[i] - from) * ease;
      }
      this.displayCounts = disp;
      if (p >= 1) this.layoutAnim = null;
    } else if (this.displayCounts.length !== this.targetCounts.length) {
      this.displayCounts = this.targetCounts.slice();
    }
    this.layout = layoutLanes(this.displayCounts, this.metrics());
  }

  private metrics(): { trackHeight: number; trackGap: number; lanePad: number } {
    return { trackHeight: this.theme.trackHeight, trackGap: 2, lanePad: 3 };
  }

  private autoGutter(): void {
    if (this.theme.gutterWidth > 0) {
      this.gutterW = this.theme.gutterWidth;
      return;
    }
    let maxChars = 0;
    for (const lane of this.lanes) maxChars = Math.max(maxChars, lane.label.length);
    this.gutterW = Math.round(Math.min(220, Math.max(70, maxChars * this.charW + 16)));
  }

  private nowMs(): number {
    return this.nowFn ? this.nowFn() : Date.now();
  }

  private perfNow(): number {
    return typeof performance !== 'undefined' ? performance.now() : Date.now();
  }

  private tzOffsetMs(): number {
    return -new Date().getTimezoneOffset() * 60_000;
  }

  /** Battery awareness for the idle render tier (feature-detected; absent API = AC tier). */
  private watchBattery(): void {
    type BatteryLike = {
      charging: boolean;
      addEventListener?: (type: string, fn: () => void) => void;
      removeEventListener?: (type: string, fn: () => void) => void;
    };
    const nav = typeof navigator !== 'undefined' ? (navigator as Navigator & { getBattery?: () => Promise<BatteryLike> }) : null;
    if (!nav || typeof nav.getBattery !== 'function') return;
    nav
      .getBattery()
      .then((b) => {
        if (!this.connected || this.batteryOff) return;
        const update = (): void => {
          this.batteryDischarging = !b.charging;
        };
        update();
        b.addEventListener?.('chargingchange', update);
        this.batteryOff = () => {
          b.removeEventListener?.('chargingchange', update);
          this.batteryDischarging = false;
        };
      })
      .catch(() => {
        /* API present but denied: stay on the AC tier */
      });
  }

  private noteInput(): void {
    this.lastInputTs = this.perfNow();
  }

  /** Current pacing tier: any live gesture/tween = full rate; else idle (AC/battery). */
  private renderTier(): RenderTier {
    if (this.pointers.size > 0 || this.glidePx !== 0 || this.layoutAnim !== null) return 'interactive';
    if (this.perfNow() - this.lastInputTs < INTERACT_GRACE_MS) return 'interactive';
    return this.batteryDischarging ? 'idle-battery' : 'idle';
  }

  // -- Viewport internals -----------------------------------------------------------

  private pinToNow(): void {
    const span = this.view.end - this.view.start;
    const end = this.nowMs() + span * FOLLOW_LEAD_FRAC;
    this.view = { start: end - span, end };
  }

  /**
   * Apply a user-driven viewport. Backward PANS disengage follow outright;
   * everything else keeps the magnetic re-engage rule (followAfterGesture —
   * without the pan carve-out, small trackpad pan steps were re-pinned to
   * "now" one by one and horizontal panning never escaped follow mode).
   */
  private applyUserView(next: TimeView, opts?: { pan?: boolean }): void {
    const span = next.end - next.start;
    const now = this.nowMs();
    const wasFollowing = this.following;
    this.following = followAfterGesture(this.view.end, next, now, opts?.pan === true);
    if (this.following) {
      const end = now + span * FOLLOW_LEAD_FRAC;
      this.view = { start: end - span, end };
    } else {
      this.view = next;
    }
    if (wasFollowing !== this.following) this.syncChrome();
    // The content moved under a resting cursor — keep hover/tooltip honest.
    if (this.lastMouse && this.pointers.size === 0) {
      const p = this.lastMouse;
      this.setHover(this.hitAt(p.x, p.y), p.cx, p.cy);
    }
    this.emitViewport();
    this.invalidate();
  }

  private emitViewport(): void {
    this.dispatchEvent(
      new CustomEvent('viewportchange', {
        detail: { start: this.view.start, end: this.view.end, followNow: this.following },
      }),
    );
  }

  private plotWidth(): number {
    return Math.max(1, this.cssW - this.gutterW);
  }

  private plotHeight(): number {
    return Math.max(1, this.cssH - AXIS_H);
  }

  private maxLaneScroll(): number {
    return Math.max(0, this.layout.totalHeight - this.plotHeight());
  }

  private clampLaneScroll(): void {
    this.laneScroll = Math.max(0, Math.min(this.laneScroll, this.maxLaneScroll()));
  }

  private msPerPx(): number {
    return (this.view.end - this.view.start) / this.plotWidth();
  }

  /**
   * The view all GEOMETRY goes through: origin snapped to whole device
   * pixels (memoized). One global rounding, zero per-element rounding —
   * the scene translates in integer device-pixel steps and bars never
   * jiggle relative to each other (see snapViewToDevicePixels).
   */
  private renderView(): TimeView {
    const c = this.rvCache;
    const w = this.plotWidth();
    if (c.start !== this.view.start || c.end !== this.view.end || c.w !== w || c.dpr !== this.dpr) {
      c.start = this.view.start;
      c.end = this.view.end;
      c.w = w;
      c.dpr = this.dpr;
      c.out = snapViewToDevicePixels(this.view, w, this.dpr);
    }
    return c.out;
  }

  // -- Chrome (DOM) sync ---------------------------------------------------------

  private syncChrome(): void {
    this.pillEl.hidden = this.following || this.hasAttribute('no-live-pill');
    const empty = this.lanes.length === 0 && this.byId.size === 0;
    this.emptyEl.hidden = !empty;
    if (empty) {
      this.emptyEl.textContent = this.getAttribute('empty-text') ?? 'nothing here yet — waiting for data';
    }
  }

  // -- Scheduling ---------------------------------------------------------------

  private invalidate(): void {
    this.dirty = true;
    this.schedule();
  }

  /** True while something time-based needs continuous frames. */
  private animating(): boolean {
    if (this.following || this.glidePx !== 0 || this.layoutAnim !== null) return true;
    if (this.coverage.pending() && this.loadRangeFn) return true;
    if (this.reducedMotion) return false;
    // Ongoing intervals pulse only while their live edge is in view.
    const now = this.nowMs();
    if (now < this.view.start || this.byId.size === 0) return false;
    for (const per of this.perLane) {
      for (let i = per.length - 1; i >= 0; i--) {
        const n = per[i];
        if (n.end === null && n.start <= this.view.end) return true;
        if (n.end !== null && n.end < this.view.start) break; // sorted by start; cheap bail
      }
    }
    return false;
  }

  private schedule(): void {
    if (!this.connected || this.raf !== 0) return;
    if (!this.inView || document.hidden) return;
    this.raf = requestAnimationFrame(this.onFrame);
  }

  private onFrame = (t: number): void => {
    this.raf = 0;
    // Adaptive pacing: a pure animation frame (nothing dirty) renders only
    // when the current tier's budget has elapsed — full rate while
    // interacting, ~30fps idle, ~10fps idle on battery. Dirty frames
    // (fresh data, hover changes) always render immediately.
    if (!this.dirty && !shouldRender(t, this.lastRenderTs, frameBudgetMs(this.renderTier()))) {
      if (this.animating()) this.schedule();
      else this.lastFrame = 0;
      return;
    }
    const dt = this.lastFrame > 0 ? Math.min(100, t - this.lastFrame) : 16;
    this.lastFrame = t;
    this.lastRenderTs = t;
    this.stepGlide(dt);
    if (this.following) this.pinToNow();
    this.pumpLoad();
    this.updateVisibleLayout();
    if (this.dirty || this.animating()) {
      this.dirty = false;
      this.draw();
    }
    if (this.animating()) this.schedule();
    else this.lastFrame = 0;
  };

  private onVisibility = (): void => {
    if (!document.hidden) this.invalidate();
  };

  private onMotionPref = (e: MediaQueryListEvent): void => {
    this.reducedMotion = e.matches;
    this.invalidate();
  };

  // -- Async history -----------------------------------------------------------

  private pumpLoad(): void {
    const fn = this.loadRangeFn;
    if (!fn) return;
    const now = this.nowMs();
    // loadRange fills BACKWARD gaps only: the probe is clamped to the
    // covered end (historyProbe), so the sliver between the last covered
    // time and the ever-advancing "now" is NEVER requested — that region
    // belongs to the consumer's live merges. Without the clamp, follow
    // mode reopened a fresh forward gap every frame and refired loadRange
    // serially at ~one request per round-trip, forever (~30 req/s).
    const probe = historyProbe(this.view, now, this.coverage.coveredEnd());
    if (!probe) return;
    const req = this.coverage.nextRequest(probe, now);
    if (!req) return;
    const tick = this.loadTick;
    const tracker = this.coverage;
    fn(req.start, req.end).then(
      (res) => {
        if (tick === this.loadTick) tracker.settle(req, { ok: true, exhausted: res?.exhausted });
        this.invalidate();
      },
      () => {
        if (tick === this.loadTick) tracker.settle(req, { ok: false }, this.nowMs());
        this.invalidate();
      },
    );
    this.invalidate();
  }

  // -- Sizing / theme ------------------------------------------------------------

  private resizeBackingStore(): void {
    const raw = typeof devicePixelRatio === 'number' && devicePixelRatio > 0 ? devicePixelRatio : 1;
    const dpr = Math.min(2, raw);
    const bw = Math.max(1, Math.round(this.clientWidth * dpr));
    const bh = Math.max(1, Math.round(this.clientHeight * dpr));
    if (bw === this.canvas.width && bh === this.canvas.height && dpr === this.dpr) return;
    this.canvas.width = bw;
    this.canvas.height = bh;
    this.dpr = dpr;
    this.cssW = bw / dpr;
    this.cssH = bh / dpr;
    this.readTheme();
    this.clampLaneScroll();
    this.invalidate();
  }

  private readTheme(): void {
    const cs = getComputedStyle(this);
    const t = this.theme;
    t.bg = readProp(cs, '--timeline-bg', THEME_DEFAULTS.bg);
    t.fg = readProp(cs, '--timeline-fg', THEME_DEFAULTS.fg);
    t.muted = readProp(cs, '--timeline-muted', THEME_DEFAULTS.muted);
    t.grid = readProp(cs, '--timeline-grid', THEME_DEFAULTS.grid);
    t.hairline = readProp(cs, '--timeline-hairline', THEME_DEFAULTS.hairline);
    t.rowTint = readProp(cs, '--timeline-row-tint', THEME_DEFAULTS.rowTint);
    t.now = readProp(cs, '--timeline-now', THEME_DEFAULTS.now);
    t.emphasis = readProp(cs, '--timeline-emphasis', THEME_DEFAULTS.emphasis);
    t.font = readProp(cs, '--timeline-font', THEME_DEFAULTS.font);
    t.fontSize = readNum(cs, '--timeline-font-size', THEME_DEFAULTS.fontSize, 6, 24);
    t.catLightness = readNum(cs, '--timeline-cat-lightness', THEME_DEFAULTS.catLightness, 0.2, 0.95);
    t.catChroma = readNum(cs, '--timeline-cat-chroma', THEME_DEFAULTS.catChroma, 0, 0.3);
    t.trackHeight = readNum(cs, '--timeline-track-height', THEME_DEFAULTS.trackHeight, 10, 40);
    t.gutterWidth = readNum(cs, '--timeline-gutter-width', THEME_DEFAULTS.gutterWidth, 0, 400);
    this.fontAxis = `${t.fontSize - 1}px ${t.font}`;
    this.fontBar = `${t.fontSize}px ${t.font}`;
    this.oklch = typeof CSS !== 'undefined' && !!CSS.supports && CSS.supports('color', 'oklch(0.6 0.1 120)');
    const ctx = (this.ctx ??= this.canvas.getContext('2d'));
    if (ctx) {
      ctx.font = this.fontBar;
      const probe = 'abcdefghijklmnop0123456789';
      this.charW = ctx.measureText(probe).width / probe.length || 6;
    }
    this.colorCache.clear();
    this.patternCache.clear();
    this.layout = layoutLanes(this.displayCounts, this.metrics());
    this.autoGutter();
  }

  // -- Styles / colors -------------------------------------------------------------

  private styleFor(state: string): IntervalStyle {
    return this.styleMap[state] ?? this.styleMap[''] ?? {};
  }

  private resolved(catKey: string, state: string, override: string | null): ResolvedStyle {
    const cacheKey = `${catKey} ${state} ${override ?? ''}`;
    const hit = this.colorCache.get(cacheKey);
    if (hit) return hit;
    const st = this.styleFor(state);
    const t = this.theme;
    let fill: string;
    let border: string;
    if (override) {
      fill = override;
      border = override;
    } else {
      const hue = categoryHue(catKey);
      const j = categoryJitter(catKey);
      const l = clamp(t.catLightness * (st.lightnessScale ?? 1) + j.dl, 0.2, 0.92);
      const c = clamp(t.catChroma * (st.saturationScale ?? 1) + j.dc, 0, 0.3);
      const mode = this.oklch ? 'oklch' : 'hsl';
      const alpha = clamp(st.alphaScale ?? 1, 0, 1);
      fill = categoryColor(hue, { mode, lightness: l, chroma: c, alpha });
      border = categoryColor(hue, { mode, lightness: clamp(l + 0.14, 0, 0.96), chroma: c, alpha: clamp(alpha + 0.1, 0, 1) });
    }
    const emphasisBorder = st.border?.emphasis === true;
    const out: ResolvedStyle = {
      fill,
      border: emphasisBorder ? t.emphasis : border,
      borderWidth: st.border?.width ?? 1,
      dash: st.border?.dash ?? null,
      pattern: st.pattern ?? 'solid',
      glyph: st.glyph ?? 'none',
      labelColor: (st.alphaScale ?? 1) < 0.7 ? t.muted : t.fg,
    };
    this.colorCache.set(cacheKey, out);
    return out;
  }

  private patternFor(kind: 'hatch' | 'stipple', color: string): CanvasPattern | null {
    const key = `${kind} ${color}`;
    const hit = this.patternCache.get(key);
    if (hit) return hit;
    const size = 7;
    const dpr = this.dpr;
    const tile = document.createElement('canvas');
    tile.width = size * dpr;
    tile.height = size * dpr;
    const c = tile.getContext('2d');
    if (!c) return null;
    c.scale(dpr, dpr);
    c.strokeStyle = color;
    c.fillStyle = color;
    if (kind === 'hatch') {
      c.lineWidth = 1.6;
      c.beginPath();
      // 45° stripes, drawn twice so the tile wraps seamlessly.
      c.moveTo(-size / 2, size * 1.5);
      c.lineTo(size * 1.5, -size / 2);
      c.moveTo(-size / 2, size / 2);
      c.lineTo(size / 2, -size / 2);
      c.moveTo(size / 2, size * 1.5);
      c.lineTo(size * 1.5, size / 2);
      c.stroke();
    } else {
      c.beginPath();
      c.arc(size * 0.28, size * 0.28, 0.9, 0, Math.PI * 2);
      c.arc(size * 0.78, size * 0.78, 0.9, 0, Math.PI * 2);
      c.fill();
    }
    const ctx = this.ctx;
    if (!ctx) return null;
    const pattern = ctx.createPattern(tile, 'repeat');
    if (!pattern) return null;
    pattern.setTransform?.(new DOMMatrix().scale(1 / dpr));
    this.patternCache.set(key, pattern);
    return pattern;
  }

  // -- Geometry ----------------------------------------------------------------

  /**
   * CSS-px rect of an interval (valid even outside the viewport). Mapped
   * through the device-pixel-snapped render view and deliberately NOT
   * rounded per element — one global rounding policy (renderView), so bars
   * hold exact relative offsets while the viewport translates.
   */
  private rectFor(n: NInterval, now: number): HitRect {
    const w = this.plotWidth();
    const m = this.metrics();
    const rv = this.renderView();
    const xs = this.gutterW + timeToX(n.start, rv, w);
    const xe = this.gutterW + timeToX(n.end ?? now, rv, w);
    const y = AXIS_H + this.layout.tops[n.laneIdx] - this.laneScroll + trackTop(n.track, m);
    return { x: xs, y, w: Math.max(0, xe - xs), h: m.trackHeight };
  }

  // -- Hit testing ---------------------------------------------------------------

  private hitAt(x: number, y: number): TimelineHit | null {
    if (this.lanes.length === 0) return null;
    const now = this.nowMs();
    // Lane gutter → the lane label.
    if (y >= AXIS_H && x < this.gutterW) {
      const laneIdx = this.laneAtY(y);
      return laneIdx >= 0 ? { type: 'lane', lane: this.lanes[laneIdx] } : null;
    }
    if (y < AXIS_H || x < this.gutterW) return null;
    // Connectors first (thin, drawn on top; tight tolerance).
    for (let i = this.connectors.length - 1; i >= 0; i--) {
      const c = this.connectors[i];
      const from = this.byId.get(c.fromIntervalId);
      const to = this.byId.get(c.toIntervalId);
      if (from && to) {
        const pts = connectorRoute(this.rectFor(from, now), this.rectFor(to, now));
        if (hitTestPolyline(x, y, pts, CONNECTOR_TOL)) return { type: 'connector', connector: c };
      } else if (from || to) {
        const anchor = this.rectFor((from ?? to) as NInterval, now);
        const stub = this.stubRect(anchor, !!from);
        if (x >= stub.x && x <= stub.x + stub.w && y >= stub.y && y <= stub.y + stub.h) {
          return { type: 'connector', connector: c, missingEndpoint: from ? 'to' : 'from' };
        }
      }
    }
    // Intervals: topmost = last in draw order within the lane.
    const laneIdx = this.laneAtY(y);
    if (laneIdx >= 0) {
      const per = this.perLane[laneIdx];
      for (let i = per.length - 1; i >= 0; i--) {
        const n = per[i];
        if (n.start > this.renderView().end) continue;
        const r = expandHitRect(this.rectFor(n, now), HIT_MIN_W);
        if (x >= r.x && x <= r.x + r.w && y >= r.y && y <= r.y + r.h) {
          return { type: 'interval', interval: n.src, lane: this.lanes[n.laneIdx] };
        }
      }
    }
    // Markers (full-height lines, generous ±3px).
    const w = this.plotWidth();
    for (let i = this.markers.length - 1; i >= 0; i--) {
      const mx = this.gutterW + timeToX(this.markers[i].time, this.renderView(), w);
      if (Math.abs(x - mx) <= 3) {
        return { type: 'marker', marker: this.markers[i] };
      }
    }
    return null;
  }

  private laneAtY(y: number): number {
    const rel = y - AXIS_H + this.laneScroll;
    const { tops, heights } = this.layout;
    for (let i = 0; i < tops.length; i++) {
      if (rel >= tops[i] && rel < tops[i] + heights[i]) return i;
    }
    return -1;
  }

  private stubRect(anchor: HitRect, fromSide: boolean): HitRect {
    const x = fromSide ? anchor.x + anchor.w : anchor.x - 14;
    return { x, y: anchor.y + anchor.h / 2 - 5, w: 14, h: 10 };
  }

  // -- Pointer / wheel / keyboard interaction ------------------------------------------

  private toLocal(e: PointerEvent | WheelEvent | MouseEvent): { x: number; y: number } {
    const b = this.canvas.getBoundingClientRect();
    return { x: e.clientX - b.left, y: e.clientY - b.top };
  }

  private onWheel = (e: WheelEvent): void => {
    // Unconditional: the page must never scroll instead while the gesture
    // is over the canvas.
    e.preventDefault();
    this.noteInput();
    const p = this.toLocal(e);
    const route = routeWheel(e, this.maxLaneScroll() > 0);
    if (e.ctrlKey || e.metaKey) {
      if (e.deltaMode === 0) {
        // Pixel-precise trackpad pinch: apply 1:1, no smoothing, no lag.
        const anchor = xToTime(p.x - this.gutterW, this.view, this.plotWidth());
        this.applyUserView(zoomView(this.view, anchor, zoomFactorForWheel(route.zoomPx)));
        this.glidePx = 0;
      } else {
        // Discrete wheel steps: glide over ~130ms so they feel smooth.
        this.glidePx += route.zoomPx;
        this.glideX = p.x;
        this.invalidate();
      }
      return;
    }
    if (route.laneScrollPx !== 0) {
      this.laneScroll += route.laneScrollPx;
      this.clampLaneScroll();
    }
    if (route.panPx !== 0) {
      // A pan, possibly alongside the lane scroll (diagonal gesture).
      this.applyUserView(panView(this.view, route.panPx * this.msPerPx()), { pan: true });
    } else if (route.laneScrollPx !== 0) {
      this.invalidate();
    }
  };

  private stepGlide(dt: number): void {
    if (this.glidePx === 0) return;
    const ease = 1 - Math.exp(-dt / 45);
    let apply = this.glidePx * ease;
    if (Math.abs(this.glidePx - apply) < 0.5) apply = this.glidePx;
    this.glidePx -= apply;
    const anchor = xToTime(this.glideX - this.gutterW, this.view, this.plotWidth());
    this.applyUserView(zoomView(this.view, anchor, zoomFactorForWheel(apply)));
  }

  private onPointerDown = (e: PointerEvent): void => {
    if (e.button !== 0 && e.pointerType === 'mouse') return;
    this.noteInput();
    this.canvas.setPointerCapture(e.pointerId);
    const p = this.toLocal(e);
    this.pointers.set(e.pointerId, p);
    this.dragTotal = 0;
    this.downHit = this.pointers.size === 1 ? this.hitAt(p.x, p.y) : null;
    this.focus({ preventScroll: true });
  };

  private onPointerMove = (e: PointerEvent): void => {
    const p = this.toLocal(e);
    if (!this.pointers.has(e.pointerId)) {
      this.updateHover(p.x, p.y, e);
      return;
    }
    const prev = this.pointers.get(e.pointerId) as { x: number; y: number };
    this.pointers.set(e.pointerId, p);
    this.noteInput();
    if (this.pointers.size === 2) {
      // Pinch zoom + two-finger pan.
      const [a, b] = [...this.pointers.values()];
      const other = a.x === p.x && a.y === p.y ? b : a;
      const distNow = Math.hypot(p.x - other.x, p.y - other.y);
      const distPrev = Math.hypot(prev.x - other.x, prev.y - other.y);
      const midX = (p.x + other.x) / 2;
      const midPrevX = (prev.x + other.x) / 2;
      let next = panView(this.view, (midPrevX - midX) * this.msPerPx());
      const zoomed = distPrev > 8 && distNow > 8;
      if (zoomed) {
        const anchor = xToTime(midX - this.gutterW, next, this.plotWidth());
        next = zoomView(next, anchor, distNow / distPrev);
      }
      this.applyUserView(next, { pan: !zoomed });
      return;
    }
    const dx = p.x - prev.x;
    const dy = p.y - prev.y;
    this.dragTotal += Math.abs(dx) + Math.abs(dy);
    if (this.dragTotal > CLICK_SLOP) {
      this.downHit = null;
      this.canvas.style.cursor = 'grabbing';
      this.hideTooltip();
    }
    if (this.dragTotal > CLICK_SLOP || this.downHit === null) {
      let next = this.view;
      if (dx !== 0) next = panView(next, -dx * this.msPerPx());
      this.laneScroll -= dy;
      this.clampLaneScroll();
      this.applyUserView(next, { pan: true });
    }
  };

  private onPointerUp = (e: PointerEvent): void => {
    if (!this.pointers.delete(e.pointerId)) return;
    if (this.canvas.hasPointerCapture(e.pointerId)) this.canvas.releasePointerCapture(e.pointerId);
    this.canvas.style.cursor = '';
    if (this.pointers.size > 0) return;
    if (this.downHit && this.dragTotal <= CLICK_SLOP) {
      const hit = this.downHit;
      if (hit.type === 'interval') {
        this.dispatchEvent(new CustomEvent('intervalclick', { detail: { interval: hit.interval, lane: hit.lane } }));
      } else if (hit.type === 'connector') {
        this.dispatchEvent(new CustomEvent('connectorclick', { detail: { connector: hit.connector } }));
      } else if (hit.type === 'lane') {
        // A click on the gutter label — lets consumers make lanes navigable
        // (e.g. a lane per CI hook linking to that hook's page).
        this.dispatchEvent(new CustomEvent('laneclick', { detail: { lane: hit.lane } }));
      }
    }
    this.downHit = null;
  };

  private onPointerLeave = (): void => {
    this.lastMouse = null;
    if (this.pointers.size === 0) this.setHover(null, 0, 0);
  };

  private onKeyDown = (e: KeyboardEvent): void => {
    this.noteInput();
    const span = this.view.end - this.view.start;
    const center = (this.view.start + this.view.end) / 2;
    switch (e.key) {
      case 'ArrowLeft':
        this.applyUserView(panView(this.view, -span * (e.shiftKey ? 0.5 : 0.1)), { pan: true });
        break;
      case 'ArrowRight':
        this.applyUserView(panView(this.view, span * (e.shiftKey ? 0.5 : 0.1)), { pan: true });
        break;
      case 'ArrowUp':
        this.laneScroll -= 48;
        this.clampLaneScroll();
        this.invalidate();
        break;
      case 'ArrowDown':
        this.laneScroll += 48;
        this.clampLaneScroll();
        this.invalidate();
        break;
      case '+':
      case '=':
        this.applyUserView(zoomView(this.view, center, 1.5));
        break;
      case '-':
      case '_':
        this.applyUserView(zoomView(this.view, center, 1 / 1.5));
        break;
      case 'End':
        this.jumpToNow();
        break;
      case 'Home': {
        let earliest = Infinity;
        for (const per of this.perLane) if (per.length > 0) earliest = Math.min(earliest, per[0].start);
        const ex = this.coverage.exhaustedBefore;
        if (ex != null) earliest = Math.min(earliest, ex);
        if (Number.isFinite(earliest)) {
          this.applyUserView({ start: earliest - span * 0.1, end: earliest + span * 0.9 });
        }
        break;
      }
      default:
        return;
    }
    e.preventDefault();
  };

  // -- Hover / tooltip -----------------------------------------------------------

  private updateHover(x: number, y: number, e: MouseEvent): void {
    this.lastMouse = { x, y, cx: e.clientX, cy: e.clientY };
    const hit = this.hitAt(x, y);
    this.setHover(hit, e.clientX, e.clientY);
  }

  private setHover(hit: TimelineHit | null, clientX: number, clientY: number): void {
    const prevId = this.hoverIntervalId;
    const nextId = hit?.type === 'interval' ? hit.interval.id : null;
    this.hoverIntervalId = nextId;
    this.hover = hit;
    this.canvas.style.cursor = hit ? 'pointer' : '';
    if (nextId !== prevId) {
      this.dispatchEvent(
        new CustomEvent('intervalhover', {
          detail: hit?.type === 'interval' ? { interval: hit.interval, lane: hit.lane } : { interval: null, lane: null },
        }),
      );
      this.invalidate();
    }
    if (hit) this.showTooltip(hit, clientX, clientY);
    else this.hideTooltip();
  }

  private showTooltip(hit: TimelineHit, clientX: number, clientY: number): void {
    // Lane tooltips only earn their keep when the gutter label is truncated.
    if (hit.type === 'lane') {
      const fits = fitText(hit.lane.label, this.gutterW - 16, this.charW) === hit.lane.label;
      if (fits) {
        this.hideTooltip();
        return;
      }
    }
    const tt = this.tooltipEl;
    tt.textContent = '';
    let content: string | Node | null | undefined;
    if (this.tooltipForFn) {
      content = this.tooltipForFn(hit);
      if (content == null) {
        this.hideTooltip();
        return;
      }
    } else {
      content = this.defaultTooltip(hit);
      if (content == null) {
        this.hideTooltip();
        return;
      }
    }
    if (typeof content === 'string') tt.textContent = content;
    else tt.append(content);
    tt.classList.add('visible');
    // Position near the cursor, flipped to stay inside the host.
    const host = this.getBoundingClientRect();
    let x = clientX - host.left + 14;
    let y = clientY - host.top + 16;
    const tw = tt.offsetWidth;
    const th = tt.offsetHeight;
    if (x + tw > this.cssW - 6) x = Math.max(6, clientX - host.left - tw - 12);
    if (y + th > this.cssH - 6) y = Math.max(6, clientY - host.top - th - 12);
    tt.style.left = `${Math.round(x)}px`;
    tt.style.top = `${Math.round(y)}px`;
  }

  private hideTooltip(): void {
    this.tooltipEl.classList.remove('visible');
  }

  private defaultTooltip(hit: TimelineHit): Node | null {
    const tz = this.tzOffsetMs();
    const frag = document.createDocumentFragment();
    const row = (k: string, v: string): void => {
      const r = document.createElement('div');
      r.className = 'tt-row';
      const kEl = document.createElement('span');
      kEl.className = 'tt-k';
      kEl.textContent = k;
      const vEl = document.createElement('span');
      vEl.className = 'tt-v';
      vEl.textContent = v;
      r.append(kEl, vEl);
      frag.append(r);
    };
    if (hit.type === 'interval') {
      const iv = hit.interval;
      const n = this.byId.get(iv.id);
      if (!n) return null;
      const title = document.createElement('div');
      title.className = 'tt-title';
      const swatch = document.createElement('span');
      swatch.className = 'tt-swatch';
      swatch.style.background = this.resolved(n.catKey, n.state, this.overrideColor(n)).fill;
      title.append(swatch, document.createTextNode(n.label || n.id));
      frag.append(title);
      row('lane', hit.lane.label);
      row('category', n.catKey);
      if (n.state) row('state', n.state);
      const now = this.nowMs();
      const end = n.end ?? now;
      const fine = end - n.start < 10_000;
      row('start', formatTimeFull(n.start, tz, fine));
      row('end', n.end === null ? 'ongoing' : formatTimeFull(n.end, tz, fine));
      row('duration', formatDuration(end - n.start) + (n.end === null ? ' …' : ''));
      if (n.segs) {
        for (const s of n.segs) {
          row(s.kind, `${formatDuration((s.end ?? end) - s.start)}`);
        }
      }
    } else if (hit.type === 'connector') {
      const c = hit.connector;
      const title = document.createElement('div');
      title.className = 'tt-title';
      title.textContent = c.label ?? c.kind ?? 'link';
      frag.append(title);
      const name = (id: string): string => {
        const n = this.byId.get(id);
        return n ? n.label || n.id : `${id} (missing)`;
      };
      row('from', name(c.fromIntervalId));
      row('to', name(c.toIntervalId));
      if (hit.missingEndpoint) row('note', `${hit.missingEndpoint} endpoint not loaded`);
    } else if (hit.type === 'marker') {
      const title = document.createElement('div');
      title.className = 'tt-title';
      title.textContent = hit.marker.label ?? 'marker';
      frag.append(title);
      row('time', formatTimeFull(toMs(hit.marker.time), tz));
    } else {
      const title = document.createElement('div');
      title.className = 'tt-title';
      title.textContent = hit.lane.label;
      frag.append(title);
      row('lane id', hit.lane.id);
    }
    return frag;
  }

  private overrideColor(n: NInterval): string | null {
    if (!this.colorForFn) return null;
    return this.colorForFn(n.src, this.lanes[n.laneIdx]) ?? null;
  }

  // -- Drawing -------------------------------------------------------------------

  private draw(): void {
    const raw = typeof devicePixelRatio === 'number' && devicePixelRatio > 0 ? devicePixelRatio : 1;
    if (Math.min(2, raw) !== this.dpr) this.resizeBackingStore();
    const ctx = (this.ctx ??= this.canvas.getContext('2d'));
    if (!ctx || this.cssW < 4 || this.cssH < 4) return;
    const t = this.theme;
    const dpr = this.dpr;
    const w = this.cssW;
    const h = this.cssH;
    const now = this.nowMs();
    const gx = this.gutterW;
    const plotW = this.plotWidth();
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

    ctx.fillStyle = t.bg;
    ctx.fillRect(0, 0, w, h);

    this.drawAxisAndGrid(ctx, now);
    this.drawLanes(ctx);

    // Plot-area clip for time-domain content.
    ctx.save();
    ctx.beginPath();
    ctx.rect(gx, AXIS_H, plotW, h - AXIS_H);
    ctx.clip();

    this.drawCoverage(ctx, now);
    this.drawIntervals(ctx, now);
    this.drawConnectors(ctx, now);
    this.drawMarkers(ctx);
    this.drawNowLine(ctx, now);

    ctx.restore();

    // Axis base hairline over everything.
    ctx.strokeStyle = t.hairline;
    ctx.lineWidth = 1 / dpr;
    ctx.beginPath();
    const yAxis = snap(AXIS_H, dpr);
    ctx.moveTo(0, yAxis);
    ctx.lineTo(w, yAxis);
    ctx.stroke();
  }

  private drawAxisAndGrid(ctx: CanvasRenderingContext2D, now: number): void {
    const t = this.theme;
    const dpr = this.dpr;
    const w = this.cssW;
    const h = this.cssH;
    const gx = this.gutterW;
    const plotW = this.plotWidth();
    const tz = this.tzOffsetMs();
    const maxTicks = Math.max(2, Math.floor(plotW / 88));
    const rv = this.renderView();
    const span = rv.end - rv.start;
    const step = timeTickStep(span, maxTicks);
    const ticks = timeTicks(rv, maxTicks, tz);

    ctx.font = this.fontAxis;
    ctx.textBaseline = 'middle';
    const hairline = 1 / dpr;
    for (const tick of ticks) {
      const x = snap(gx + timeToX(tick, rv, plotW), dpr);
      if (x < gx) continue;
      const isDay = (tick + tz) % 86_400_000 === 0;
      ctx.strokeStyle = t.grid;
      ctx.lineWidth = isDay ? hairline * 2 : hairline;
      ctx.beginPath();
      ctx.moveTo(x, AXIS_H);
      ctx.lineTo(x, h);
      ctx.stroke();
      ctx.fillStyle = isDay ? t.fg : t.muted;
      ctx.textAlign = 'center';
      const label = formatTimeTick(tick, step, tz);
      // Keep edge labels inside the canvas.
      const half = (label.length * this.charW * 0.92) / 2;
      const lx = Math.min(w - half - 2, Math.max(gx + half + 2, x));
      ctx.fillText(label, lx, AXIS_H / 2 + 0.5);
    }
    // Context date in the gutter corner when the ticks themselves are
    // sub-day (a date-step axis already says the date on every tick).
    if (step < 86_400_000 && this.lanes.length > 0) {
      ctx.fillStyle = t.muted;
      ctx.textAlign = 'left';
      const dateLabel = formatTimeFull(rv.start, tz).split(' ').slice(0, 2).join(' ');
      ctx.fillText(dateLabel, 4, AXIS_H / 2 + 0.5);
    }
    void now;
  }

  private drawLanes(ctx: CanvasRenderingContext2D): void {
    const t = this.theme;
    const dpr = this.dpr;
    const w = this.cssW;
    const h = this.cssH;
    const { tops, heights } = this.layout;
    const hairline = 1 / dpr;
    ctx.font = this.fontBar;
    ctx.textBaseline = 'middle';
    ctx.textAlign = 'left';
    for (let i = 0; i < this.lanes.length; i++) {
      const top = AXIS_H + tops[i] - this.laneScroll;
      const lh = heights[i];
      if (top + lh < AXIS_H || top > h) continue;
      if (i % 2 === 1 && t.rowTint !== 'none') {
        ctx.fillStyle = t.rowTint;
        ctx.fillRect(0, Math.max(AXIS_H, top), w, Math.min(top + lh, h) - Math.max(AXIS_H, top));
      }
      const yBottom = snap(top + lh, dpr);
      if (yBottom > AXIS_H && yBottom < h) {
        ctx.strokeStyle = t.hairline;
        ctx.lineWidth = hairline;
        ctx.beginPath();
        ctx.moveTo(0, yBottom);
        ctx.lineTo(w, yBottom);
        ctx.stroke();
      }
      const label = fitText(this.lanes[i].label, this.gutterW - 16, this.charW);
      if (label !== '' && top + lh / 2 > AXIS_H + 4 && top + lh / 2 < h - 2) {
        ctx.fillStyle = t.muted;
        ctx.fillText(label, 8, top + lh / 2 + 0.5);
      }
    }
    // Gutter | plot separator.
    ctx.strokeStyle = t.hairline;
    ctx.lineWidth = hairline;
    ctx.beginPath();
    const xg = snap(this.gutterW, dpr);
    ctx.moveTo(xg, AXIS_H);
    ctx.lineTo(xg, h);
    ctx.stroke();
  }

  private drawCoverage(ctx: CanvasRenderingContext2D, now: number): void {
    if (!this.loadRangeFn) return;
    const t = this.theme;
    const gx = this.gutterW;
    const plotW = this.plotWidth();
    const h = this.cssH;
    const rv = this.renderView();
    const probeEnd = Math.min(rv.end, now);
    if (probeEnd > rv.start) {
      const gaps = this.coverage.uncoveredIn({ start: rv.start, end: probeEnd });
      const pending = this.coverage.pending();
      for (const gap of gaps) {
        const x0 = gx + timeToX(gap.start, rv, plotW);
        const x1 = gx + timeToX(gap.end, rv, plotW);
        if (x1 - x0 < 1) continue;
        const busy = pending !== null && pending.start < gap.end && pending.end > gap.start;
        const pat = this.patternFor('hatch', busy ? withAlpha(t.muted, 0.35) : withAlpha(t.muted, 0.18));
        ctx.fillStyle = pat ?? withAlpha(t.muted, 0.08);
        ctx.save();
        if (busy && !this.reducedMotion) ctx.translate((now / 40) % 7, 0);
        ctx.fillRect(x0 - 7, AXIS_H, x1 - x0 + 7, h - AXIS_H);
        ctx.restore();
        if (busy && x1 - x0 > 90) {
          ctx.fillStyle = t.muted;
          ctx.font = this.fontAxis;
          ctx.textAlign = 'center';
          ctx.textBaseline = 'middle';
          ctx.fillText('loading…', (x0 + x1) / 2, AXIS_H + 14);
        }
      }
    }
    // Explicit end-of-history boundary.
    const ex = this.coverage.exhaustedBefore;
    if (ex !== null && ex >= rv.start && ex <= rv.end) {
      const x = snap(gx + timeToX(ex, rv, plotW), this.dpr);
      // The void before history: clearly darker than the plot bg.
      if (x > gx) {
        ctx.fillStyle = 'rgba(0, 0, 0, 0.5)';
        ctx.fillRect(gx, AXIS_H, x - gx, h - AXIS_H);
      }
      ctx.strokeStyle = t.muted;
      ctx.lineWidth = 1;
      ctx.setLineDash(MARKER_DASH);
      ctx.beginPath();
      ctx.moveTo(x, AXIS_H);
      ctx.lineTo(x, h);
      ctx.stroke();
      ctx.setLineDash(EMPTY_DASH);
      ctx.fillStyle = t.muted;
      ctx.font = this.fontAxis;
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      ctx.fillText(this.getAttribute('history-end-text') ?? 'history ends here', x + 6, h - 12);
    }
  }

  private drawIntervals(ctx: CanvasRenderingContext2D, now: number): void {
    const t = this.theme;
    const dpr = this.dpr;
    const h = this.cssH;
    const m = this.metrics();
    const { tops, heights } = this.layout;
    ctx.font = this.fontBar;
    ctx.textBaseline = 'middle';
    const rv = this.renderView();
    for (let laneIdx = 0; laneIdx < this.perLane.length; laneIdx++) {
      const laneTop = AXIS_H + tops[laneIdx] - this.laneScroll;
      if (laneTop + heights[laneIdx] < AXIS_H || laneTop > h) continue;
      const per = this.perLane[laneIdx];
      for (let i = 0; i < per.length; i++) {
        const n = per[i];
        if (n.start > rv.end) break; // sorted by start
        if ((n.end ?? now) < rv.start && n.end !== null) continue;
        this.drawInterval(ctx, n, now);
      }
    }
    void dpr;
    void t;
  }

  private drawInterval(ctx: CanvasRenderingContext2D, n: NInterval, now: number): void {
    const t = this.theme;
    const dpr = this.dpr;
    const r = this.rectFor(n, now);
    const bh = this.metrics().trackHeight;
    const style = this.resolved(n.catKey, n.state, this.overrideColor(n));
    const hovered = this.hoverIntervalId === n.id;

    // Bar vs pip from the DURATION mapped through the current scale —
    // translation-invariant, so a scrolling viewport can never flip an
    // event's shape (a rounded-coordinate width oscillates ±1px with
    // subpixel phase). Zero/near-zero-duration events are pips; anything
    // wider draws as a bar, clamped to MIN_BAR_PX so a real duration is
    // never demoted to a pip by rounding.
    const trueW = durationWidthPx(n.start, n.end ?? now, this.renderView(), this.plotWidth());
    if (isInstantWidth(trueW)) {
      this.drawInstant(ctx, n, style, r.x + r.w / 2, r.y + bh / 2, bh, hovered);
      return;
    }

    // Unrounded coordinates on purpose: renderView is the single global
    // rounding step; rounding again per bar would jiggle neighbors
    // relative to each other during fractional translations.
    const x0 = r.x;
    const bw = Math.max(r.w, MIN_BAR_PX);
    const x1 = x0 + bw;
    const y = r.y;
    const radius = Math.min(3, bh / 3, bw / 2);
    const path = new Path2D();
    path.roundRect(x0, y, bw, bh, radius);

    // Body fill.
    if (style.pattern === 'outline') {
      ctx.fillStyle = withAlpha(style.fill, 0.1);
      ctx.fill(path);
    } else if (style.pattern === 'hatch' || style.pattern === 'stipple') {
      ctx.fillStyle = withAlpha(style.fill, style.pattern === 'hatch' ? 0.22 : 0.75);
      ctx.fill(path);
      const pat = this.patternFor(style.pattern, style.pattern === 'stipple' ? withAlpha('#000000', 0.4) : style.fill);
      if (pat) {
        ctx.save();
        ctx.clip(path);
        ctx.fillStyle = pat;
        ctx.fillRect(x0, y, bw, bh);
        ctx.restore();
      }
    } else {
      ctx.fillStyle = style.fill;
      ctx.fill(path);
    }

    // Phase segments, clipped to the bar.
    if (n.segs) {
      ctx.save();
      ctx.clip(path);
      const w = this.plotWidth();
      const rv = this.renderView();
      for (const s of n.segs) {
        const sx0 = Math.max(x0, this.gutterW + timeToX(s.start, rv, w));
        const sx1 = Math.min(x1, this.gutterW + timeToX(s.end ?? (n.end ?? now), rv, w));
        if (sx1 - sx0 < 0.5) continue;
        const ss = this.resolved(n.catKey, s.kind, null);
        if (ss.pattern === 'hatch' || ss.pattern === 'stipple') {
          ctx.fillStyle = withAlpha(ss.fill, 0.2);
          ctx.fillRect(sx0, y, sx1 - sx0, bh);
          const pat = this.patternFor(ss.pattern, ss.fill);
          if (pat) {
            ctx.fillStyle = pat;
            ctx.fillRect(sx0, y, sx1 - sx0, bh);
          }
        } else {
          ctx.fillStyle = ss.pattern === 'outline' ? withAlpha(ss.fill, 0.12) : ss.fill;
          ctx.fillRect(sx0, y, sx1 - sx0, bh);
        }
        // Hairline phase boundary.
        ctx.fillStyle = withAlpha('#000000', 0.35);
        ctx.fillRect(sx0, y, 1 / dpr, bh);
      }
      ctx.restore();
    }

    // Ongoing: animated leading edge at the live end.
    if (n.end === null && x1 > this.gutterW) {
      const pulse = this.reducedMotion ? 0.55 : 0.4 + 0.25 * Math.sin(now / 550);
      const gw = Math.min(14, bw);
      const grad = ctx.createLinearGradient(x1 - gw, 0, x1, 0);
      grad.addColorStop(0, withAlpha('#ffffff', 0));
      grad.addColorStop(1, withAlpha('#ffffff', pulse));
      ctx.save();
      ctx.clip(path);
      ctx.fillStyle = grad;
      ctx.fillRect(x1 - gw, y, gw, bh);
      ctx.restore();
    }

    // Border.
    ctx.strokeStyle = style.border;
    ctx.lineWidth = style.borderWidth;
    if (style.dash) ctx.setLineDash(style.dash);
    ctx.stroke(path);
    if (style.dash) ctx.setLineDash(EMPTY_DASH);

    // Corner glyph (emphasis): a filled notch triangle, top-right.
    if (style.glyph === 'bang' && bw >= 8) {
      const g = Math.min(8, bh * 0.5);
      ctx.fillStyle = t.emphasis;
      ctx.beginPath();
      ctx.moveTo(x1 - g, y);
      ctx.lineTo(x1, y);
      ctx.lineTo(x1, y + g);
      ctx.closePath();
      ctx.fill();
    } else if (style.glyph === 'dot' && bw >= 8) {
      ctx.fillStyle = style.border;
      ctx.beginPath();
      ctx.arc(x0 + bh * 0.32, y + bh * 0.32, 1.8, 0, Math.PI * 2);
      ctx.fill();
    }

    // Label — never allowed to spill out of the bar; sticks to the plot's
    // left edge while the bar's start is scrolled off-screen.
    const pad = 5;
    const glyphPad = style.glyph === 'bang' ? 8 : 0;
    const labelX = Math.max(x0, this.gutterW) + pad;
    const label = fitText(n.label, x1 - labelX - pad - glyphPad, this.charW);
    if (label !== '') {
      ctx.fillStyle = style.labelColor;
      ctx.fillText(label, labelX, y + bh / 2 + 0.5);
    }

    if (hovered) {
      ctx.strokeStyle = withAlpha('#ffffff', 0.75);
      ctx.lineWidth = 1.25;
      ctx.stroke(path);
    }
  }

  private drawInstant(
    ctx: CanvasRenderingContext2D,
    n: NInterval,
    style: ResolvedStyle,
    cx: number,
    cy: number,
    trackH: number,
    hovered: boolean,
  ): void {
    const t = this.theme;
    const r = Math.min(trackH * 0.42, 8);
    const rx = r * 0.78;
    ctx.beginPath();
    ctx.moveTo(cx, cy - r);
    ctx.lineTo(cx + rx, cy);
    ctx.lineTo(cx, cy + r);
    ctx.lineTo(cx - rx, cy);
    ctx.closePath();
    ctx.fillStyle = style.pattern === 'outline' ? withAlpha(style.fill, 0.15) : style.fill;
    ctx.fill();
    const emphasis = style.glyph === 'bang' || style.border === t.emphasis;
    ctx.strokeStyle = style.border;
    ctx.lineWidth = emphasis ? 2 : 1;
    ctx.stroke();
    if (emphasis) {
      // Unmissable: a stem above the diamond, like an exclamation.
      ctx.strokeStyle = t.emphasis;
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(cx, cy - r - 1.5);
      ctx.lineTo(cx, cy - r - 5);
      ctx.stroke();
    }
    if (hovered) {
      ctx.strokeStyle = withAlpha('#ffffff', 0.75);
      ctx.lineWidth = 1.25;
      ctx.beginPath();
      ctx.moveTo(cx, cy - r - 2);
      ctx.lineTo(cx + rx + 2, cy);
      ctx.lineTo(cx, cy + r + 2);
      ctx.lineTo(cx - rx - 2, cy);
      ctx.closePath();
      ctx.stroke();
    }
  }

  private drawConnectors(ctx: CanvasRenderingContext2D, now: number): void {
    const t = this.theme;
    if (this.connectors.length === 0) return;
    const hovered = this.hover?.type === 'connector' ? this.hover.connector : null;
    for (const c of this.connectors) {
      const from = this.byId.get(c.fromIntervalId);
      const to = this.byId.get(c.toIntervalId);
      const isHover = hovered === c;
      ctx.strokeStyle = isHover ? t.fg : withAlpha(t.muted, 0.8);
      ctx.lineWidth = isHover ? 1.6 : 1;
      ctx.lineJoin = 'round';
      if (from && to) {
        const fr = this.rectFor(from, now);
        const tr = this.rectFor(to, now);
        const pts = connectorRoute(fr, tr);
        if (!this.routeVisible(pts)) continue;
        ctx.beginPath();
        ctx.moveTo(pts[0].x, pts[0].y);
        for (let i = 1; i < pts.length; i++) ctx.lineTo(pts[i].x, pts[i].y);
        ctx.stroke();
        // Endpoint dots; highlighted while hovered.
        ctx.fillStyle = isHover ? t.fg : withAlpha(t.muted, 0.9);
        const last = pts[pts.length - 1];
        ctx.beginPath();
        ctx.arc(pts[0].x, pts[0].y, isHover ? 2.4 : 1.8, 0, Math.PI * 2);
        ctx.arc(last.x, last.y, isHover ? 2.4 : 1.8, 0, Math.PI * 2);
        ctx.fill();
      } else if (from || to) {
        // Stub: one endpoint is missing/not loaded — mark it explicitly.
        const anchor = this.rectFor((from ?? to) as NInterval, now);
        const sr = this.stubRect(anchor, !!from);
        const yMid = sr.y + sr.h / 2;
        ctx.setLineDash(MARKER_DASH);
        ctx.beginPath();
        ctx.moveTo(from ? sr.x : sr.x + sr.w, yMid);
        ctx.lineTo(from ? sr.x + sr.w - 4 : sr.x + 4, yMid);
        ctx.stroke();
        ctx.setLineDash(EMPTY_DASH);
        ctx.beginPath();
        ctx.arc(from ? sr.x + sr.w - 3 : sr.x + 3, yMid, 2.4, 0, Math.PI * 2);
        ctx.stroke();
      }
    }
  }

  private routeVisible(pts: { x: number; y: number }[]): boolean {
    let minX = Infinity;
    let maxX = -Infinity;
    for (const p of pts) {
      if (p.x < minX) minX = p.x;
      if (p.x > maxX) maxX = p.x;
    }
    return maxX >= this.gutterW && minX <= this.cssW;
  }

  private drawMarkers(ctx: CanvasRenderingContext2D): void {
    const t = this.theme;
    const gx = this.gutterW;
    const plotW = this.plotWidth();
    const h = this.cssH;
    ctx.font = this.fontAxis;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'middle';
    const rv = this.renderView();
    for (const m of this.markers) {
      if (m.time < rv.start || m.time > rv.end) continue;
      const x = snap(gx + timeToX(m.time, rv, plotW), this.dpr);
      const color = m.kind === 'emphasis' ? t.emphasis : t.muted;
      ctx.strokeStyle = color;
      ctx.lineWidth = 1;
      ctx.setLineDash(MARKER_DASH);
      ctx.beginPath();
      ctx.moveTo(x, AXIS_H);
      ctx.lineTo(x, h);
      ctx.stroke();
      ctx.setLineDash(EMPTY_DASH);
      if (m.label) {
        ctx.fillStyle = color;
        ctx.fillText(m.label, x + 5, AXIS_H + 9);
      }
    }
  }

  private drawNowLine(ctx: CanvasRenderingContext2D, now: number): void {
    const rv = this.renderView();
    if (now < rv.start || now > rv.end) return;
    const t = this.theme;
    const x = snap(this.gutterW + timeToX(now, rv, this.plotWidth()), this.dpr);
    ctx.strokeStyle = withAlpha(t.now, 0.85);
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(x, AXIS_H);
    ctx.lineTo(x, this.cssH);
    ctx.stroke();
    // Small cap at the axis.
    ctx.fillStyle = t.now;
    ctx.beginPath();
    ctx.moveTo(x - 3.5, AXIS_H);
    ctx.lineTo(x + 3.5, AXIS_H);
    ctx.lineTo(x, AXIS_H + 4.5);
    ctx.closePath();
    ctx.fill();
  }
}

// -- Helpers -----------------------------------------------------------------------

function readProp(cs: CSSStyleDeclaration, name: string, fallback: string): string {
  const v = cs.getPropertyValue(name).trim();
  return v !== '' ? v : fallback;
}

function readNum(cs: CSSStyleDeclaration, name: string, fallback: number, min: number, max: number): number {
  const v = parseFloat(cs.getPropertyValue(name));
  if (!Number.isFinite(v)) return fallback;
  return Math.min(max, Math.max(min, v));
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}

/** Snap a CSS-px coordinate to the device-pixel grid + half-pixel (crisp 1px lines). */
function snap(v: number, dpr: number): number {
  return (Math.round(v * dpr) + 0.5) / dpr;
}

/**
 * Apply an alpha to any supported color form we produce (#rrggbb, rgb/rgba,
 * hsl/hsla, oklch). Unknown forms are returned unchanged.
 */
function withAlpha(color: string, alpha: number): string {
  const a = Math.round(alpha * 1000) / 1000;
  if (color.startsWith('#')) {
    const hex = color.slice(1);
    if (hex.length === 6 || hex.length === 3) {
      const full = hex.length === 3 ? hex.split('').map((ch) => ch + ch).join('') : hex;
      const r = parseInt(full.slice(0, 2), 16);
      const g = parseInt(full.slice(2, 4), 16);
      const b = parseInt(full.slice(4, 6), 16);
      return `rgba(${r}, ${g}, ${b}, ${a})`;
    }
    return color;
  }
  if (color.startsWith('oklch(') && !color.includes('/')) {
    return `${color.slice(0, -1)} / ${a})`;
  }
  const rgbHsl = color.match(/^(rgb|hsl)a?\(([^)]+)\)$/);
  if (rgbHsl) {
    const parts = rgbHsl[2].split(',').map((s) => s.trim());
    return `${rgbHsl[1]}a(${parts.slice(0, 3).join(', ')}, ${a})`;
  }
  return color;
}

// Auto-register under the conventional tag name, but never clobber an existing
// definition (a consumer may have registered their own, or loaded this twice).
if (typeof customElements !== 'undefined' && !customElements.get('timeline-view')) {
  customElements.define('timeline-view', TimelineViewElement);
}
