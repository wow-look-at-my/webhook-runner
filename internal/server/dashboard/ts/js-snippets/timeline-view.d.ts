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
 * ones and an explicit end-of-history boundary. Browser navigation
 * gestures never fire over the component: the wheel listener lives on the
 * HOST (horizontal deltas over the DOM chrome are consumed like over the
 * canvas) and the host carries overscroll-behavior: none, so panning hard
 * into exhausted history can't turn into a history-back swipe. A corner
 * ⤢ toggle (always visible; `no-fullscreen-button` hides it) flips the
 * reflected `fullscreen` attribute: viewport-fill via position:fixed —
 * deliberately NOT the Fullscreen API — with the page scroll locked while
 * active, Escape to exit, and a 'fullscreenchange' event. A minimap strip
 * along the bottom (own canvas; hidden with no data, on short hosts, or
 * via `no-minimap`) shows the full loaded extent as per-lane density
 * marks with the viewport as a draggable window: edge handles resize it,
 * grabbing the middle pans it, clicking outside centers it — all through
 * the same follow/park/loadRange semantics as canvas gestures.
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
 * scrolling — no per-element rounding jiggle; TEXT origins are the one
 * per-element exception — they snap to the device grid for crisp glyph
 * rasterization, stepping in whole pixels while things move), bar-vs-pip
 * shapes are decided from data-space durations (never from rounded screen
 * coords, so shapes don't flicker during pans), rows are VERTICALLY
 * STICKY (a stateful per-lane TrackAllocator: a visible interval keeps
 * its sub-track while on screen — panning and live updates never
 * reshuffle the rows being watched — a returning interval remembers its
 * old row, new arrivals fill from the bottom), and lane heights derive
 * from the parallelism visible in the CURRENT window (a historical burst
 * stops padding its lane once off-screen; height changes tween ~150ms,
 * honoring prefers-reduced-motion).
 *
 * Cheap by construction: draws only when dirty (one rAF at a time), a
 * continuous loop runs only while following/animating and the element is
 * visible, and idle animation is paced adaptively — full rate while
 * interacting (plus a short grace window), ~30fps idle, ~10fps idle on
 * battery (feature-detected via navigator.getBattery), paused while the
 * document is hidden; culled to the viewport; DPR-aware (capped at 3), on
 * an OPAQUE canvas (subpixel text AA; keep --timeline-bg opaque).
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
/**
 * What the pointer is over — handed to tooltipFor and hover/click events.
 * 'cluster' (a ×N group of visually-overlapping instant markers) is the
 * one hit type NEVER handed to tooltipFor: its summary tooltip is
 * component-built, and clicking it zooms to the member extent instead of
 * dispatching intervalclick.
 */
export type TimelineHit = {
    type: 'interval';
    interval: TimelineInterval;
    lane: TimelineLane;
} | {
    type: 'cluster';
    intervals: TimelineInterval[];
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
 * A consumer-supplied legend row (`legendEntries`): a short glyph sample —
 * rendered verbatim in the swatch column — plus its plain-language
 * meaning. This is how a consumer teaches the glyphs IT composes into
 * labels (e.g. an adapter's '⧗ group · 3rd' queue badge or '⏳N' holder
 * count) alongside the component's own vocabulary.
 */
export interface TimelineLegendEntry {
    /** The glyph/badge sample (e.g. '⧗', '⏳3'). */
    glyph: string;
    /** What it means. */
    text: string;
}
/**
 * The timeline element. Auto-registered as `<timeline-view>` when this
 * module loads (unless the name is taken). Data arrives via properties and
 * methods — setData / mergeData / setLanes / setIntervals / setConnectors /
 * setMarkers — never attributes; the only attributes are scalar toggles:
 * `no-live-pill` (hide the jump-to-now pill), `no-auto-fit` (disable
 * compact-lane auto-fit), `history-end-text` (boundary label), `empty-text`
 * (empty-state hint), `fullscreen` (reflected viewport-fill mode — see the
 * `fullscreen` property), `no-fullscreen-button` (hide the corner toggle;
 * the property/attribute still work programmatically), `no-minimap` (hide
 * the bottom overview strip), `no-legend` (hide the "?" legend pill —
 * the in-place glyph dictionary; consumers append their own rows via the
 * `legendEntries` property).
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
    private fsEl;
    private emptyEl;
    private staleEl;
    private legendEl;
    private legendPanelEl;
    private legendOpen;
    private userLegend;
    private mmCanvas;
    private mmCtx;
    private mmVisible;
    private mmDrag;
    private hadData;
    private fsLocked;
    private fsPrevOverflow;
    private fsSeenScrollX;
    private fsSeenScrollY;
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
    private hoverClusterId;
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
    private packedPlotW;
    private laneClusters;
    private allocators;
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
    attributeChangedCallback(name: string, oldValue: string | null, newValue: string | null): void;
    /**
     * Viewport-fill mode (NOT the Fullscreen API — deliberately: no
     * permission prompt, no browser chrome transition, plain CSS): the host
     * gets the reflected boolean `fullscreen` attribute and
     * :host([fullscreen]) pins it position:fixed over the whole viewport;
     * the existing ResizeObserver → resizeBackingStore path re-derives
     * everything (layout, clustering, DPR backing store — which stays
     * capped at MAX_DPR: fullscreen must not step off the perf cliff the
     * cap exists for). Toggled by the corner button, this property, or the
     * attribute; Escape exits; 'fullscreenchange' fires on every change.
     */
    get fullscreen(): boolean;
    set fullscreen(v: boolean);
    /** The fullscreen side effects (scroll lock, resize, focus, event) — attribute-change driven. */
    private applyFullscreen;
    /**
     * Page scroll lock: held exactly while CONNECTED && fullscreen. The
     * page behind a viewport-filling chart must not scroll (or scroll-chain
     * from unconsumed wheel deltas). Entering fullscreen collapses the
     * host's slot in the page AND hides the root's overflow — both of which
     * reset/clamp the viewport scroll offset — so the pre-lock offset (the
     * scroll listener's snapshot) is restored on unlock: the page is
     * exactly where the user left it when fullscreen exits.
     */
    private syncScrollLock;
    /** Passive pre-lock scroll snapshot (see fsSeenScrollX) — frozen while locked. */
    private onWinScroll;
    private onDocKeyDown;
    /**
     * Consumer-supplied legend rows, appended under the component-owned
     * vocabulary in the "?" panel — the additive hook for glyphs a consumer
     * composes into its LABELS (queue-position badges, holder counts, …),
     * which the component draws but cannot explain. Entries are copied on
     * set; malformed values are dropped; an open panel re-renders at once.
     */
    get legendEntries(): TimelineLegendEntry[];
    set legendEntries(v: TimelineLegendEntry[]);
    private toggleLegend;
    private closeLegend;
    /** (Re)build the panel rows — only ever runs on open / live entry swap. */
    private buildLegendPanel;
    private legendRow;
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
     * collapses to one track). Rows are STICKY (TrackAllocator, one per
     * lane): a visible interval keeps its track while it stays on screen —
     * visible-membership churn during pans/live updates never reflows the
     * rows being watched — a returning interval remembers its old track,
     * and new arrivals take the lowest conflict-free one, so lane height
     * recovers from the bottom once a tall burst scrolls away. Auto-fit
     * then demotes lanes to the compact track height until the stack fits
     * the host (computeAutoFit — tallest lanes first, hysteretic promotion,
     * a pure function of the visible counts + host height). Count AND
     * height CHANGES ease over LAYOUT_TWEEN_MS (snapped under
     * prefers-reduced-motion). this.layout always reflects the CURRENT
     * (possibly animating) heights, and hit-testing shares it (rectFor
     * reads displayHeights), so hovers stay aligned mid-tween.
     */
    private updateVisibleLayout;
    /** The lane's sticky row allocator (created on first use; pruned with its lane in rebuild). */
    private allocatorFor;
    /**
     * Cluster + row one lane for the current window; returns its visible
     * track count. Instant markers that visually overlap at this scale
     * merge into ×N clusters (clusterInstants — component-native and
     * scale-aware, so zooming in splits them); each cluster then packs as
     * ONE item spanning its member extent, which is what keeps a burst of
     * coincident instants from blowing up the lane height. Rows come from
     * the lane's sticky TrackAllocator; a cluster's packing identity is its
     * FIRST member's id, stable while membership is (pure pans never change
     * membership), so a cluster's row doesn't hop frame to frame. Members
     * ride their cluster's row — hit rects and connector endpoints anchored
     * on a member resolve to the cluster's position.
     */
    private packLane;
    /**
     * A cluster's styling keys: the members' shared state/category where
     * uniform (an all-skipped cluster stays skip-flavored), else the
     * neutral fallbacks — '' (the default treatment) for mixed states, the
     * lane's own color for mixed categories.
     */
    private clusterKeys;
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
     * tiny re-engage zone reliably hittable. Non-zoom interactive gestures
     * keep the pin while following (a forward pan at the stop stays live);
     * ZOOMS (`zoom`) and programmatic setViewport (`jump`) are exempt.
     * Zooms because the ANCHOR must win during the gesture: while pinned,
     * the pin used to rebuild the view from `now` keeping only the zoomed
     * SPAN, so wheel/pinch zoom anchored at the now marker instead of the
     * cursor — a zoom instead re-earns follow like a fresh gesture (it
     * keeps following only when its right edge stays inside the snap zone,
     * so zooming AT the live edge stays live; anywhere else it parks with
     * the timestamp under the cursor still under the cursor, and follow
     * may re-dock magnetically on a later gesture). One asymmetry is
     * deliberate: a zoom-OUT at the live edge still can't show the future —
     * the end stop caps it right-anchored, exactly like a parked zoom-out
     * at the stop.
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
    /**
     * The 2d context — OPAQUE (alpha: false) on purpose: the chart paints
     * its own background every frame, and an opaque canvas lets the engine
     * use subpixel text antialiasing (alpha canvases get grayscale-only) — a
     * real legibility win at 10-11px. Consequence: --timeline-bg must be an
     * opaque color (a translucent bg would composite on black, not on the
     * host).
     */
    private ctx2d;
    /** The minimap strip's 2d context — OPAQUE for the same reasons as ctx2d. */
    private mmCtx2d;
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
    /**
     * The strip's data extent: earliest loaded interval start — widened by
     * coverage knowledge (the first covered time, the exhausted-history
     * boundary) — through max(live edge, latest interval end). Null while
     * nothing is loaded (the strip is hidden then anyway). O(n) over the
     * loaded intervals; called per drawn frame and per strip pointer event,
     * both of which already do O(n) work.
     */
    private mmExtent;
    private mmLocalX;
    private onMMPointerDown;
    private onMMPointerMove;
    private onMMPointerUp;
    private onMMPointerLeave;
    private updateHover;
    private setHover;
    private showTooltip;
    private hideTooltip;
    private defaultTooltip;
    private overrideColor;
    private draw;
    /**
     * The minimap strip: the FULL loaded extent (mmExtent) as per-lane
     * collapsed density marks in category hues at low alpha (no text), the
     * live edge as a now tick, and the current viewport as a brighter
     * window rect with grabbable edge handles. Rendered only from draw() —
     * the strip repaints exactly when the main chart does (same rAF loop,
     * same dirty flag, same idle pacing), never on its own schedule.
     */
    private drawMinimap;
    /**
     * Snap a TEXT draw origin (x or y) to the device-pixel grid. Applied
     * per fillText call — text, unlike bar geometry, tolerates per-element
     * rounding (see snapTextOrigin): a fractional origin — laneScroll
     * accumulation, height tweens, odd track heights — smears every glyph
     * stroke across two pixel rows; a snapped one rasterizes crisp, at the
     * cost of labels stepping in whole device pixels while things move.
     */
    private textPx;
    private drawAxisAndGrid;
    private drawLanes;
    private drawCoverage;
    private drawIntervals;
    private drawInterval;
    private drawInstant;
    /**
     * Screen geometry of a cluster's marker — shared by drawing and hit
     * testing so the two can never disagree. Null while the cluster is
     * unplaced (outside the window) or no part of its extent is visible.
     */
    private clusterPos;
    /**
     * A ×N cluster marker: the SAME diamond pip as a single instant — the
     * ×N count badge alone carries "several instants live here at this
     * zoom". There is no collision to disambiguate (markers merge exactly
     * while they'd visually overlap, so a badged pip can only ever BE a
     * cluster), and a shape switch just made the group look like a foreign
     * glyph. Styled by the members' shared state exactly like singles
     * (all-skipped = dim-filled diamond, all-cancelled = hollow dashed
     * diamond, mixed = the neutral default); sits at the extent midpoint,
     * sliding along the visible slice at a window edge (clusterMarkerTime).
     * Like pips, clusters get no edge-continuation treatment — a point
     * marker has no clipped extent.
     */
    private drawCluster;
    private drawConnectors;
    private routeVisible;
    private drawMarkers;
    private drawNowLine;
}
