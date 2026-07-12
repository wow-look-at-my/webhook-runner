// Vendored from wow-look-at-my/js-snippets src/ui/timeline-view-math.ts (PR #34, vendored 2026-07-12). Do not edit here — fix upstream and re-copy.
// Pure math for the <timeline-view> element: time<->pixel scales, an
// anchor-preserving zoom (with wheel-delta normalization and gesture
// routing), the follow-now engage/disengage rule, a device-pixel-snapped
// render origin (whole-pixel scrolling), a nice TIME tick ladder
// (1/2/5/10/15/30 across ms → s → min → h → days, with per-step label
// granularity), greedy first-fit sub-track packing (whole-set and
// visible-window variants), lane layout, label-fit and instant-interval
// (zero/near-zero DURATION) helpers, stable category → hue hashing,
// data-coverage / range-request bookkeeping for async history loading,
// render-loop pacing tiers, and hit-testing. No DOM or browser APIs —
// everything here runs (and is tested) under node; ui/timeline-view.ts is
// the canvas-bound half that consumes it.

// -- Data model ------------------------------------------------------------------

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
  /** Phase start (ms since epoch, or a Date). Clamped into the parent interval. */
  start: number | Date;
  /** Phase end; null/undefined = runs to the parent interval's end. */
  end?: number | Date | null;
  /** Style-map key for this phase (e.g. a built-in like 'dim' or 'hatch'). */
  kind: string;
}

/** One bar on a lane: [start, end] on the shared time axis. */
export interface TimelineInterval {
  /** Unique interval id — connectors reference it, mergeData dedupes on it. */
  id: string;
  /** The lane this interval belongs to. */
  laneId: string;
  /** Start time (ms since epoch, or a Date). */
  start: number | Date;
  /** End time; null/undefined = ongoing (renders to the live "now" edge). */
  end?: number | Date | null;
  /** Text drawn inside the bar when it fits (never overflows the bar). */
  label?: string;
  /** Color key: same category = same hue. Defaults to lane.group, then laneId. */
  category?: string;
  /** Style-map key: rendering treatment (e.g. 'failed', 'dim', 'hatch'). */
  state?: string;
  /** Phases within the bar, each styled via its `kind`. */
  segments?: TimelineSegment[];
  /** Opaque consumer payload — echoed back in events and tooltip callbacks. */
  data?: unknown;
}

/** A line between two intervals (e.g. a handoff or dependency of the consumer's choosing). */
export interface TimelineConnector {
  fromIntervalId: string;
  toIntervalId: string;
  /** Consumer-defined kind — echoed in events/tooltips. */
  kind?: string;
  /** Tooltip text for the connector. */
  label?: string;
}

/** A vertical time marker across all lanes. */
export interface TimelineMarker {
  time: number | Date;
  label?: string;
  /** 'emphasis' renders in the emphasis color; anything else is muted. */
  kind?: string;
}

/** Accept ms-since-epoch or Date anywhere a time enters the API. */
export function toMs(t: number | Date): number {
  return typeof t === 'number' ? t : t.getTime();
}

// -- Viewport / scale --------------------------------------------------------------

/** A visible time window [start, end] in ms since epoch. */
export interface TimeView {
  start: number;
  end: number;
}

/** Hard zoom clamps: ~2 s to ~7 days of visible span. */
export const MIN_SPAN_MS = 2_000;
export const MAX_SPAN_MS = 7 * 86_400_000;

/** Time → x in [0, width] for the view (un-clamped; callers cull). */
export function timeToX(t: number, view: TimeView, width: number): number {
  return ((t - view.start) / (view.end - view.start)) * width;
}

/** x → time for the view (inverse of timeToX). */
export function xToTime(x: number, view: TimeView, width: number): number {
  return view.start + (x / width) * (view.end - view.start);
}

/** Shift the view by dt ms (positive = later). */
export function panView(view: TimeView, dt: number): TimeView {
  return { start: view.start + dt, end: view.end + dt };
}

/**
 * Zoom the view by `factor` (> 1 zooms in) keeping `anchor` at the same
 * on-screen fraction — the time under the cursor stays under the cursor.
 * The span is clamped to [minSpan, maxSpan]; clamping preserves the anchor
 * fraction, so the invariant holds even at the clamp.
 */
export function zoomView(
  view: TimeView,
  anchor: number,
  factor: number,
  minSpan = MIN_SPAN_MS,
  maxSpan = MAX_SPAN_MS,
): TimeView {
  const span = view.end - view.start;
  const f = Number.isFinite(factor) && factor > 0 ? factor : 1;
  let next = span / f;
  if (next < minSpan) next = minSpan;
  else if (next > maxSpan) next = maxSpan;
  const frac = span > 0 ? (anchor - view.start) / span : 0.5;
  const start = anchor - frac * next;
  return { start, end: start + next };
}

/**
 * Normalize a WheelEvent delta to pixels. deltaMode 0 (pixel) passes
 * through 1:1; 1 (line) and 2 (page) — discrete wheels — convert via the
 * given heights. Non-finite deltas normalize to 0.
 */
export function wheelDeltaToPixels(delta: number, deltaMode: number, lineHeight = 16, pageHeight = 800): number {
  if (!Number.isFinite(delta)) return 0;
  if (deltaMode === 1) return delta * lineHeight;
  if (deltaMode === 2) return delta * pageHeight;
  return delta;
}

/** Pixels of zoom wheel per doubling of the scale. */
export const ZOOM_PX_PER_DOUBLE = 260;

/**
 * Continuous exponential zoom factor for a wheel delta in pixels: negative
 * (scroll up / pinch out) zooms in. ZOOM_PX_PER_DOUBLE px doubles the scale,
 * so factors compose exactly: f(a) * f(b) === f(a + b).
 */
export function zoomFactorForWheel(deltaPx: number): number {
  return Math.pow(2, -deltaPx / ZOOM_PX_PER_DOUBLE);
}

/** The parts of a WheelEvent the gesture router reads. */
export interface WheelInput {
  deltaX: number;
  deltaY: number;
  deltaMode: number;
  ctrlKey: boolean;
  metaKey: boolean;
  shiftKey: boolean;
}

/** Where a wheel gesture's energy goes (all deltaMode-normalized pixels). */
export interface WheelRoute {
  /** Zoom (ctrl/meta + wheel), from the vertical delta. 0 = no zoom. */
  zoomPx: number;
  /** Horizontal time pan. */
  panPx: number;
  /** Vertical lane-stack scroll. */
  laneScrollPx: number;
}

/**
 * Route a wheel/trackpad gesture: ctrl/meta+wheel zooms; shift+wheel pans
 * time (a vertical wheel pans horizontally); otherwise deltaX ALWAYS pans
 * time, while deltaY scrolls the lane stack when it overflows and joins the
 * time pan when it doesn't. A diagonal two-finger gesture therefore applies
 * both axes in one event, and a pure horizontal swipe is never dropped.
 */
export function routeWheel(e: WheelInput, lanesOverflow: boolean): WheelRoute {
  const dx = wheelDeltaToPixels(e.deltaX, e.deltaMode);
  const dy = wheelDeltaToPixels(e.deltaY, e.deltaMode);
  if (e.ctrlKey || e.metaKey) return { zoomPx: dy, panPx: 0, laneScrollPx: 0 };
  if (e.shiftKey) return { zoomPx: 0, panPx: dy || dx, laneScrollPx: 0 };
  return {
    zoomPx: 0,
    panPx: dx + (lanesOverflow ? 0 : dy),
    laneScrollPx: lanesOverflow ? dy : 0,
  };
}

// -- Follow-now rule ---------------------------------------------------------------

/** Fraction of the span "now" sits in from the right edge while following. */
export const FOLLOW_LEAD_FRAC = 0.02;
/** A gesture ending with the right edge this close to "now" re-engages follow. */
export const FOLLOW_SNAP_FRAC = 0.02;

/**
 * Whether follow-now is engaged after a user-driven viewport change.
 *
 * A PURE PAN that moves the right edge backward (into the past) always
 * disengages. This is load-bearing for trackpads: a two-finger pan arrives
 * as many small wheel events, and an unconditional magnetic rule re-pinned
 * the view after every event smaller than the snap zone — making it
 * impossible to leave "now" by scrolling. Everything else (zooms, forward
 * pans, programmatic jumps) keeps the magnetic rule: engaged iff the right
 * edge lands within FOLLOW_SNAP_FRAC of "now" — so zooming while pinned
 * stays pinned, and panning forward re-docks at the live edge.
 */
export function followAfterGesture(prevEnd: number, next: TimeView, now: number, isPan: boolean): boolean {
  if (isPan && next.end < prevEnd) return false;
  return next.end >= now - (next.end - next.start) * FOLLOW_SNAP_FRAC;
}

// -- Whole-pixel scrolling ------------------------------------------------------------

/**
 * Snap a view's ORIGIN to the device-pixel grid, span preserved: with the
 * snapped view, any fixed time's x keeps a constant subpixel phase, so a
 * moving viewport translates the whole scene in WHOLE device-pixel steps
 * and bars keep exact relative offsets. This is the ONE place rounding may
 * touch time→x. Rounding per element instead makes neighboring bars round
 * in different directions as a fractional translation slides under them —
 * they visibly jiggle relative to each other. Snapping happens in DEVICE
 * pixels (dpr-aware) so HiDPI displays don't land on half pixels.
 */
export function snapViewToDevicePixels(view: TimeView, plotWidthCss: number, dpr: number): TimeView {
  const span = view.end - view.start;
  const msPerDevPx = span / (plotWidthCss * dpr);
  if (!Number.isFinite(msPerDevPx) || msPerDevPx <= 0) return view;
  const start = Math.round(view.start / msPerDevPx) * msPerDevPx;
  return { start, end: start + span };
}

// -- Time ticks --------------------------------------------------------------------

/**
 * The tick ladder, in ms: 1/2/5-style steps through ms, then the natural
 * time subdivisions (10/15/30 s and min, 1/2/3/6/12 h), then days/weeks.
 */
export const TIME_TICK_STEPS: readonly number[] = [
  1, 2, 5, 10, 20, 50, 100, 200, 500,
  1_000, 2_000, 5_000, 10_000, 15_000, 30_000,
  60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000,
  3_600_000, 7_200_000, 10_800_000, 21_600_000, 43_200_000,
  86_400_000, 172_800_000, 604_800_000,
];

/**
 * The smallest ladder step splitting `span` ms into at most `maxTicks`
 * intervals (the largest step is returned when even it is too fine).
 */
export function timeTickStep(span: number, maxTicks: number): number {
  const max = Math.max(1, maxTicks);
  for (const step of TIME_TICK_STEPS) {
    if (span / step <= max) return step;
  }
  return TIME_TICK_STEPS[TIME_TICK_STEPS.length - 1];
}

/**
 * Tick times within the view on the ladder step for `maxTicks`, aligned so
 * ticks land on round LOCAL times (pass the zone's UTC offset in ms —
 * `-new Date().getTimezoneOffset() * 60000` — so hour/day steps align to
 * local midnight; fixed-offset alignment, DST shifts are not chased).
 */
export function timeTicks(view: TimeView, maxTicks: number, tzOffsetMs = 0): number[] {
  const span = view.end - view.start;
  if (!Number.isFinite(span) || span <= 0) return [];
  const step = timeTickStep(span, maxTicks);
  const first = Math.ceil((view.start + tzOffsetMs) / step) * step - tzOffsetMs;
  const ticks: number[] = [];
  for (let t = first; t <= view.end; t += step) ticks.push(t);
  return ticks;
}

/** Civil date parts for a UTC-shifted timestamp (pure, Date-free). */
interface Civil {
  y: number;
  mo: number; // 1-12
  d: number; // 1-31
  h: number;
  mi: number;
  s: number;
  ms: number;
  dayMs: number; // ms since local midnight
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function civil(t: number, tzOffsetMs: number): Civil {
  const local = t + tzOffsetMs;
  const dayMs = ((local % 86_400_000) + 86_400_000) % 86_400_000;
  const days = Math.floor(local / 86_400_000);
  // Howard Hinnant's civil_from_days.
  const z = days + 719_468;
  const era = Math.floor(z / 146_097);
  const doe = z - era * 146_097;
  const yoe = Math.floor((doe - Math.floor(doe / 1_460) + Math.floor(doe / 36_524) - Math.floor(doe / 146_096)) / 365);
  const y = yoe + era * 400;
  const doy = doe - (365 * yoe + Math.floor(yoe / 4) - Math.floor(yoe / 100));
  const mp = Math.floor((5 * doy + 2) / 153);
  const d = doy - Math.floor((153 * mp + 2) / 5) + 1;
  const mo = mp < 10 ? mp + 3 : mp - 9;
  return {
    y: mo <= 2 ? y + 1 : y,
    mo,
    d,
    h: Math.floor(dayMs / 3_600_000),
    mi: Math.floor(dayMs / 60_000) % 60,
    s: Math.floor(dayMs / 1_000) % 60,
    ms: dayMs % 1_000,
    dayMs,
  };
}

function pad2(n: number): string {
  return n < 10 ? `0${n}` : String(n);
}

/**
 * Tick label with granularity matched to the step: sub-second steps show
 * `:SS.mmm`, second steps `HH:MM:SS`, minute/hour steps `HH:MM`, and day+
 * steps `Mon D`. A tick exactly at local midnight labels as the date (the
 * day boundary reads as a date, not '00:00'). Times are rendered in the
 * zone given by `tzOffsetMs` (see timeTicks).
 */
export function formatTimeTick(t: number, step: number, tzOffsetMs = 0): string {
  const c = civil(t, tzOffsetMs);
  if (step < 86_400_000 && c.dayMs === 0) return `${MONTHS[c.mo - 1]} ${c.d}`;
  if (step < 1_000) return `:${pad2(c.s)}.${String(c.ms).padStart(3, '0')}`;
  if (step < 60_000) return `${pad2(c.h)}:${pad2(c.mi)}:${pad2(c.s)}`;
  if (step < 86_400_000) return `${pad2(c.h)}:${pad2(c.mi)}`;
  return `${MONTHS[c.mo - 1]} ${c.d}`;
}

/** Full timestamp for tooltips/readouts: `Mon D HH:MM:SS` (+ `.mmm` when withMs). */
export function formatTimeFull(t: number, tzOffsetMs = 0, withMs = false): string {
  const c = civil(t, tzOffsetMs);
  const base = `${MONTHS[c.mo - 1]} ${c.d} ${pad2(c.h)}:${pad2(c.mi)}:${pad2(c.s)}`;
  return withMs ? `${base}.${String(c.ms).padStart(3, '0')}` : base;
}

/**
 * Compact human duration: '—' for non-finite/negative, then 0ms → '0ms',
 * sub-second → 'Nms', sub-minute → 'N.Ns', sub-hour → 'Nm NNs',
 * sub-day → 'Nh NNm', else 'Nd Nh'.
 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 1_000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1_000).toFixed(1)}s`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ${pad2(Math.round(ms / 1_000) % 60)}s`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ${pad2(Math.floor(ms / 60_000) % 60)}m`;
  return `${Math.floor(ms / 86_400_000)}d ${Math.floor(ms / 3_600_000) % 24}h`;
}

// -- Sub-track packing --------------------------------------------------------------

/** Effective minimum interval footprint used by packing, so coincident zero-length intervals stack. */
export const PACK_MIN_MS = 1;

/** The slice of an interval that packing needs. */
export interface PackItem {
  id: string;
  start: number;
  /** null/undefined = ongoing (blocks its track forever). */
  end?: number | null;
}

/**
 * Greedy first-fit interval packing for one lane: returns `tracks[i]` = the
 * sub-track (row within the lane) for items[i], plus the track count.
 *
 * Deterministic and stable under re-sorting: items are ordered by (start,
 * id) internally, so the same SET of intervals packs identically no matter
 * the input order, and results are index-aligned with the input. An
 * interval reuses the lowest track whose last occupant ended at or before
 * its start; ongoing intervals (end == null) block their track forever.
 * Every interval occupies at least PACK_MIN_MS, so coincident instants (and
 * an instant at a bar's start) get their own track instead of vanishing.
 */
export function packTracks(items: readonly PackItem[]): { tracks: number[]; trackCount: number } {
  const order = items.map((_, i) => i);
  order.sort((a, b) => {
    const ia = items[a];
    const ib = items[b];
    return ia.start - ib.start || (ia.id < ib.id ? -1 : ia.id > ib.id ? 1 : 0);
  });
  const tracks = new Array<number>(items.length).fill(0);
  const trackEnds: number[] = [];
  for (const i of order) {
    const it = items[i];
    const end = Math.max(it.end == null ? Infinity : it.end, it.start + PACK_MIN_MS);
    let t = 0;
    while (t < trackEnds.length && trackEnds[t] > it.start) t++;
    trackEnds[t] = end;
    tracks[i] = t;
  }
  return { tracks, trackCount: Math.max(1, trackEnds.length) };
}

/** Effective packing footprint end: ongoing blocks forever, instants occupy PACK_MIN_MS. */
function packEnd(it: PackItem): number {
  return Math.max(it.end == null ? Infinity : it.end, it.start + PACK_MIN_MS);
}

/**
 * packTracks over only the items that intersect `view` (a partially
 * visible interval counts; an ongoing one — end null — intersects every
 * window at/after its start). Same deterministic (start, id) ordering and
 * first-fit reuse as packTracks, evaluated over the visible subset only —
 * so one historical parallelism burst stops padding its lane the moment it
 * scrolls out of view. Assignment is a pure function of the visible SET:
 * while the window slides over unchanged overlap, nothing hops tracks.
 * Items outside the view get track -1 (callers keep or cull them);
 * trackCount is >= 1, so a lane with nothing visible collapses to one
 * track.
 */
export function packVisibleTracks(items: readonly PackItem[], view: TimeView): { tracks: number[]; trackCount: number } {
  const order: number[] = [];
  for (let i = 0; i < items.length; i++) {
    const it = items[i];
    if (it.start <= view.end && packEnd(it) >= view.start) order.push(i);
  }
  order.sort((a, b) => {
    const ia = items[a];
    const ib = items[b];
    return ia.start - ib.start || (ia.id < ib.id ? -1 : ia.id > ib.id ? 1 : 0);
  });
  const tracks = new Array<number>(items.length).fill(-1);
  const trackEnds: number[] = [];
  for (const i of order) {
    const it = items[i];
    let t = 0;
    while (t < trackEnds.length && trackEnds[t] > it.start) t++;
    trackEnds[t] = packEnd(it);
    tracks[i] = t;
  }
  return { tracks, trackCount: Math.max(1, trackEnds.length) };
}

// -- Lane layout --------------------------------------------------------------------

/** Vertical metrics for lane layout (CSS px). */
export interface LaneMetrics {
  /** Height of one sub-track's bar row. */
  trackHeight: number;
  /** Vertical gap between sub-tracks within a lane. */
  trackGap: number;
  /** Padding above the first and below the last track of each lane. */
  lanePad: number;
}

/** Computed vertical extents of each lane, in stacked order. */
export interface LaneLayout {
  /** Top y of each lane (starting at 0; add the axis offset / scroll externally). */
  tops: number[];
  /** Height of each lane. */
  heights: number[];
  /** Sum of all lane heights. */
  totalHeight: number;
}

/** Stack lanes vertically: lane height grows with its packed track count. */
export function layoutLanes(trackCounts: readonly number[], m: LaneMetrics): LaneLayout {
  const tops: number[] = [];
  const heights: number[] = [];
  let y = 0;
  for (const raw of trackCounts) {
    const n = Math.max(1, raw);
    const h = m.lanePad * 2 + n * m.trackHeight + (n - 1) * m.trackGap;
    tops.push(y);
    heights.push(h);
    y += h;
  }
  return { tops, heights, totalHeight: y };
}

/** y offset of a sub-track's top within its lane. */
export function trackTop(track: number, m: LaneMetrics): number {
  return m.lanePad + track * (m.trackHeight + m.trackGap);
}

// -- Labels / instants ----------------------------------------------------------------

/** Ellipsis used by fitText. */
export const ELLIPSIS = '…';

/**
 * Fit `text` into `availPx` given a (monospace) character width: returns the
 * text unchanged when it fits, an `abc…` truncation when at least `minChars`
 * characters + the ellipsis fit, else '' (suppress the label entirely —
 * never let it spill into a neighboring bar).
 */
export function fitText(text: string, availPx: number, charW: number, minChars = 2): string {
  if (!(charW > 0) || availPx <= 0 || text.length === 0) return '';
  const maxChars = Math.floor(availPx / charW);
  if (text.length <= maxChars) return text;
  if (maxChars - 1 < minChars) return '';
  return text.slice(0, maxChars - 1) + ELLIPSIS;
}

/** Below this rendered width (CSS px) an interval draws as an instant pip, not a bar. */
export const INSTANT_THRESHOLD_PX = 3;

/** True when a bar of `widthPx` should render as an instant pip/diamond. */
export function isInstantWidth(widthPx: number, threshold = INSTANT_THRESHOLD_PX): boolean {
  return widthPx < threshold;
}

/** Minimum rendered width (CSS px) for a real-duration bar — clamped up, never demoted to a pip. */
export const MIN_BAR_PX = 2;

/**
 * Rendered width of [startMs, endMs] mapped through the view's scale,
 * computed from the DURATION alone. This — not a difference of two rounded
 * screen coordinates — is what the bar-vs-pip decision must use: it is
 * exactly invariant under viewport translation, so an event's shape can
 * never flicker while the timeline scrolls (round(xEnd) - round(xStart)
 * oscillates ±1px as the bar's subpixel phase shifts).
 */
export function durationWidthPx(startMs: number, endMs: number, view: TimeView, plotWidth: number): number {
  const span = view.end - view.start;
  return span > 0 ? ((endMs - startMs) / span) * plotWidth : 0;
}

// -- Hit testing --------------------------------------------------------------------

/** An axis-aligned hit rectangle (CSS px). */
export interface HitRect {
  x: number;
  y: number;
  w: number;
  h: number;
}

/**
 * Widen a (possibly hairline) rect to at least `minW` px around its center —
 * instants get a hit target a few px larger than their visual so they stay
 * hoverable/clickable.
 */
export function expandHitRect(r: HitRect, minW: number): HitRect {
  if (r.w >= minW) return r;
  const cx = r.x + r.w / 2;
  return { x: cx - minW / 2, y: r.y, w: minW, h: r.h };
}

/**
 * Index of the TOPMOST (= last, matching paint order) rect containing the
 * point, or -1. Edges are inclusive.
 */
export function hitTestRects(x: number, y: number, rects: readonly HitRect[]): number {
  for (let i = rects.length - 1; i >= 0; i--) {
    const r = rects[i];
    if (x >= r.x && x <= r.x + r.w && y >= r.y && y <= r.y + r.h) return i;
  }
  return -1;
}

/** Squared distance from point p to segment ab. */
export function distSqToSegment(px: number, py: number, ax: number, ay: number, bx: number, by: number): number {
  const dx = bx - ax;
  const dy = by - ay;
  const len2 = dx * dx + dy * dy;
  let t = len2 > 0 ? ((px - ax) * dx + (py - ay) * dy) / len2 : 0;
  if (t < 0) t = 0;
  else if (t > 1) t = 1;
  const qx = ax + t * dx - px;
  const qy = ay + t * dy - py;
  return qx * qx + qy * qy;
}

/** True when the point is within `tol` px of the polyline. */
export function hitTestPolyline(px: number, py: number, pts: readonly { x: number; y: number }[], tol: number): boolean {
  const t2 = tol * tol;
  for (let i = 1; i < pts.length; i++) {
    if (distSqToSegment(px, py, pts[i - 1].x, pts[i - 1].y, pts[i].x, pts[i].y) <= t2) return true;
  }
  return false;
}

/**
 * Route for a connector from the right-center of `from` to the left-center
 * of `to`: a sampled cubic bezier with horizontal control handles, so the
 * line leaves the source rightward and enters the target leftward — a
 * gentle S-curve for forward targets, a readable loop-back for targets that
 * start earlier. Aligned same-row forward targets get a plain 2-point
 * segment. Returns `samples + 1` points (polyline: draw it, hit-test it
 * with hitTestPolyline).
 */
export function connectorRoute(from: HitRect, to: HitRect, samples = 24): { x: number; y: number }[] {
  const x0 = from.x + from.w;
  const y0 = from.y + from.h / 2;
  const x1 = to.x;
  const y1 = to.y + to.h / 2;
  if (Math.abs(y0 - y1) < 0.5 && x1 >= x0) {
    return [
      { x: x0, y: y0 },
      { x: x1, y: y1 },
    ];
  }
  const c = Math.min(90, Math.max(24, Math.abs(x1 - x0) * 0.5, Math.abs(y1 - y0) * 0.35));
  const pts: { x: number; y: number }[] = [];
  const n = Math.max(2, Math.floor(samples));
  for (let i = 0; i <= n; i++) {
    const t = i / n;
    const u = 1 - t;
    pts.push({
      x: u * u * u * x0 + 3 * u * u * t * (x0 + c) + 3 * u * t * t * (x1 - c) + t * t * t * x1,
      y: u * u * u * y0 + 3 * u * u * t * y0 + 3 * u * t * t * y1 + t * t * t * y1,
    });
  }
  return pts;
}

// -- Category color -----------------------------------------------------------------

/** FNV-1a 32-bit hash (stable across sessions/platforms). */
export function hashString(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/**
 * Stable category → hue in [0, 360): FNV-1a scattered by the golden-ratio
 * conjugate, so similar strings land far apart and hues spread uniformly.
 * Same string = same hue, forever.
 */
export function categoryHue(category: string): number {
  const g = (hashString(category) * 0.61803398875) % 1;
  return Math.floor(g * 360);
}

/**
 * Deterministic per-category lightness/chroma offsets (|dl| <= 0.05,
 * |dc| <= 0.02), derived from independent hash bits. A second visual
 * discriminator: two categories that happen to hash to nearby hues still
 * separate by tone, while every category keeps one stable color forever.
 */
export function categoryJitter(category: string): { dl: number; dc: number } {
  const h = hashString(`${category} tone`);
  return {
    dl: ((h & 0xff) / 255 - 0.5) * 0.1,
    dc: (((h >>> 8) & 0xff) / 255 - 0.5) * 0.04,
  };
}

/** Options for categoryColor. */
export interface CategoryColorOptions {
  /** 'oklch' (perceptually even lightness — preferred) or 'hsl' fallback. */
  mode?: 'oklch' | 'hsl';
  /** oklch lightness 0..1 (default 0.62 — readable chips on a dark bg). */
  lightness?: number;
  /** oklch chroma (default 0.11 — saturated but not neon). */
  chroma?: number;
  /** Alpha 0..1 (default 1). */
  alpha?: number;
}

/**
 * CSS color for a category hue. oklch keeps perceived lightness even across
 * hues (label text stays readable on every category); the hsl fallback
 * approximates it for engines without oklch support.
 */
export function categoryColor(hue: number, opts: CategoryColorOptions = {}): string {
  const l = opts.lightness ?? 0.62;
  const c = opts.chroma ?? 0.11;
  const a = opts.alpha ?? 1;
  if (opts.mode === 'hsl') {
    const s = Math.round(Math.min(1, c / 0.32) * 100);
    const ll = Math.round(l * 88);
    return a >= 1 ? `hsl(${hue}, ${s}%, ${ll}%)` : `hsla(${hue}, ${s}%, ${ll}%, ${round3(a)})`;
  }
  return a >= 1 ? `oklch(${round3(l)} ${round3(c)} ${hue})` : `oklch(${round3(l)} ${round3(c)} ${hue} / ${round3(a)})`;
}

function round3(n: number): number {
  return Math.round(n * 1000) / 1000;
}

// -- Style map ----------------------------------------------------------------------

/** Fill pattern for an interval state / segment kind. */
export type StylePattern = 'solid' | 'hatch' | 'stipple' | 'outline';

/** Rendering treatment for one interval `state` or segment `kind`. */
export interface IntervalStyle {
  pattern?: StylePattern;
  /** Multiplies the fill alpha (default 1). */
  alphaScale?: number;
  /** Multiplies the category chroma/saturation (default 1). */
  saturationScale?: number;
  /** Multiplies the category lightness (default 1). */
  lightnessScale?: number;
  /** Border treatment; `emphasis: true` uses the theme emphasis color. */
  border?: { width?: number; dash?: number[]; emphasis?: boolean };
  /** Corner glyph: 'bang' is the unmissable failure mark. */
  glyph?: 'none' | 'bang' | 'dot';
}

/** Named style map: interval `state` / segment `kind` → treatment. */
export type StyleMap = Record<string, IntervalStyle>;

/**
 * Built-in treatments (consumer keys spread on top via the element's
 * `styles` property): '' solid; 'emphasis'/'failed' unmissable — thick
 * emphasis border + corner bang glyph + stipple, hue untouched;
 * 'dim'/'queued' desaturated + translucent; 'hatch'/'waiting' 45° stripes;
 * 'outline' hollow.
 */
export const DEFAULT_STYLES: StyleMap = {
  '': { pattern: 'solid' },
  emphasis: { pattern: 'stipple', border: { width: 2, emphasis: true }, glyph: 'bang' },
  failed: { pattern: 'stipple', border: { width: 2, emphasis: true }, glyph: 'bang' },
  dim: { pattern: 'solid', alphaScale: 0.4, saturationScale: 0.45, lightnessScale: 0.85 },
  queued: { pattern: 'solid', alphaScale: 0.4, saturationScale: 0.45, lightnessScale: 0.85 },
  hatch: { pattern: 'hatch', alphaScale: 0.85 },
  waiting: { pattern: 'hatch', alphaScale: 0.85 },
  outline: { pattern: 'outline' },
};

// -- Coverage / async history ---------------------------------------------------------

/** A half-open-ish time range [start, end] used by coverage bookkeeping. */
export interface TimeRange {
  start: number;
  end: number;
}

/** Merge overlapping/touching ranges into a sorted disjoint list (new array). */
export function mergeRanges(ranges: readonly TimeRange[]): TimeRange[] {
  const sorted = ranges
    .filter((r) => r.end > r.start)
    .slice()
    .sort((a, b) => a.start - b.start);
  const out: TimeRange[] = [];
  for (const r of sorted) {
    const last = out[out.length - 1];
    if (last && r.start <= last.end) {
      if (r.end > last.end) last.end = r.end;
    } else {
      out.push({ start: r.start, end: r.end });
    }
  }
  return out;
}

/** Subtract a sorted disjoint cover list from `span`, returning the gaps. */
export function subtractRanges(span: TimeRange, covers: readonly TimeRange[]): TimeRange[] {
  const out: TimeRange[] = [];
  let cursor = span.start;
  for (const c of covers) {
    if (c.end <= cursor) continue;
    if (c.start >= span.end) break;
    if (c.start > cursor) out.push({ start: cursor, end: Math.min(c.start, span.end) });
    cursor = Math.max(cursor, c.end);
    if (cursor >= span.end) break;
  }
  if (cursor < span.end) out.push({ start: cursor, end: span.end });
  return out;
}

/**
 * The range a consumer may ask `loadRange` about for this viewport: the
 * visible window plus a little backward prefetch, clamped to `now` AND to
 * the covered end. The covered-end clamp is load-bearing: the region
 * between the last covered time and "now" belongs to the consumer's LIVE
 * data feed (setData/mergeData `coverage`), and in follow mode "now"
 * advances every frame — if loadRange could be asked for that forward
 * sliver, a fresh gap would reopen the moment each request settled and the
 * loader would refire serially at ~one request per round-trip (~30/s),
 * forever. loadRange exists for BACKWARD history only. With no coverage at
 * all the probe may still reach `now` (the bootstrap load), which latches
 * once it settles. Returns null when nothing is requestable.
 */
export function historyProbe(view: TimeView, now: number, coveredEnd: number | null, prefetchFrac = 0.15): TimeRange | null {
  const span = view.end - view.start;
  let end = Math.min(view.end, now);
  if (coveredEnd !== null && coveredEnd < end) end = coveredEnd;
  const start = view.start - span * prefetchFrac;
  return end > start ? { start, end } : null;
}

/** Options for CoverageTracker. */
export interface CoverageOptions {
  /** Never request less than this much history at once (default 60 s). */
  minChunkMs?: number;
  /** First retry delay after a failed load (default 2 s). */
  backoffMs?: number;
  /** Retry delay cap (default 60 s). */
  maxBackoffMs?: number;
}

/**
 * Bookkeeping for `loadRange`-style async history loading. Tracks which
 * time ranges are covered by data the consumer has supplied, which request
 * is in flight (one at a time — no request storms), the exhausted-history
 * boundary, and a rejection backoff.
 *
 *   const next = tracker.nextRequest(view, now); // range to fetch, or null
 *   ...call loadRange(next)...                    // tracker marked it in flight
 *   tracker.settle(next, { ok: true });           // → covered
 *   tracker.settle(next, { ok: true, exhausted: true }); // → history ends here
 *   tracker.settle(next, { ok: false });          // → retried after backoff
 */
export class CoverageTracker {
  private covered: TimeRange[] = [];
  private inflight: TimeRange | null = null;
  private minChunk: number;
  private backoff0: number;
  private backoffMax: number;
  private backoff: number;
  private retryAt = -Infinity;
  /** Time before which history is known exhausted (null = unknown). */
  exhaustedBefore: number | null = null;

  constructor(opts: CoverageOptions = {}) {
    this.minChunk = opts.minChunkMs ?? 60_000;
    this.backoff0 = opts.backoffMs ?? 2_000;
    this.backoffMax = opts.maxBackoffMs ?? 60_000;
    this.backoff = this.backoff0;
  }

  /** Mark [start, end] as covered by consumer-supplied data. */
  addCovered(start: number, end: number): void {
    if (!(end > start)) return;
    this.covered = mergeRanges([...this.covered, { start, end }]);
  }

  /** Sorted disjoint covered ranges (live reference — do not mutate). */
  coveredRanges(): readonly TimeRange[] {
    return this.covered;
  }

  /** End of the newest covered range (null while nothing is covered). */
  coveredEnd(): number | null {
    const last = this.covered[this.covered.length - 1];
    return last ? last.end : null;
  }

  /** The in-flight request, if any. */
  pending(): TimeRange | null {
    return this.inflight;
  }

  /**
   * Uncovered gaps within `span` that could still hold data (gaps entirely
   * before the exhausted boundary are dropped; a gap straddling it is
   * clipped). Use for painting the loading / uncovered affordance.
   */
  uncoveredIn(span: TimeRange): TimeRange[] {
    let gaps = subtractRanges(span, this.covered);
    const ex = this.exhaustedBefore;
    if (ex != null) {
      gaps = gaps.filter((g) => g.end > ex).map((g) => (g.start < ex ? { start: ex, end: g.end } : g));
    }
    return gaps;
  }

  /**
   * The next range to fetch for the given viewport, or null (fully covered,
   * a request is already in flight, backing off after a failure, or history
   * is exhausted). The returned range is marked in flight — pass it to
   * settle() when the load resolves or rejects. Requests are widened to
   * minChunkMs (extending into the past) so tiny scroll steps don't spray
   * tiny requests.
   */
  nextRequest(view: TimeView, now: number): TimeRange | null {
    if (this.inflight || now < this.retryAt) return null;
    const gaps = this.uncoveredIn({ start: view.start, end: view.end });
    if (gaps.length === 0) return null;
    const gap = gaps[gaps.length - 1]; // newest gap first — fill toward the past
    const req: TimeRange = { start: gap.start, end: gap.end };
    if (req.end - req.start < this.minChunk) req.start = req.end - this.minChunk;
    const ex = this.exhaustedBefore;
    if (ex != null && req.start < ex) req.start = ex;
    if (!(req.end > req.start)) return null;
    this.inflight = req;
    return req;
  }

  /** Resolve/reject the in-flight request (no-op for a stale range). */
  settle(range: TimeRange, result: { ok: boolean; exhausted?: boolean }, now = 0): void {
    if (!this.inflight || this.inflight.start !== range.start || this.inflight.end !== range.end) return;
    this.inflight = null;
    if (!result.ok) {
      this.retryAt = now + this.backoff;
      this.backoff = Math.min(this.backoff * 2, this.backoffMax);
      return;
    }
    this.backoff = this.backoff0;
    this.retryAt = -Infinity;
    this.addCovered(range.start, range.end);
    if (result.exhausted) {
      const first = this.covered[0];
      this.exhaustedBefore = first ? first.start : range.start;
    }
  }
}

// -- Render pacing ---------------------------------------------------------------------

/** Render tiers: full rate while interacting, throttled idle, cheaper still on battery. */
export type RenderTier = 'interactive' | 'idle' | 'idle-battery';

/** Idle frame budget: ~30fps while nothing is being interacted with. */
export const IDLE_FRAME_MS = 1000 / 30;
/** Idle-on-battery frame budget: ~10fps. */
export const IDLE_BATTERY_FRAME_MS = 100;
/** Full-rate grace window after the last input — interaction never feels throttled. */
export const INTERACT_GRACE_MS = 500;

/** ms-per-frame budget for a tier (0 = render every rAF tick). */
export function frameBudgetMs(tier: RenderTier): number {
  if (tier === 'idle') return IDLE_FRAME_MS;
  if (tier === 'idle-battery') return IDLE_BATTERY_FRAME_MS;
  return 0;
}

/**
 * Frame gate for a rAF loop: render when the tier's budget has elapsed
 * since the last RENDERED frame. The half-tick slack keeps a 33.3ms budget
 * from aliasing down (a 60Hz display ticks at 16.7ms — without slack,
 * 33.3ms would round up to every 3rd tick = 20fps instead of 30).
 */
export function shouldRender(nowTs: number, lastRenderTs: number, budgetMs: number, rafIntervalMs = 16.7): boolean {
  if (budgetMs <= 0) return true;
  return nowTs - lastRenderTs >= budgetMs - rafIntervalMs / 2;
}
