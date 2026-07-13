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
 * the live edge). Every follow transition is CONTINUOUS: engaging eases
 * the small follow lead in over ~200ms from where the gesture parked,
 * disengaging lets the backward deltas consume the lead (any residual
 * glides out), and the pill glides to the followed position — the view
 * never teleports in a single frame (reduced motion snaps instead).
 * Interaction is trackpad-first: two-finger pan (x = time;
 * y scrolls the lane stack when it overflows, and is otherwise left to
 * the PAGE — a plain vertical wheel never pans the chart sideways and
 * never has its default prevented, so page scrolling works across the
 * chart), ctrl/meta+wheel = smooth zoom anchored under the cursor
 * (discrete wheel steps glide), shift+wheel = time pan, drag = pan, pinch =
 * zoom, arrows/±/Home/End when focused. `loadRange` turns scrolling into
 * the past into async history requests — for BACKWARD gaps only; the live
 * forward edge always belongs to the consumer's own setData/mergeData
 * `coverage` — with uncovered regions visibly distinct from empty-but-known
 * ones and an explicit end-of-history boundary.
 *
 * FEED STALENESS: every setData/mergeData (or an explicit markFresh())
 * stamps the feed fresh; when `staleAfterMs` (default 10s) passes without
 * a stamp the chart STOPS trusting the clock — the live edge (ongoing
 * bars, the now line, the follow pin, the forward clamp) freezes at the
 * last vouched timestamp instead of extrapolating a dead feed (a finished
 * run must never render as "running forever"), ongoing bars restyle as
 * unknown (dim + hatch), a "live data stale (Ns) — reconnecting…" note
 * counts up forever, and 'stalechange' fires. The next stamp recovers,
 * gliding the edge back to the live clock — no teleports in either
 * direction (reduced motion snaps). Consumers should resync with one full
 * setData on recovery.
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
import { type TimelineLane, type TimelineInterval, type TimelineConnector, type TimelineMarker, type TimeView, type TimeRange, type StyleMap } from './timeline-view-math.ts';
export * from './timeline-view-math.ts';
/**
 * Theme defaults — the dark "Scratch Proto" palette. Override per element /
 * ancestor / :root with the CSS custom properties named here.
 */
export declare const THEME_DEFAULTS: {
    /** --timeline-bg — plot background. */
    bg: string;
    /** --timeline-fg — bar labels and primary text. */
    fg: string;
    /** --timeline-muted — axis ticks, lane labels, secondary text. */
    muted: string;
    /** --timeline-grid — vertical time gridlines. */
    grid: string;
    /** --timeline-hairline — horizontal lane separators. */
    hairline: string;
    /** --timeline-row-tint — alternating lane tint ('none' disables). */
    rowTint: string;
    /** --timeline-now — the live now line + jump pill accent. */
    now: string;
    /** --timeline-emphasis — emphasis/failed borders and glyphs. */
    emphasis: string;
    /** --timeline-font — font family for all canvas text. */
    font: string;
    /** --timeline-font-size — base font size in px. */
    fontSize: number;
    /** --timeline-cat-lightness — oklch lightness for category fills (0..1). */
    catLightness: number;
    /** --timeline-cat-chroma — oklch chroma for category fills. */
    catChroma: number;
    /** --timeline-track-height — sub-track bar height in px (clamped 10..40). */
    trackHeight: number;
    /**
     * --timeline-track-height-compact — sub-track height in CSS px for lanes
     * auto-fit demotes (clamped 2..track-height; the canvas' DPR scaling
     * already multiplies to device pixels, so 4 here is 8 device px at dpr 2).
     */
    trackHeightCompact: number;
    /** --timeline-gutter-width — lane-label gutter in px (0 = auto-size). */
    gutterWidth: number;
};
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
export type LoadRangeFn = (start: number, end: number) => Promise<{
    exhausted?: boolean;
} | void>;
/** What the pointer is over — handed to tooltipFor and hover/click events. */
export type TimelineHit = {
    type: 'interval';
    interval: TimelineInterval;
    lane: TimelineLane;
} | {
    type: 'connector';
    connector: TimelineConnector;
    missingEndpoint?: 'from' | 'to';
} | {
    type: 'marker';
    marker: TimelineMarker;
} | {
    type: 'lane';
    lane: TimelineLane;
};
/** Tooltip content callback: string or Node (never injected as HTML). */
export type TooltipFn = (hit: TimelineHit) => string | Node | null | undefined;
/** Color override callback: return a CSS color, or null for the default. */
export type ColorFn = (interval: TimelineInterval, lane: TimelineLane) => string | null | undefined;
/**
 * The timeline element. Auto-registered as `<timeline-view>` when this
 * module loads (unless the name is taken). Data arrives via properties and
 * methods — setData / mergeData / setLanes / setIntervals / setConnectors /
 * setMarkers — never attributes; the only attributes are scalar toggles:
 * `no-live-pill` (hide the jump-to-now pill), `no-auto-fit` (disable
 * compact-lane auto-fit), `history-end-text` (boundary label), `empty-text`
 * (empty-state hint).
 *
 * Auto-fit (default ON): each layout pass compares the natural lane stack
 * (every lane at --timeline-track-height) against the host's plot height;
 * while it overflows, whole lanes are demoted to the compact track height
 * (--timeline-track-height-compact, default 4px) one at a time — tallest
 * (most parallel) lane first, ties demoting the LOWER lane first so
 * top-of-chart lanes keep detail longest — until it fits or every lane is
 * compact (then the vertical lane scroll takes over as before). Demotion
 * is immediate; promotion is hysteretic (~10% headroom required) so
 * heights never flap at the boundary, and changes ease through the same
 * ~150ms layout tween as track-count changes. Read `fitState` / listen
 * for 'fitchange' to observe the demotion set.
 */
export declare class TimelineViewElement extends HTMLElement {
    static get observedAttributes(): string[];
    private canvas;
    private ctx;
    private tooltipEl;
    private pillEl;
    private emptyEl;
    private staleEl;
    private lanes;
    private laneIdxById;
    private perLane;
    private byId;
    private connectors;
    private markers;
    private layout;
    private styleMap;
    private view;
    private following;
    private laneScroll;
    private leadFrac;
    private leadAnim;
    private lastFreshMs;
    private staleAfter;
    private feedStale;
    private edgeAnim;
    private staleTimer;
    private staleNoteText;
    private coverage;
    private loadRangeFn;
    private loadTick;
    private tooltipForFn;
    private colorForFn;
    private nowFn;
    private pointers;
    private lastMouse;
    private dragTotal;
    private downHit;
    private hover;
    private hoverIntervalId;
    private glidePx;
    private glideX;
    private lastFrame;
    private lastInputTs;
    private lastRenderTs;
    private batteryDischarging;
    private batteryOff;
    private packEpoch;
    private packedEpoch;
    private packedStart;
    private packedEnd;
    private targetCounts;
    private displayCounts;
    private targetHeights;
    private displayHeights;
    private layoutAnim;
    private fitCount;
    private demotedIds;
    private fitKey;
    private rvCache;
    private cssW;
    private cssH;
    private dpr;
    private theme;
    private fontAxis;
    private fontBar;
    private charW;
    private gutterW;
    private oklch;
    private reducedMotion;
    private colorCache;
    private patternCache;
    private raf;
    private dirty;
    private connected;
    private inView;
    private ro;
    private io;
    private motionMq;
    constructor();
    connectedCallback(): void;
    disconnectedCallback(): void;
    attributeChangedCallback(): void;
    /** Replace all data (lanes, intervals, connectors, markers, coverage). */
    setData(data: TimelineData): void;
    /**
     * Merge data into the current set: lanes/intervals/connectors dedupe by
     * id (intervals replace in place), markers append-and-dedupe by
     * (time, label, kind). The way loadRange results are supplied.
     */
    mergeData(data: TimelineData): void;
    /**
     * Stamp the live feed FRESH as of `ts` (default: the current clock).
     * setData/mergeData stamp automatically; call this from polls that
     * returned "no changes" so a quiet-but-healthy feed never reads as
     * stale. When more than `staleAfterMs` passes without a stamp the chart
     * enters stale mode: the live edge (ongoing bars, the now line, the
     * follow pin) FREEZES at the last stamped time — never extrapolating
     * state the data no longer vouches for — ongoing bars restyle as
     * unknown, and a "live data stale — reconnecting" note appears until
     * the next stamp. On recovery, do ONE full resync (setData) before
     * resuming incremental merges — runs that ended during the outage
     * otherwise stay unknown.
     */
    markFresh(ts?: number | Date): void;
    /**
     * ms without a freshness stamp before the chart declares its feed stale
     * (default STALE_AFTER_DEFAULT_MS = 10s; tune to ~2 poll intervals).
     * Zero or a non-finite value disables staleness — for static datasets
     * that are loaded once and never fed.
     */
    get staleAfterMs(): number;
    set staleAfterMs(v: number);
    /** Read-back of the staleness state (mirrors the latest 'stalechange'). */
    get staleState(): {
        stale: boolean;
        lastFresh: number | null;
    };
    /** Individual setters (each replaces just that slice of the data). */
    setLanes(lanes: TimelineLane[]): void;
    setIntervals(intervals: TimelineInterval[]): void;
    setConnectors(connectors: TimelineConnector[]): void;
    setMarkers(markers: TimelineMarker[]): void;
    /** Drop everything (data, coverage, viewport stays). */
    clear(): void;
    /** Style map for interval `state` / segment `kind` (spread over the built-ins). */
    get styles(): StyleMap;
    set styles(map: StyleMap | null | undefined);
    /** Async history loader (see LoadRangeFn); null disables. */
    get loadRange(): LoadRangeFn | null;
    set loadRange(fn: LoadRangeFn | null | undefined);
    /** Tooltip content override; null restores the built-in tooltip. */
    get tooltipFor(): TooltipFn | null;
    set tooltipFor(fn: TooltipFn | null | undefined);
    /** Per-interval color override; null restores category colors. */
    get colorFor(): ColorFn | null;
    set colorFor(fn: ColorFn | null | undefined);
    /** Clock override (ms since epoch) for replay/testing; null = Date.now. */
    get nowProvider(): (() => number) | null;
    set nowProvider(fn: (() => number) | null | undefined);
    /**
     * Current auto-fit state (read-only): whether auto-fit is enabled (no
     * `no-auto-fit` attribute) and which lanes are demoted to the compact
     * track height right now, as lane ids in display order. Mirrors the
     * latest 'fitchange' event — cheap to poll, handy for debugging.
     */
    get fitState(): {
        enabled: boolean;
        demoted: string[];
    };
    /** The visible time window (ms since epoch). */
    get viewport(): TimeView;
    /**
     * Jump/zoom to an explicit window (hard-stops at now; disengages follow
     * unless it ends within the 2-device-px snap zone of now).
     */
    setViewport(start: number | Date, end: number | Date): void;
    /** Whether the right edge is pinned to live "now" (default true). */
    get followNow(): boolean;
    set followNow(v: boolean);
    /**
     * Re-engage follow mode, keeping the current span: a fast
     * JUMP_TO_NOW_TWEEN_MS glide from wherever the view is to the followed
     * position — never a single-frame teleport (reduced motion snaps).
     */
    jumpToNow(): void;
    /** Re-read the --timeline-* custom properties (call after retheming). */
    refreshTheme(): void;
    private ingestInterval;
    /**
     * Re-sort and re-layout after any data change. Track assignment and lane
     * heights come from the VISIBLE window (updateVisibleLayout), so a
     * historical parallelism burst stops padding its lane once off-screen.
     */
    private rebuild;
    /**
     * Track assignment + lane heights from the intervals intersecting the
     * CURRENT viewport (partial overlap counts; a lane with nothing visible
     * collapses to one track). Deterministic given the visible data — a
     * merely-translating viewport over unchanged overlap recomputes to the
     * identical result, so nothing jitters frame to frame. Auto-fit then
     * demotes lanes to the compact track height until the stack fits the
     * host (computeAutoFit — tallest lanes first, hysteretic promotion, a
     * pure function of the visible counts + host height, so it shares the
     * same stability guarantee). Count AND height CHANGES ease over
     * LAYOUT_TWEEN_MS (snapped under prefers-reduced-motion). this.layout
     * always reflects the CURRENT (possibly animating) heights, and
     * hit-testing shares it (rectFor reads displayHeights), so hovers stay
     * aligned mid-tween.
     */
    private updateVisibleLayout;
    private metrics;
    /** Effective compact track height: the themed value, never above the normal height. */
    private compactTrackH;
    /** The (possibly animating) per-track bar height of a lane. */
    private laneTrackHeight;
    /** Fire 'fitchange' when the demotion SET (by lane id) actually changes. */
    private emitFitChange;
    private autoGutter;
    private nowMs;
    private perfNow;
    private tzOffsetMs;
    /** Battery awareness for the idle render tier (feature-detected; absent API = AC tier). */
    private watchBattery;
    private noteInput;
    /** Current pacing tier: any live gesture/tween = full rate; else idle (AC/battery). */
    private renderTier;
    /**
     * The LIVE EDGE every live semantic uses — ongoing (end = null) bar
     * ends, the now line, the follow pin, and the forward clamp on user
     * views: the real clock while the feed is fresh, FROZEN at lastFreshMs
     * while it is stale (liveEdgeTarget). The stale <-> fresh transition is
     * EASED (followLeadAt over JUMP_TO_NOW_TWEEN_MS): entering stale mode
     * retracts the edge from wherever it had extrapolated back to the last
     * vouched timestamp as a glide, and recovery advances it to the live
     * clock the same way — composing with the follow pin, so neither
     * transition teleports the view. Reduced motion snaps.
     */
    private liveEdge;
    /**
     * Re-evaluate staleness; on a transition, glide the live edge and
     * announce it. `edgeFrom` overrides the glide's start point — markFresh
     * passes the edge it captured before moving the stamp.
     */
    private updateStale;
    /** The stale affordance: "live data stale (Ns) — reconnecting…", counting forever. */
    private syncStaleNote;
    /**
     * Advance + read the animated follow lead (fraction of span). Time-based
     * (followLeadAt), so multiple reads within a frame agree; the tween
     * clears itself the moment it lands on its target. A reduced-motion
     * preference snaps any in-flight glide to its target.
     */
    private currentLead;
    /** Retarget the follow-lead tween from `from` toward `target` (reduced motion snaps). */
    private glideLead;
    /**
     * following := true, easing from the CURRENT view position to the
     * followed lead over `dur` — jumpToNow's glide (and the followNow
     * setter's). The seed lead may be deeply negative (a parked view far in
     * the past): the glide crosses the whole gap, decelerating into the
     * pin — never a teleport. This frame's pin lands exactly where the view
     * already is.
     */
    private engageFollowGlide;
    private pinToNow;
    /**
     * The disengaged counterpart of the per-tick pin: while residual follow
     * lead is still gliding out after a backward-pan disengage, the view's
     * end tracks the DECAYING ceiling now + span * lead — moving backward by
     * at most the easing step per frame — until the lead is gone or "now"
     * overtakes the parked end first. Once settled this is a no-op and the
     * view is an ordinary parked view (end <= now).
     */
    private decayLead;
    /**
     * Apply a user-driven viewport. Backward PANS disengage follow outright;
     * everything else re-engages only within FOLLOW_SNAP_DEVICE_PX device
     * pixels of the `now` end stop (followAfterGesture — the pan carve-out
     * is load-bearing: without it, small trackpad pan steps were re-pinned
     * to "now" one by one and horizontal panning never escaped follow mode).
     * The follow rule reads the RAW gesture (an overshoot past now must
     * count as "at the stop"); the view actually applied hard-stops at now
     * (clampViewToNow), so every input path — wheel, drag, pinch, keyboard,
     * setViewport — parks exactly at the end stop, which is what makes the
     * tiny re-engage zone reliably hittable. Interactive gestures keep the
     * pin while following (zooming at the live edge stays live); a
     * programmatic setViewport (`jump`) is exempt from that — it lands
     * where it says, engaging follow only inside the snap zone.
     *
     * The FOLLOW LEAD is eased, never assigned: engaging keeps the view
     * exactly where the gesture parked it and the per-tick pin glides end
     * out to now + span * FOLLOW_LEAD_FRAC over FOLLOW_LEAD_TWEEN_MS;
     * disengaging (a backward pan) lets the gesture's own delta consume the
     * lead and glides any residual back down (decayLead) instead of slamming
     * end to now in the same frame — the two single-frame ~2%-of-plot-width
     * teleports this replaced. Reduced motion snaps both.
     */
    private applyUserView;
    private emitViewport;
    private plotWidth;
    private plotHeight;
    private maxLaneScroll;
    private clampLaneScroll;
    private msPerPx;
    /**
     * The view all GEOMETRY goes through: origin snapped to whole device
     * pixels (memoized). One global rounding, zero per-element rounding —
     * the scene translates in integer device-pixel steps and bars never
     * jiggle relative to each other (see snapViewToDevicePixels).
     */
    private renderView;
    private syncChrome;
    private invalidate;
    /** True while something time-based needs continuous frames. */
    private animating;
    private schedule;
    private onFrame;
    private onVisibility;
    private onMotionPref;
    private pumpLoad;
    private resizeBackingStore;
    private readTheme;
    private styleFor;
    private resolved;
    private patternFor;
    /**
     * CSS-px rect of an interval (valid even outside the viewport). Mapped
     * through the device-pixel-snapped render view and deliberately NOT
     * rounded per element — one global rounding policy (renderView), so bars
     * hold exact relative offsets while the viewport translates.
     */
    private rectFor;
    private hitAt;
    private laneAtY;
    private stubRect;
    private toLocal;
    private onWheel;
    private stepGlide;
    private onPointerDown;
    private onPointerMove;
    private onPointerUp;
    private onPointerLeave;
    private onKeyDown;
    private updateHover;
    private setHover;
    private showTooltip;
    private hideTooltip;
    private defaultTooltip;
    private overrideColor;
    private draw;
    private drawAxisAndGrid;
    private drawLanes;
    private drawCoverage;
    private drawIntervals;
    private drawInterval;
    private drawInstant;
    private drawConnectors;
    private routeVisible;
    private drawMarkers;
    private drawNowLine;
}
