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
export declare function toMs(t: number | Date): number;
/** A visible time window [start, end] in ms since epoch. */
export interface TimeView {
    start: number;
    end: number;
}
/** Hard zoom clamps: ~2 s to ~7 days of visible span. */
export declare const MIN_SPAN_MS = 2000;
export declare const MAX_SPAN_MS: number;
/** Time → x in [0, width] for the view (un-clamped; callers cull). */
export declare function timeToX(t: number, view: TimeView, width: number): number;
/** x → time for the view (inverse of timeToX). */
export declare function xToTime(x: number, view: TimeView, width: number): number;
/** Shift the view by dt ms (positive = later). */
export declare function panView(view: TimeView, dt: number): TimeView;
/**
 * The hard right end stop for user-driven views: the right edge never
 * passes `now` (span preserved; views already at/before now come back
 * unchanged). Every user input path (wheel, drag, pinch, keyboard,
 * setViewport) clamps through this, so panning/zooming toward the future
 * reliably parks EXACTLY at the stop — which is what makes the
 * FOLLOW_SNAP_DEVICE_PX follow re-engage trivially hittable.
 */
export declare function clampViewToNow(view: TimeView, now: number): TimeView;
/**
 * Zoom the view by `factor` (> 1 zooms in) keeping `anchor` at the same
 * on-screen fraction — the time under the cursor stays under the cursor.
 * The span is clamped to [minSpan, maxSpan]; clamping preserves the anchor
 * fraction, so the invariant holds even at the clamp.
 */
export declare function zoomView(view: TimeView, anchor: number, factor: number, minSpan?: number, maxSpan?: number): TimeView;
/**
 * Normalize a WheelEvent delta to pixels. deltaMode 0 (pixel) passes
 * through 1:1; 1 (line) and 2 (page) — discrete wheels — convert via the
 * given heights. Non-finite deltas normalize to 0.
 */
export declare function wheelDeltaToPixels(delta: number, deltaMode: number, lineHeight?: number, pageHeight?: number): number;
/** Pixels of zoom wheel per doubling of the scale. */
export declare const ZOOM_PX_PER_DOUBLE = 260;
/**
 * Continuous exponential zoom factor for a wheel delta in pixels: negative
 * (scroll up / pinch out) zooms in. ZOOM_PX_PER_DOUBLE px doubles the scale,
 * so factors compose exactly: f(a) * f(b) === f(a + b).
 */
export declare function zoomFactorForWheel(deltaPx: number): number;
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
    /**
     * False = the chart takes NOTHING from this event — the caller must not
     * preventDefault, so the page scrolls normally over the chart. True the
     * moment any axis routes somewhere (preventDefault the whole event; a
     * diagonal gesture's unconsumed axis is dropped, never half-forwarded).
     */
    consumed: boolean;
}
/**
 * Route a wheel/trackpad gesture: ctrl/meta+wheel zooms (always consumed —
 * a pinch stream must never leak browser page-zoom, even on a zero-delta
 * tick); shift+wheel pans time (a vertical wheel pans horizontally);
 * otherwise deltaX pans time and deltaY scrolls the lane stack ONLY when
 * it overflows the host. A plain vertical wheel over a non-overflowing
 * chart is NOT consumed — vertical scrolling must never scroll the chart
 * sideways, and the page keeps scrolling normally across the chart. A
 * diagonal two-finger gesture applies each axis to its own behavior and is
 * consumed iff at least one axis routed.
 */
export declare function routeWheel(e: WheelInput, lanesOverflow: boolean): WheelRoute;
/** Fraction of the span "now" sits in from the right edge while following. */
export declare const FOLLOW_LEAD_FRAC = 0.02;
/**
 * A gesture ending with the right edge within this many DEVICE pixels
 * (literal screen pixels — at dpr 2 this is 1 CSS px) of the `now` end
 * stop re-engages follow.
 */
export declare const FOLLOW_SNAP_DEVICE_PX = 2;
/**
 * Whether follow-now is engaged after a user-driven viewport change.
 *
 * A PURE PAN that moves the right edge backward (into the past) always
 * disengages. This is load-bearing for trackpads: a two-finger pan arrives
 * as many small wheel events, and an unconditional magnetic rule re-pinned
 * the view after every event smaller than the snap zone — making it
 * impossible to leave "now" by scrolling. While ALREADY following, any
 * other gesture stays pinned — zooming at the live edge keeps following
 * even though an anchored zoom nudges the raw end backward. While NOT
 * following, a gesture re-engages only when the right edge lands within
 * FOLLOW_SNAP_DEVICE_PX DEVICE pixels of the `now` end stop — pass the
 * view's ms-per-DEVICE-pixel scale (span / (plotWidthCss * dpr)). The
 * zone is deliberately tiny (the old span-fraction zone re-docked views
 * that merely got NEAR the edge): user views hard-stop at now
 * (clampViewToNow), so a forward drag parks exactly at the stop and
 * reliably re-docks, while a view parked 3+ device px short stays put.
 */
export declare function followAfterGesture(wasFollowing: boolean, prevEnd: number, next: TimeView, now: number, isPan: boolean, msPerDevicePx: number): boolean;
/**
 * Duration of the follow-lead ease (ms): engaging follow ramps the lead in
 * from where the gesture parked, and a backward-pan disengage glides any
 * residual lead back out — both over this window, instead of teleporting
 * the view by span * FOLLOW_LEAD_FRAC in a single frame (~2% of the plot
 * width — 50+ device px on a wide monitor).
 */
export declare const FOLLOW_LEAD_TWEEN_MS = 200;
/** Duration of the jump-to-now glide (ms): fast, deliberate — but continuous. */
export declare const JUMP_TO_NOW_TWEEN_MS = 250;
/**
 * The eased follow lead `elapsedMs` into a glide from `fromFrac` toward
 * `targetFrac` over `tweenMs`. Leads are FRACTIONS of the span (like
 * FOLLOW_LEAD_FRAC) — dimensionless, so zooming mid-glide rescales the
 * lead with the span exactly like the steady-state lead does. easeOutQuad
 * (the LAYOUT_TWEEN family): monotone from → target with no overshoot,
 * the per-tick step is bounded by |target - from| * 2 * dt / tweenMs (the
 * no-teleport guarantee — the ease's steepest slope is at t=0), and it
 * lands EXACTLY on the target at elapsed >= tweenMs (no asymptote). A
 * non-positive tweenMs snaps straight to the target — the
 * prefers-reduced-motion path.
 */
export declare function followLeadAt(fromFrac: number, targetFrac: number, elapsedMs: number, tweenMs: number): number;
/**
 * The lead fraction a user gesture legitimately holds: its own end
 * relative to `now`, capped at `maxFrac` — the lead the view was already
 * allowed (a gesture may consume lead or park behind now, never mint
 * lead). ENGAGE seeds the ease-in from this (a gesture parked at/just
 * short of the now stop seeds ≈ 0; a jump-to-now from deep in the past
 * seeds very negative — the glide crosses the gap); DISENGAGE floors it
 * at 0 for the residual that glides back out.
 */
export declare function gestureLeadFrac(endMs: number, now: number, span: number, maxFrac: number): number;
/**
 * Default ms without fresh data before a live chart declares its feed
 * STALE (the element's `staleAfterMs`). Tune to ~2 poll intervals of the
 * consumer's live feed; a non-finite or non-positive value disables
 * staleness entirely (static, never-fed datasets).
 */
export declare const STALE_AFTER_DEFAULT_MS = 10000;
/**
 * Whether the live feed is stale: fresh data last arrived at `lastFresh`
 * (null = no data has EVER arrived — an empty chart is never stale) and
 * more than `staleAfterMs` has since passed. The guard against a chart
 * misrepresenting state when its feed silently dies: a finished run whose
 * end never arrived would otherwise render as "running" forever.
 */
export declare function feedIsStale(now: number, lastFresh: number | null, staleAfterMs: number): boolean;
/**
 * The LIVE EDGE every live semantic advances to — ongoing (end = null)
 * bar ends, the now line, the follow-mode pin, and the user-view forward
 * clamp: real `now` while the feed is fresh, FROZEN at `lastFresh` once
 * stale. Once stale the chart never extrapolates past the last timestamp
 * the data actually vouched for — frozen bars can only be honest. (The
 * element eases the transition between the two targets with followLeadAt;
 * this is the steady-state value.)
 */
export declare function liveEdgeTarget(now: number, lastFresh: number | null, staleAfterMs: number): number;
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
export declare function snapViewToDevicePixels(view: TimeView, plotWidthCss: number, dpr: number): TimeView;
/**
 * The tick ladder, in ms: 1/2/5-style steps through ms, then the natural
 * time subdivisions (10/15/30 s and min, 1/2/3/6/12 h), then days/weeks.
 */
export declare const TIME_TICK_STEPS: readonly number[];
/**
 * The smallest ladder step splitting `span` ms into at most `maxTicks`
 * intervals (the largest step is returned when even it is too fine).
 */
export declare function timeTickStep(span: number, maxTicks: number): number;
/**
 * Tick times within the view on the ladder step for `maxTicks`, aligned so
 * ticks land on round LOCAL times (pass the zone's UTC offset in ms —
 * `-new Date().getTimezoneOffset() * 60000` — so hour/day steps align to
 * local midnight; fixed-offset alignment, DST shifts are not chased).
 */
export declare function timeTicks(view: TimeView, maxTicks: number, tzOffsetMs?: number): number[];
/**
 * Tick label with granularity matched to the step: sub-second steps show
 * `:SS.mmm`, second steps `HH:MM:SS`, minute/hour steps `HH:MM`, and day+
 * steps `Mon D`. A tick exactly at local midnight labels as the date (the
 * day boundary reads as a date, not '00:00'). Times are rendered in the
 * zone given by `tzOffsetMs` (see timeTicks).
 */
export declare function formatTimeTick(t: number, step: number, tzOffsetMs?: number): string;
/** Full timestamp for tooltips/readouts: `Mon D HH:MM:SS` (+ `.mmm` when withMs). */
export declare function formatTimeFull(t: number, tzOffsetMs?: number, withMs?: boolean): string;
/**
 * Compact human duration: '—' for non-finite/negative, then 0ms → '0ms',
 * sub-second → 'Nms', sub-minute → 'N.Ns', sub-hour → 'Nm NNs',
 * sub-day → 'Nh NNm', else 'Nd Nh'.
 */
export declare function formatDuration(ms: number): string;
/** Effective minimum interval footprint used by packing, so coincident zero-length intervals stack. */
export declare const PACK_MIN_MS = 1;
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
export declare function packTracks(items: readonly PackItem[]): {
    tracks: number[];
    trackCount: number;
};
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
export declare function packVisibleTracks(items: readonly PackItem[], view: TimeView): {
    tracks: number[];
    trackCount: number;
};
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
/**
 * Height of one lane given its track count, at `trackHeight` px per track
 * (defaults to the metrics' normal height). Fractional counts are allowed —
 * they drive the lane-height tween.
 */
export declare function laneHeight(trackCount: number, m: LaneMetrics, trackHeight?: number): number;
/**
 * Stack lanes vertically: lane height grows with its packed track count.
 * `trackHeights[i]`, when given, overrides the metrics' track height for
 * lane i — how auto-fit renders demoted lanes at the compact height (and
 * how height changes tween: fractional per-lane heights are fine).
 */
export declare function layoutLanes(trackCounts: readonly number[], m: LaneMetrics, trackHeights?: readonly number[]): LaneLayout;
/** y offset of a sub-track's top within its lane (per-lane `trackHeight` overrides the metrics'). */
export declare function trackTop(track: number, m: LaneMetrics, trackHeight?: number): number;
/**
 * Headroom hysteresis for auto-fit: a demoted lane only re-promotes when
 * the resulting layout would fit with this fraction of the available
 * height to spare, so heights can't flap when hovering at the boundary.
 */
export declare const FIT_HYSTERESIS_FRAC = 0.1;
/** Result of computeAutoFit. */
export interface FitResult {
    /** Per lane (input order): true = render ALL of that lane's tracks at the compact height. */
    demoted: boolean[];
    /**
     * Number of demoted lanes — the hysteresis state. Feed it back as
     * `prevDemotedCount` on the next evaluation.
     */
    count: number;
}
/**
 * The order lanes are demoted to compact in: by visible track count
 * DESCENDING (the tallest / most parallel lane first — one compact tall
 * lane recovers the most height), ties broken by LATER display order
 * first — so when two lanes are equally tall, the one further down the
 * chart demotes first and top-of-chart lanes keep their detail longest.
 * Returns lane indices, first-to-demote first. Deterministic for a given
 * count list.
 */
export declare function demotionOrder(trackCounts: readonly number[]): number[];
/**
 * Auto-fit: decide which lanes render at the compact track height so the
 * lane stack fits `availHeight` (the host's plot height). Evaluate with
 * the NATURAL layout (every lane at the normal track height); while it
 * overflows, demote lanes one at a time in demotionOrder() until the
 * total fits or every lane is compact (if all-compact still overflows,
 * the caller's vertical lane scrolling takes over). Demotion applies to a
 * whole lane — all of its tracks go compact together.
 *
 * Deterministic and oscillation-free: the result is a pure function of
 * (track counts, metrics, heights, prevDemotedCount). Demotion reacts
 * immediately (an overflowing layout never persists), but promotion is
 * hysteretic — a demoted lane is only promoted when the layout stays
 * fitting with `hysteresisFrac` headroom (so all lanes re-promote only
 * once the natural layout fits within availHeight * (1 - hysteresisFrac)).
 * Between the two thresholds the previous demotion COUNT is kept; the
 * demotion SET is always re-derived from the CURRENT counts, so a lane
 * whose parallelism left the window hands its demotion to the now-tallest
 * lane deterministically. `compactTrackHeight` is clamped to at most the
 * normal track height.
 */
export declare function computeAutoFit(trackCounts: readonly number[], m: LaneMetrics, compactTrackHeight: number, availHeight: number, prevDemotedCount?: number, hysteresisFrac?: number): FitResult;
/** Ellipsis used by fitText. */
export declare const ELLIPSIS = "\u2026";
/**
 * Fit `text` into `availPx` given a (monospace) character width: returns the
 * text unchanged when it fits, an `abc…` truncation when at least `minChars`
 * characters + the ellipsis fit, else '' (suppress the label entirely —
 * never let it spill into a neighboring bar).
 */
export declare function fitText(text: string, availPx: number, charW: number, minChars?: number): string;
/** Below this rendered width (CSS px) an interval draws as an instant pip, not a bar. */
export declare const INSTANT_THRESHOLD_PX = 3;
/** True when a bar of `widthPx` should render as an instant pip/diamond. */
export declare function isInstantWidth(widthPx: number, threshold?: number): boolean;
/** Minimum rendered width (CSS px) for a real-duration bar — clamped up, never demoted to a pip. */
export declare const MIN_BAR_PX = 2;
/**
 * Rendered width of [startMs, endMs] mapped through the view's scale,
 * computed from the DURATION alone. This — not a difference of two rounded
 * screen coordinates — is what the bar-vs-pip decision must use: it is
 * exactly invariant under viewport translation, so an event's shape can
 * never flicker while the timeline scrolls (round(xEnd) - round(xStart)
 * oscillates ±1px as the bar's subpixel phase shifts).
 */
export declare function durationWidthPx(startMs: number, endMs: number, view: TimeView, plotWidth: number): number;
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
export declare function expandHitRect(r: HitRect, minW: number): HitRect;
/**
 * Index of the TOPMOST (= last, matching paint order) rect containing the
 * point, or -1. Edges are inclusive.
 */
export declare function hitTestRects(x: number, y: number, rects: readonly HitRect[]): number;
/** Squared distance from point p to segment ab. */
export declare function distSqToSegment(px: number, py: number, ax: number, ay: number, bx: number, by: number): number;
/** True when the point is within `tol` px of the polyline. */
export declare function hitTestPolyline(px: number, py: number, pts: readonly {
    x: number;
    y: number;
}[], tol: number): boolean;
/**
 * Route for a connector from the right-center of `from` to the left-center
 * of `to`: a sampled cubic bezier with horizontal control handles, so the
 * line leaves the source rightward and enters the target leftward — a
 * gentle S-curve for forward targets, a readable loop-back for targets that
 * start earlier. Aligned same-row forward targets get a plain 2-point
 * segment. Returns `samples + 1` points (polyline: draw it, hit-test it
 * with hitTestPolyline).
 */
export declare function connectorRoute(from: HitRect, to: HitRect, samples?: number): {
    x: number;
    y: number;
}[];
/** FNV-1a 32-bit hash (stable across sessions/platforms). */
export declare function hashString(s: string): number;
/**
 * Stable category → hue in [0, 360): FNV-1a scattered by the golden-ratio
 * conjugate, so similar strings land far apart and hues spread uniformly.
 * Same string = same hue, forever.
 */
export declare function categoryHue(category: string): number;
/**
 * Deterministic per-category lightness/chroma offsets (|dl| <= 0.05,
 * |dc| <= 0.02), derived from independent hash bits. A second visual
 * discriminator: two categories that happen to hash to nearby hues still
 * separate by tone, while every category keeps one stable color forever.
 */
export declare function categoryJitter(category: string): {
    dl: number;
    dc: number;
};
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
export declare function categoryColor(hue: number, opts?: CategoryColorOptions): string;
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
    border?: {
        width?: number;
        dash?: number[];
        emphasis?: boolean;
    };
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
export declare const DEFAULT_STYLES: StyleMap;
/** A half-open-ish time range [start, end] used by coverage bookkeeping. */
export interface TimeRange {
    start: number;
    end: number;
}
/** Merge overlapping/touching ranges into a sorted disjoint list (new array). */
export declare function mergeRanges(ranges: readonly TimeRange[]): TimeRange[];
/** Subtract a sorted disjoint cover list from `span`, returning the gaps. */
export declare function subtractRanges(span: TimeRange, covers: readonly TimeRange[]): TimeRange[];
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
export declare function historyProbe(view: TimeView, now: number, coveredEnd: number | null, prefetchFrac?: number): TimeRange | null;
/** Options for CoverageTracker. */
export interface CoverageOptions {
    /** Never request less than this much history at once (default 60 s). */
    minChunkMs?: number;
    /** Fixed delay between retries of a failed load (default 2 s). */
    retryMs?: number;
}
/**
 * Bookkeeping for `loadRange`-style async history loading. Tracks which
 * time ranges are covered by data the consumer has supplied, which request
 * is in flight (one at a time — no request storms), the exhausted-history
 * boundary, and the fixed retry cadence for rejected loads.
 *
 *   const next = tracker.nextRequest(view, now); // range to fetch, or null
 *   ...call loadRange(next)...                    // tracker marked it in flight
 *   tracker.settle(next, { ok: true });           // → covered
 *   tracker.settle(next, { ok: true, exhausted: true }); // → history ends here
 *   tracker.settle(next, { ok: false });          // → retried ~retryMs later, forever
 */
export declare class CoverageTracker {
    private covered;
    private inflight;
    private minChunk;
    private retryEvery;
    private retryAt;
    /** Time before which history is known exhausted (null = unknown). */
    exhaustedBefore: number | null;
    constructor(opts?: CoverageOptions);
    /** Mark [start, end] as covered by consumer-supplied data. */
    addCovered(start: number, end: number): void;
    /** Sorted disjoint covered ranges (live reference — do not mutate). */
    coveredRanges(): readonly TimeRange[];
    /** End of the newest covered range (null while nothing is covered). */
    coveredEnd(): number | null;
    /** The in-flight request, if any. */
    pending(): TimeRange | null;
    /**
     * True while a failed load is waiting out the fixed retry cadence
     * (nothing in flight, next attempt scheduled). Callers driving requests
     * from a frame loop must keep the loop alive while this is true, or the
     * retry parks until the next unrelated wakeup.
     */
    waitingRetry(now: number): boolean;
    /**
     * Uncovered gaps within `span` that could still hold data (gaps entirely
     * before the exhausted boundary are dropped; a gap straddling it is
     * clipped). Use for painting the loading / uncovered affordance.
     */
    uncoveredIn(span: TimeRange): TimeRange[];
    /**
     * The next range to fetch for the given viewport, or null (fully covered,
     * a request is already in flight, waiting out the retry cadence after a
     * failure, or history is exhausted). The returned range is marked in
     * flight — pass it to settle() when the load resolves or rejects.
     * Requests are widened to minChunkMs (extending into the past) so tiny
     * scroll steps don't spray tiny requests.
     */
    nextRequest(view: TimeView, now: number): TimeRange | null;
    /** Resolve/reject the in-flight request (no-op for a stale range). */
    settle(range: TimeRange, result: {
        ok: boolean;
        exhausted?: boolean;
    }, now?: number): void;
}
/** Render tiers: full rate while interacting, throttled idle, cheaper still on battery. */
export type RenderTier = 'interactive' | 'idle' | 'idle-battery';
/** Idle frame budget: ~30fps while nothing is being interacted with. */
export declare const IDLE_FRAME_MS: number;
/** Idle-on-battery frame budget: ~10fps. */
export declare const IDLE_BATTERY_FRAME_MS = 100;
/** Full-rate grace window after the last input — interaction never feels throttled. */
export declare const INTERACT_GRACE_MS = 500;
/** ms-per-frame budget for a tier (0 = render every rAF tick). */
export declare function frameBudgetMs(tier: RenderTier): number;
/**
 * Frame gate for a rAF loop: render when the tier's budget has elapsed
 * since the last RENDERED frame. The half-tick slack keeps a 33.3ms budget
 * from aliasing down (a 60Hz display ticks at 16.7ms — without slack,
 * 33.3ms would round up to every 3rd tick = 20fps instead of 30).
 */
export declare function shouldRender(nowTs: number, lastRenderTs: number, budgetMs: number, rafIntervalMs?: number): boolean;
