import type { HitRect } from './hit-test.ts';
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
    /**
     * Ordered label fallbacks, fullest → most compact; the widest that fits
     * draws. Overrides the tiers otherwise derived from `label`.
     */
    labelTiers?: string[];
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
/** The default visible span at the reference 16:9 container aspect: 3 minutes. */
export declare const DEFAULT_SPAN_REF_MS = 180000;
/**
 * The DEFAULT visible span for a container of `hostW` × `hostH` CSS px:
 * DEFAULT_SPAN_REF_MS (3 min) at a 16:9 aspect, scaled LINEARLY by the
 * actual aspect ratio — a wider container shows proportionally more time
 * at the same ms-per-pixel density, a squarer one less — clamped to
 * [MIN_SPAN_MS, MAX_SPAN_MS]. Degenerate sizes (zero/negative/non-finite
 * — an unlaid-out host) fall back to the 3-minute reference. The element
 * applies this on every resize until the first user gesture or
 * programmatic setViewport (`viewTouched`); it never overrides a chosen
 * window.
 */
export declare function defaultSpanForAspect(hostW: number, hostH: number, refSpanMs?: number): number;
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
 * Static scroll bounds: the earliest time the view may start at and the
 * latest it may end at. Each side is INDEPENDENT — a null side is
 * unbounded, so `{ min, max: null }` limits how far back the user can
 * scroll while the right edge still tracks the live clock, and
 * `{ min: null, max }` freezes the right edge over an unlimited past.
 */
export interface TimeBounds {
    min: number | null;
    max: number | null;
}
/**
 * Clamp a view into `bounds`, preserving its span: a view past `max`
 * shifts back, one before `min` shifts forward. A span WIDER than the
 * bounded range cannot preserve both stops, so it collapses to exactly
 * [min, max] — which is what a zoom-out against a short static window
 * should land on. Null and non-finite sides are ignored, so an unbounded
 * view comes back untouched.
 */
export declare function clampViewToBounds(view: TimeView, bounds: TimeBounds): TimeView;
/**
 * The widest span `bounds` can show, for a zoom clamp: the distance
 * between two finite stops, else `maxSpan`. Never below `minSpan` — a
 * bounded range narrower than the hard zoom floor still zooms to the
 * floor, and clampViewToBounds then parks that window over the range.
 */
export declare function boundedMaxSpan(bounds: TimeBounds, maxSpan?: number, minSpan?: number): number;
/**
 * Zoom the view by `factor` (> 1 zooms in) keeping `anchor` at the same
 * on-screen fraction — the time under the cursor stays under the cursor.
 * The span is clamped to [minSpan, maxSpan]; clamping preserves the anchor
 * fraction, so the invariant holds even at the clamp.
 */
export declare function zoomView(view: TimeView, anchor: number, factor: number, minSpan?: number, maxSpan?: number): TimeView;
/**
 * The view that renders ONE span full-width: [start, end] plus `pad`
 * fraction of the span on each side. Spans whose padded window would fall
 * under `minSpan` (instants, sub-second runs) center in a `minSpan`
 * window instead — never left-anchored by a later span clamp. Order- and
 * NaN-tolerant like setViewport (callers still clamp through it).
 */
export declare function fitSpanView(start: number | Date, end: number | Date, pad?: number, minSpan?: number): TimeView;
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
 * Direction-aware lane-stack scrollability: whether the stack can move
 * up (toward earlier lanes; scroll offset > 0) and/or down (offset <
 * max) RIGHT NOW. Feeding this — rather than a bare overflow bit — is
 * what lets a plain vertical wheel scroll an overflowing stack IN PLACE
 * while still handing the page every wheel the stack cannot use (the
 * standard nested-scroller contract).
 */
export interface LaneScrollable {
    up: boolean;
    down: boolean;
}
/**
 * routeWheel/WheelGestureRouter's lane-stack input: the legacy boolean
 * ("lanes overflow" — vertical wheels then NEVER consumed, the pinned
 * page-scroll-always-wins behavior) or the direction-aware LaneScrollable
 * form, which additionally lets vertical-dominant wheels scroll the stack
 * while it can actually move in the wheel's direction.
 */
export type LaneScrollInput = boolean | LaneScrollable;
/**
 * Route a wheel/trackpad gesture: ctrl/meta+wheel zooms (always consumed —
 * a pinch stream must never leak browser page-zoom, even on a zero-delta
 * tick); shift+wheel pans time (a vertical wheel pans horizontally);
 * otherwise the DOMINANT axis decides. Horizontal-dominant (|dx| > |dy|):
 * deltaX pans time — consumed — and the gesture's minor vertical
 * component still nudges the lane stack when it overflows the host (a
 * diagonal two-finger pan moves both axes; the event is consumed either
 * way, so nothing is half-forwarded). Vertical-dominant — ties included —
 * follows the NESTED-SCROLLER contract when given the direction-aware
 * LaneScrollable form: the stack takes the wheel (consumed,
 * laneScrollPx = dy) exactly while it can actually move in the wheel's
 * direction, and the moment it cannot — at its edge, or no overflow —
 * the event routes NOTHING and the page scrolls normally, so a tall lane
 * stack is scrollable in place and the page is always reachable past it.
 * With the legacy boolean overflow form, vertical-dominant NEVER
 * consumes, whether or not the lanes overflow — the pinned behavior that
 * keeps mere overflow from eating page scroll (an overflowing stack once
 * captured plain deltaY unconditionally, which ate the page's vertical
 * scroll on exactly the busy charts that always overflow; the
 * direction-aware form cannot regress into that, because consumption
 * requires headroom, which scrolling exhausts).
 *
 * This is the PER-EVENT rule — exact for a FRESH/ISOLATED event. A real
 * trackpad swipe is a STREAM of events whose jittery minority are
 * individually opposite-dominant, so the element routes streams through
 * WheelGestureRouter, which applies this rule to a gesture's first
 * decisive event and then holds that axis for the whole stream.
 */
export declare function routeWheel(e: WheelInput, lanes: LaneScrollInput): WheelRoute;
/** The three wheel-routing outcomes, without magnitudes (see classifyWheel). */
export type WheelClass = 'zoom' | 'pan' | 'passthrough';
/**
 * classifyWheel(e): the routing decision without magnitudes.
 *   'zoom'        iff e.ctrlKey || e.metaKey                       (always consumed, even zero-delta)
 *   'pan'         iff (e.shiftKey && (dyPx || dxPx) !== 0)         (shift-pan: dy first, else dx — Chrome vs Firefox)
 *                  || (!mods && |dxPx| > |dyPx|)                   (horizontal-dominant; implies dxPx !== 0)
 *   'passthrough' otherwise — vertical-dominant (ties included), shift with all-zero deltas,
 *                  or an all-zero unmodified tick. NEVER preventDefault on 'passthrough'.
 * where dxPx/dyPx = wheelDeltaToPixels(delta, e.deltaMode) — classification happens
 * AFTER deltaMode normalization so a line-mode (Firefox mouse) wheel classifies
 * identically to its pixel-mode equivalent.
 *
 * A readability/test wrapper over routeWheel's `consumed` contract — the
 * pinned invariant (see the test suite): for all e and every lanes input
 * o that gives the stack no vertical headroom (the legacy boolean form,
 * or a LaneScrollable with neither direction available),
 * routeWheel(e, o).consumed === (classifyWheel(e) !== 'passthrough').
 * The lanes input deliberately has NO input here: mere OVERFLOW must
 * never influence consumption — an overflowing lane stack capturing
 * plain vertical wheels unconditionally is exactly the regression this
 * pins out. (The direction-aware LaneScrollable form consumes vertical
 * wheels ONLY while the stack has headroom in the wheel's direction —
 * headroom that scrolling exhausts — which is a scroll TARGET decision,
 * not the overflow capture: classifyWheel stays the no-headroom table.)
 *
 * Like routeWheel, this describes a FRESH/ISOLATED event only: within a
 * live gesture, WheelGestureRouter's axis lock governs consumption, so a
 * 'passthrough'-classed jitter event inside a locked-horizontal stream IS
 * consumed (and a 'pan'-classed one inside a locked-vertical stream is
 * NOT).
 */
export declare function classifyWheel(e: WheelInput): WheelClass;
/**
 * Milliseconds of unmodified-wheel silence that ends a gesture: an
 * unmodified event arriving more than this after the previous unmodified
 * event classifies FRESH (per routeWheel's dominant-axis rule) instead of
 * inheriting the stream's axis lock. Sized between one momentum-tail
 * event spacing (well under it at ~16ms cadence, and still over the
 * sparse tail ticks) and a deliberate pause before a new gesture.
 */
export declare const WHEEL_GESTURE_GAP_MS = 200;
/**
 * Mid-gesture decisive-flip re-lock thresholds: the opposite axis must
 * beat the locked axis by MORE than the ratio AND carry at least the
 * pixel floor. The floor is sized above any proportional swipe jitter
 * (a mostly-horizontal swipe's vertical wobble rides at ~5-15px against
 * ~120px of dx, and shrinks with the swipe through the momentum tail)
 * but under a single deliberate scroll tick (~50-150px trackpad, 48px
 * for a 3-line discrete wheel).
 */
export declare const WHEEL_AXIS_FLIP_RATIO = 2;
export declare const WHEEL_AXIS_FLIP_MIN_PX = 24;
/**
 * Stream-level wheel router: routeWheel's per-event table plus a GESTURE
 * AXIS LOCK. A physical trackpad swipe arrives as a STREAM of wheel
 * events, and the jittery minority inside a mostly-horizontal swipe are
 * individually vertical-dominant (dx -4, dy 10 at gesture edges and
 * momentum tails) — per-event routing let each of those through to the
 * page, so a horizontal chart pan crept the page vertically; and
 * symmetrically, a page scroll's horizontal-dominant jitter nudged the
 * chart sideways. The first decisive unmodified event of a gesture LOCKS
 * the stream's axis:
 *
 *   'h' (|dx| > |dy|): EVERY unmodified event in the gesture is consumed
 *       — dx pans time and dy nudges the lane stack when it overflows
 *       (the per-event horizontal-dominant route, applied stream-wide),
 *       so the incidental vertical component never reaches the page.
 *   'v' (ties included): the gesture's TARGET latches from the lane
 *       stack's scrollability at lock time (routeWheel's nested-scroller
 *       rule). Stack can move in the initial direction → a LANE-SCROLL
 *       gesture: every unmodified event is consumed with
 *       laneScrollPx = dy for the rest of the gesture (the element clamps
 *       at the edges — hitting an edge mid-swipe does NOT hand the tail
 *       to the page, exactly the browser's own scroll-latching behavior;
 *       the NEXT gesture re-evaluates and passes through). Stack cannot
 *       move that way (at its edge, no overflow, or the legacy boolean
 *       input) → a PAGE gesture: NOTHING is consumed for the rest of the
 *       gesture — the page scrolls, and a horizontal-jitter event never
 *       pans the chart.
 *
 * A gap of more than WHEEL_GESTURE_GAP_MS since the gesture's last
 * unmodified event ends it; the next unmodified event re-classifies
 * fresh (a deliberate axis change usually comes with a natural pause).
 * Zero-delta unmodified ticks route nothing and neither start, extend,
 * nor reset a gesture. Modifier events (ctrl/meta zoom, shift pan) route
 * exactly as routeWheel and neither read nor extend the lock — a pinch
 * mid-scroll is its own intent, and the surrounding gesture survives it
 * (unless the modifier hold itself outlasts the gap, which is a real
 * pause).
 *
 * DECISIVE-FLIP RE-LOCK: a mid-gesture event whose OPPOSITE axis beats
 * the locked one by more than WHEEL_AXIS_FLIP_RATIO with at least
 * WHEEL_AXIS_FLIP_MIN_PX of magnitude re-locks the gesture to that axis
 * on the spot. The magnitude floor is what keeps jitter from flipping:
 * a swipe's incidental minor axis is proportional to its major one
 * (dy ~8 against dx ~120), so a proportional wobble can never clear the
 * floor AND the ratio at once, while a genuine direction change (a full
 * ~100px vertical tick mid-h-stream) flips immediately. The case that
 * makes this load-bearing rather than polish: a page scroll carries a
 * SECOND chart under the cursor mid-stream, and the first event its
 * fresh router happens to see is a horizontal-dominant jitter event —
 * without the flip that chart locks 'h' and eats the rest of the page's
 * scroll (browser-verified on the two-chart showcase).
 *
 * Pure with respect to time: `ts` is the caller's clock — the element
 * passes e.timeStamp; tests drive it explicitly — and the router never
 * reads Date.now(). Pinned invariant (see the test suite): a FRESH
 * router routes any single event exactly like routeWheel, so the
 * per-event behavior table above stays the isolated-event contract.
 */
export declare class WheelGestureRouter {
    private axis;
    /**
     * A 'v'-locked gesture's latched target: true = the lane stack (every
     * unmodified event consumed, laneScrollPx = dy, the element clamps at
     * edges), false = the page (nothing consumed). Latched from
     * scrollability when the 'v' lock is taken — at gesture start or on a
     * decisive flip — and held for the whole gesture, so a stack that hits
     * its edge mid-swipe never janks the tail into page scroll; the next
     * gesture re-evaluates against fresh scrollability.
     */
    private vLane;
    private lastTs;
    /**
     * Route one event of the stream. `ts` is the event's timestamp in ms
     * on any monotonic clock (e.timeStamp / performance.now()); WheelRoute
     * semantics — `consumed` is the preventDefault contract — are
     * unchanged from routeWheel.
     */
    route(e: WheelInput, lanes: LaneScrollInput, ts: number): WheelRoute;
}
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
 * other NON-ZOOM gesture stays pinned (a forward pan at the stop stays
 * live). ZOOM gestures never inherit the pin: the element passes
 * `wasFollowing: false` for them, because during a zoom the
 * cursor-anchored view must beat the now pin (the pin kept only the
 * zoomed SPAN and re-derived the position from `now`, anchoring
 * wheel/pinch zoom at the now marker instead of the cursor) — so a zoom
 * re-earns follow through the same snap rule as any fresh gesture: an
 * anchored zoom-in that pulls the right edge out of the snap zone parks
 * with the anchor intact, while one that stays at the live edge (or a
 * zoom-out pressing into the end stop) keeps following. While NOT
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
 * Snap a CSS-px coordinate to the nearest WHOLE device pixel — for TEXT
 * draw origins only. Glyphs rasterize sharpest when their origin sits on
 * the device-pixel grid (a fractional baseline smears every horizontal
 * stroke across two pixel rows as gray), and text — unlike bar
 * geometry — tolerates per-element rounding: nothing tiles against a
 * label, so the at-most-half-device-px step during scrolls/tweens reads
 * as stepping, never as neighbors jiggling. Geometry keeps the single
 * global view-origin rounding (snapViewToDevicePixels); never round bars
 * per element.
 */
export declare function snapTextOrigin(v: number, dpr: number): number;
/**
 * The now line's x (CSS px, `gutterX` offset included), snapped to the
 * device-pixel grid + half a device px (a crisp 1px stroke). Computed
 * against the RAW view — deliberately NOT the snapViewToDevicePixels
 * render view all scene geometry uses: while follow-now pins the view,
 * `now` sits at a FIXED fraction of the raw view's span, so this x is
 * frame-to-frame CONSTANT; routing it through the snapped view instead
 * re-adds the origin's per-frame quantization error (±half a device px),
 * which flips the rounded x between adjacent device pixels as the view
 * slides — the now line visibly wiggles while everything else scrolls
 * smoothly. On a parked (static) view the two computations differ only by
 * a constant sub-device-px offset, so the line just steps whole device
 * pixels as the clock advances. Scene geometry must keep the snapped
 * render view (bars are SCENE-anchored and must translate together); the
 * now line alone is VIEWPORT-anchored, which is why it alone reads the
 * raw view. Degenerate dpr passes the unsnapped x through.
 */
export declare function nowLineX(now: number, view: TimeView, gutterX: number, plotWidthCss: number, dpr: number): number;
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
 * track. STATELESS — when the visible membership changes, everything
 * reflows into freed tracks; the element rows its lanes through the
 * sticky TrackAllocator below instead, which shares this contract but
 * keeps visible rows pinned across membership churn.
 */
export declare function packVisibleTracks(items: readonly PackItem[], view: TimeView): {
    tracks: number[];
    trackCount: number;
};
/**
 * Bound on remembered id → track assignments per TrackAllocator (LRU
 * eviction beyond it): generous enough to cover every id a lane plausibly
 * cycles through between revisits, small enough that an unbounded live
 * feed can never grow the memory forever. An evicted id simply re-packs
 * as new on return.
 */
export declare const TRACK_MEMORY_CAP = 2048;
/**
 * STICKY sub-track allocation for one lane — the STATEFUL counterpart of
 * packVisibleTracks, built so rows stop shifting under the viewer as the
 * visible membership churns (panning, live updates):
 *
 * - An item assigned in the PREVIOUS call and still visible KEEPS its
 *   track unconditionally (re-verified against the other keepers, so
 *   even an item whose times were live-edited can never create a
 *   same-track overlap).
 * - An item RETURNING after scrolling out gets its remembered track back
 *   when no visible occupant conflicts — best-effort row memory, bounded
 *   by an LRU cap (`memoryCap`, default TRACK_MEMORY_CAP).
 * - Everything else — brand-new arrivals, the rare displaced returner —
 *   takes the LOWEST track with no time overlap among the items placed
 *   this call. Density recovers from the bottom: once a tall burst
 *   scrolls off-screen its high tracks fall out of use and the lane
 *   shrinks to what is still visible, WITHOUT re-rowing anything the
 *   viewer is looking at (a lone survivor parked on a high track holds
 *   its row — and the lane's height — until it leaves the window).
 *
 * Same contract as packVisibleTracks otherwise: tracks[i] aligned to the
 * input (-1 = outside the view; callers keep the previous assignment),
 * trackCount = highest in-use visible track + 1 (>= 1 — an empty window
 * collapses to one track), footprints via PACK_MIN_MS instants and
 * ongoing-blocks-forever, visible same-track items can never overlap in
 * time, and results are deterministic given the same call sequence. A
 * FRESH allocator's first call reproduces packVisibleTracks exactly (no
 * memory yet — pure lowest-free in (start, id) order).
 */
export declare class TrackAllocator {
    /** id → last assigned track. Map insertion order doubles as LRU recency. */
    private memory;
    /** ids assigned (visible) by the previous call — their tracks are kept. */
    private live;
    /** Double-buffer partner for `live` (swapped per call — no Set churn). */
    private liveNext;
    private cap;
    private visScratch;
    private returningScratch;
    private freshScratch;
    private placedScratch;
    constructor(memoryCap?: number);
    /** Assign tracks for the items visible in `view` (see the class doc). */
    assign(items: readonly PackItem[], view: TimeView): {
        tracks: number[];
        trackCount: number;
    };
}
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
/**
 * Which ends of [startMs, endMs] are CLIPPED by the view — the interval's
 * true extent continues off-screen past that edge. Drives the element's
 * edge-continuation shadow. Two deliberate exemptions: an end within half
 * a pixel of the window edge does NOT count (the interval genuinely
 * starts/ends there — and the device-pixel view snap shifts edges by up
 * to a pixel, which must never read as continuation); and a side only
 * counts when the visible part reaches all the way through the shadow
 * zone (`fadePx`), so a barely-poking stub stays a visible stub instead
 * of being swallowed by it. Pass the live edge as `endMs` for ongoing
 * intervals.
 */
export declare function edgeContinuation(startMs: number, endMs: number, view: TimeView, plotWidth: number, fadePx: number): {
    left: boolean;
    right: boolean;
};
/**
 * Fraction of a pip's width between two drawn pips. Under 1, so a dense
 * run draws its pips OVERLAPPING — packed edge over edge, each still its
 * own diamond, which is what a saturated row of events looks like.
 * Spacing them apart instead would throw away marks the row had room for.
 */
export declare const CLUSTER_OVERLAP_FRAC = 0.5;
/**
 * Floor on that pitch, in CSS px. Below it the outlines stop resolving
 * and the row smears into one shape — the exact failure the thinning
 * exists to prevent (docs/timeline/zoom-out-never-merges.md).
 */
export declare const CLUSTER_MIN_PITCH_PX = 3;
/**
 * Default centre-to-centre distance below which two instants cannot both
 * be drawn: half a full-size pip (~12px incl. its stroke). The element
 * overrides it per lane, because a compact lane's pip shrinks to a dot
 * and more of them fit.
 *
 * ONE constant does two jobs, and they are the same job: instants closer
 * than this chain into a cluster, and within a cluster the members are
 * THINNED to exactly this pitch. Marks are dropped to hold it, never
 * widened or merged to close it.
 */
export declare const CLUSTER_PITCH_PX: number;
/**
 * A cluster up to this wide (CSS px) is POINT-LIKE: its members really do
 * sit at one spot, so it draws as the ×N stack glyph. Anything wider
 * spans real time and draws its thinned marks instead — see
 * InstantCluster.
 */
export declare const CLUSTER_STACK_MAX_PX = 12;
/**
 * One drawn mark of a spread cluster — the unit it draws, hit-tests and
 * zooms by. It is one member, drawn as the pip that member always was,
 * standing for the members the thinning dropped after it.
 */
export interface ClusterMark {
    /** The drawn member's timestamp — never a midpoint, never snapped. */
    time: number;
    /** Half-open range into the cluster's `indices`: this mark's member and the ones it stands for. */
    from: number;
    to: number;
}
/** A group of visually-overlapping instant markers (see clusterInstants). */
export interface InstantCluster {
    /** Indices into the input array, in (start, id) order. */
    indices: number[];
    /**
     * Member start-time extent [first, last] (equal ends when every member
     * is coincident) — the click-to-zoom target (clusterZoomView) and the
     * marker anchor (clusterMarkerTime).
     */
    extent: TimeRange;
    /**
     * The members THINNED to a drawable pitch, in time order, together
     * covering every member exactly once. Zooming out drops marks; it
     * never merges them, so N events can never render as one shape (see
     * docs/timeline/zoom-out-never-merges.md).
     */
    marks: ClusterMark[];
}
/**
 * SCALE-AWARE clustering of instant markers: a greedy transitive sweep
 * in time order merges instants whose centers sit within `pitchPx` CSS px
 * of their neighbor at the view's scale — exactly the ones whose pips
 * would overdraw each other — and zooming in progressively splits every
 * cluster until each pip stands at its true timestamp.
 *
 * The chain is maximal: it breaks only at a real gap in the data, never
 * at a width cap, so a cluster is "one visually continuous run of
 * instants" and nothing about it depends on where the sweep started. A
 * run that spans real time is not compacted to a point — it carries
 * `marks`: its members THINNED to `pitchPx`, each at its own true
 * timestamp and each drawn as the pip it always was. Marks are dropped,
 * never merged and never redrawn as some other glyph, so however far out
 * you zoom the run stays a row of separated pips (halve the width, halve
 * the pips) and can never fuse into one shape. The rule and the two ways
 * this has been got wrong: docs/timeline/zoom-out-never-merges.md.
 *
 * Only instants participate: an item must be terminal (end != null — an
 * ongoing interval will grow into a bar) with a duration mapping under
 * `instantPx` (the pip threshold) at this scale. Membership depends only
 * on time DELTAS and the scale — never on the viewport's position — so a
 * pure pan can never change clusters (no jitter), and items beyond the
 * view still cluster, so a group scrolls into view already formed.
 * Clusters have >= 2 members (a lone pip is not a cluster; everything
 * un-clustered gets memberOf -1). Deterministic under input re-ordering:
 * the sweep runs in (start, id) order and indices refer to input
 * positions.
 */
export declare function clusterInstants(items: readonly PackItem[], view: TimeView, plotWidth: number, pitchPx?: number, instantPx?: number): {
    clusters: InstantCluster[];
    memberOf: number[];
};
/** Fraction of the zoomed window a clicked cluster's member extent occupies (centered). */
export declare const CLUSTER_ZOOM_FILL_FRAC = 0.6;
/**
 * The view a cluster click zooms to: the member extent centered, filling
 * CLUSTER_ZOOM_FILL_FRAC of the window, never narrower than `minSpan` —
 * deep enough that the members separate past the join threshold and the
 * cluster SPLITS. Fully coincident members zoom to minSpan and stay one
 * marker: they genuinely share a timestamp, and the tooltip lists them.
 */
export declare function clusterZoomView(extent: TimeRange, minSpan?: number, fillFrac?: number): TimeView;
/**
 * Where a cluster's marker sits, in TIME: the extent midpoint while that
 * fits the window, slid along the visible slice of the extent when the
 * window clips it (the sticky-label pattern — a transitive chain
 * straddling a viewport edge keeps an on-screen marker instead of hiding
 * its members' evidence), and null once no part of the extent is
 * visible. `marginMs` insets the slid marker from the window edges (pass
 * the marker radius in ms) so it stays fully visible. Continuous in the
 * view — panning slides it smoothly, never a jump — and constant (the
 * midpoint) while the extent is fully inside the window.
 */
export declare function clusterMarkerTime(extent: TimeRange, view: TimeView, marginMs: number): number | null;
/** Half-width (CSS px) of a minimap handle's hit zone — generously past the drawn bar. */
export declare const MINIMAP_HANDLE_HIT_PX = 8;
/** Minimum drawn width (CSS px) of the minimap's window rect — a 10-min window on a week-long extent stays visible and grabbable. */
export declare const MINIMAP_MIN_WINDOW_PX = 6;
/** The minimap window rect's horizontal extent, in strip px. */
export interface MinimapWindowRect {
    x0: number;
    x1: number;
}
/** What a strip x coordinate lands on (see minimapHitZone). */
export type MinimapZone = 'left-handle' | 'right-handle' | 'inside' | 'before' | 'after';
/**
 * The strip's data extent, from what the element knows: the earliest
 * loaded interval start — widened by coverage knowledge where it helps
 * (the first covered time and the exhausted-history boundary both count:
 * loaded-but-empty history and the known start of time are part of the
 * overview) — through max(now, the latest interval end). Null when no
 * start is known at all (nothing loaded — the strip hides). A
 * degenerate/tiny extent is padded backward to `minSpanMs` so the strip
 * never divides by zero and a single instant still reads as a region.
 */
export declare function minimapExtent(earliestStart: number | null, latestEnd: number | null, now: number, exhaustedBefore?: number | null, coveredStart?: number | null, minSpanMs?: number): TimeView | null;
/**
 * Map the viewport into strip px: the window rect, CROPPED to the strip
 * (a view hanging past the extent shows truncated at the strip edge —
 * never slid to a lying position), with a minimum visual width applied
 * around the center BEFORE cropping (a tiny window on a huge extent
 * stays visible); a view entirely outside the extent pins a minimum
 * sliver at the nearer strip edge. Degenerate extent/width yields the
 * full strip.
 */
export declare function minimapWindowRect(view: TimeView, extent: TimeView, width: number, minPx?: number): MinimapWindowRect;
/**
 * Hit-test a strip x against the window rect. Handles win over the
 * middle and their zones reach `hitPx` OUTSIDE the rect (generous grab
 * targets) but only min(hitPx, windowWidth/4) INSIDE it — a narrow
 * window keeps a grabbable middle instead of the handle zones swallowing
 * it. When both handle zones cover x (tiny window), the nearer handle
 * wins (ties go left). Outside everything: 'before'/'after' — the
 * click-to-center zones.
 */
export declare function minimapHitZone(x: number, rect: MinimapWindowRect, hitPx?: number): MinimapZone;
/**
 * Grab-the-middle: pan the window by a pointer delta in strip px, span
 * preserved, clamped inside the extent at both ends (a window wider than
 * the whole extent pins to the extent's live end). Pixel-delta based so
 * a drag stays 1:1 under the pointer even while the extent's live end
 * advances mid-drag.
 */
export declare function minimapPan(view: TimeView, dxPx: number, extent: TimeView, width: number): TimeView;
/**
 * Drag one window edge to the strip x. The dragged edge is clamped to
 * the extent and to [minSpan, maxSpan] against the fixed opposite edge —
 * dragging a handle past (or into) the other CLAMPS at the minimum span,
 * it never flips which edge is which mid-drag. The min-span floor wins
 * over the extent clamp (the window must stay a valid view even inside
 * a tiny extent).
 */
export declare function minimapResize(view: TimeView, edge: 'left' | 'right', xPx: number, extent: TimeView, width: number, minSpan?: number, maxSpan?: number): TimeView;
/** Click outside the window: re-center it at the clicked time, span preserved, extent-clamped like a pan. */
export declare function minimapCenter(view: TimeView, xPx: number, extent: TimeView, width: number): TimeView;
export { expandHitRect, hitTestRects, distSqToSegment, hitTestPolyline } from './hit-test.ts';
export type { HitRect } from './hit-test.ts';
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
/** The clamped phase window `segmentAtTime` resolved, with its array index. */
export interface SegmentHit {
    /** Index into the interval's `segments` array. */
    index: number;
    /** The segment's `kind` (style-map key — the legend/tooltip vocabulary). */
    kind: string;
    /** Phase start, clamped into the interval (ms). */
    start: number;
    /** Phase end (null end resolves to `intervalEnd`), clamped (ms). */
    end: number;
}
/**
 * The segment PAINTED at time `t` inside an interval's bar: the LAST array
 * entry covering t (segments draw in order — later overpaints earlier),
 * with the draw path's clamps (start floored to `intervalStart`, null/late
 * end capped to `intervalEnd` — pass the effective end: `end ?? now`).
 * Coverage is half-open [start, end) so shared phase boundaries resolve to
 * the incoming phase, EXCEPT t at the interval's own end still hits a
 * segment ending there (the bar's last pixel must resolve). Null when no
 * segment covers t (the pointer is over the base bar, or off it).
 */
export declare function segmentAtTime(segments: readonly TimelineSegment[] | null | undefined, intervalStart: number, intervalEnd: number, t: number): SegmentHit | null;
export { hashString, categoryHue, categoryJitter, categoryColor, dimColor, labelHaloColor, } from './color.ts';
export type { CategoryColorOptions } from './color.ts';
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
    /**
     * A DIMMED region: its GEOMETRY — fill, hatching, border — gets the
     * uniform dimColor transform (50% saturation, 50% value), as if one
     * filter lay over the section. Label/badge text is deliberately
     * EXEMPT: it always renders at the full-contrast theme foreground
     * over a thin counter-color halo (labelHaloColor), so labels stay
     * readable over dimmed and hatched surfaces at every zoom — deriving
     * text color from the section produced unreadable grey-on-grey that
     * flipped with the zoom level.
     */
    dimmed?: boolean;
}
/** Named style map: interval `state` / segment `kind` → treatment. */
export type StyleMap = Record<string, IntervalStyle>;
/**
 * Built-in treatments (consumer keys spread on top via the element's
 * `styles` property): '' solid; 'emphasis'/'failed' unmissable — thick
 * emphasis border + corner bang glyph + stipple, hue untouched;
 * 'dim'/'queued' uniformly dimmed (the `dimmed` flag: 50% saturation,
 * 50% value over fill and border; label text stays full-contrast — see
 * `dimmed`'s doc); 'hatch'/'waiting'
 * 45° stripes, dimmed the same way (a wait is de-emphasized time);
 * 'outline' hollow; 'cancelled' hollow + DASHED category-hue border —
 * reads "stopped, not failed" at a glance: never the emphasis color,
 * never the bang glyph, never a solid success body. (Below dash
 * legibility the element draws a BAR's border solid; the hollow body
 * still separates a tiny cancelled bar from a solid one. Pips are exempt:
 * a cancelled instant keeps a dashed diamond outline, the pattern
 * rescaled to close around the perimeter.)
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
/**
 * Draw budget (ms per rendered frame) while the ONLY motion on screen is
 * CLOCK-driven — the follow-now scroll and ongoing-bar growth. The scene
 * then translates exactly one whole DEVICE pixel per
 * span / (plotWidthCss * dpr) ms, so redrawing any faster produces
 * pixel-identical frames. The effective rate is therefore
 * min(tier fps, device px per second) — expressed here in budget form as
 * max(tierBudgetMs, per-device-pixel period), which makes the tier
 * budget the structural CEILING (the result is never below it, so the
 * chart never draws faster than the pre-existing tier pacing — the
 * interactive tier's 0 budget yields the bare per-pixel period, i.e.
 * min(display rate, px rate)). There is deliberately NO upper cap: a
 * slowly scrolling chart draws exactly at its own per-pixel rate, each
 * 1px step landing the instant it is due — extra frames between steps
 * would be identical, and a fixed wake floor (the retired ~1s clock-wake
 * cap) is precisely what read as stuttery stepping. Delivery is the
 * caller's rAF loop SKIPPING frames against this budget on an even
 * due-time grid — never a timer — so the cadence stays frame-aligned
 * and even. Degenerate geometry (empty/invalid span, no width, bad dpr)
 * falls back to the tier budget: plain pacing, never a bogus throttle.
 */
export declare function clockDrawBudgetMs(view: TimeView, plotWidthCss: number, dpr: number, tierBudgetMs: number): number;
