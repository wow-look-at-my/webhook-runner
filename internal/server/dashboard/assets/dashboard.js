"use strict";

// -- Push-first sections, fixed-cadence fallback ------------------------------
//
// While timeline.js's /runs/stream EventSource is live (window.whrStreamLive),
// the server pushes coarse `changed` signals naming the admin sections whose
// payloads moved (hooks/images/concurrency/kv/events); timeline.js republishes
// them as whr:sections-changed and each named section refetches ONCE (see the
// section feed below). An idle dashboard therefore makes ZERO polling
// requests. The interval at the bottom is the FALLBACK, exactly like the runs
// feed's: it refreshes only while the stream is down (or timeline.js never
// loaded), on a fixed cadence — never a growing backoff, never gives up.
const FALLBACK_POLL_MS = 5000;
// Signal bursts coalesce: the first signal refetches immediately (leading
// edge), followers within the window fold into one trailing pass — a busy
// server costs at most one refetch per section per window, an idle one zero.
const SECTION_COALESCE_MS = 1000;
// Every fetch is bounded: a fetch that cannot settle must fail, not wedge
// the section feed's single-flight pass (the same AbortSignal.timeout rule
// the runs feed adopted after the stuck-bars post-mortem).
const FETCH_TIMEOUT_MS = 15000;

async function fetchJSON(url) {
  const res = await fetch(url, {
    headers: { Accept: "application/json" },
    signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
  });
  if (!res.ok) throw new Error(`${url}: ${res.status}`);
  return res.json();
}

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else if (k === "data") for (const [dk, dv] of Object.entries(v)) node.dataset[dk] = dv;
      else if (v !== undefined && v !== null) node.setAttribute(k, v);
    }
  }
  for (const c of children) {
    if (c == null) continue;
    if (typeof c === "string") node.appendChild(document.createTextNode(c));
    else node.appendChild(c);
  }
  return node;
}

function fmtTime(s) {
  if (!s) return "";
  const d = new Date(s);
  return d.toLocaleString();
}

// Short wall-clock time (HH:MM:SS) for per-line log timestamps. The run's
// full Started date lives in the meta table, so the per-line stamp only
// needs the time of day.
function fmtClock(s) {
  if (!s) return "";
  const d = new Date(s);
  if (isNaN(d)) return "";
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtDuration(ms) {
  if (ms == null || ms < 0 || isNaN(ms)) return "";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

// Is a timestamp field actually set? A Go zero time can marshal as a
// real-looking "0001-01-01T00:00:00Z", so "present" means parseable AND
// after the epoch — not merely truthy.
function tsPresent(s) {
  if (!s) return false;
  const d = new Date(s);
  return !isNaN(d) && d.getTime() > 0;
}

// Queue wait: accepted (r.started — "queued") until the container launched
// (r.started_at). A pending run ticks live; a run whose launch was never
// recorded (history persisted before the wait/processing split, or a run
// that never started) shows an em-dash.
function runWaited(r) {
  const queued = new Date(r.started);
  if (isNaN(queued)) return "";
  if (tsPresent(r.started_at)) return fmtDuration(new Date(r.started_at) - queued);
  if (r.status === "pending") return fmtDuration(Date.now() - queued) + "…";
  return "—";
}

// Processing time only: container launch (r.started_at) → finish, ticking
// live while running. A pending run has no duration yet. Without a
// started_at, statuses that imply the container ran (legacy history from
// before the split) fall back to the old queued-inclusive span; a
// cancelled/error run may never have started, so it shows an em-dash rather
// than counting queue time as processing.
function runDuration(r) {
  if (tsPresent(r.started_at)) {
    const startedAt = new Date(r.started_at);
    if (tsPresent(r.finished)) return fmtDuration(new Date(r.finished) - startedAt);
    return fmtDuration(Date.now() - startedAt) + "…";
  }
  if (r.status === "pending") return "—";
  const ranStatuses = ["success", "failure", "timeout"];
  if (tsPresent(r.finished) && ranStatuses.includes(r.status)) {
    return fmtDuration(new Date(r.finished) - new Date(r.started));
  }
  return "—";
}

// What a run is currently paused on (r.waiting_on), rendered inline on the
// run row. Kind "wait" is a declared sleep (POST /wait) — "waiting Ns:
// reason", remaining time computed client-side from `until` so it counts
// down on the poll cadence (clamped to 0s when it just elapsed). Kind
// "lock" is a blocked acquire — it names the contended key and exactly who
// holds it (run + hook). Kind "group" is a queued concurrency-group
// acquire — it names the group, the run's place in line, and the holders.
// Unknown future kinds degrade to their reason/kind text, never to silence.
function waitNote(r) {
  const w = r.waiting_on;
  if (!w) return null;
  if (w.kind === "lock") {
    const holder = w.holder_run_id
      ? `${shortId(w.holder_run_id)}${w.holder_hook_id ? ` (${w.holder_hook_id})` : ""}`
      : "unknown";
    return el("span", { class: "wait-note", title: w.holder_run_id || "" },
      `waiting on lock ${w.key} held by ${holder}`);
  }
  if (w.kind === "group") {
    const holders = w.holder_run_ids || [];
    let text = `queued for a slot in group ${w.key || "?"}`;
    if (w.position > 0) text += ` — ${ordinal(w.position)} in line`;
    if (holders.length) text += `, held by ${holders.map(shortId).join(", ")}`;
    return el("span", { class: "wait-note", title: holders.join("\n") }, text);
  }
  if (w.kind === "wait") {
    if (!tsPresent(w.until)) return null;
    const left = new Date(w.until) - Date.now();
    const t = left > 0 ? fmtDuration(left) : "0s";
    return el("span", { class: "wait-note" },
      `waiting ${t}${w.reason ? ": " + w.reason : ""}`);
  }
  return el("span", { class: "wait-note" },
    `waiting${w.reason ? ": " + w.reason : w.kind ? " (" + w.kind + ")" : ""}`);
}

// The modal's "Waiting" row: the same facts as waitNote but with the
// holder run ids as CLICKABLE run links (jumping the modal to the holder),
// which a table-row note can't safely nest inside its row click handler.
function waitDetail(r) {
  const w = r.waiting_on;
  if (!w) return null;
  const frag = document.createDocumentFragment();
  const linkList = (ids) => {
    ids.forEach((id, i) => {
      if (i > 0) frag.appendChild(document.createTextNode(", "));
      frag.appendChild(runLink(id));
    });
  };
  if (w.kind === "lock") {
    frag.appendChild(document.createTextNode(`on lock ${w.key || "?"} held by `));
    if (w.holder_run_id) {
      linkList([w.holder_run_id]);
      if (w.holder_hook_id) frag.appendChild(document.createTextNode(` (${w.holder_hook_id})`));
    } else {
      frag.appendChild(document.createTextNode("unknown"));
    }
    return frag;
  }
  if (w.kind === "group") {
    let lead = `for a slot in group ${w.key || "?"}`;
    if (w.position > 0) lead += ` — ${ordinal(w.position)} in line`;
    const holders = w.holder_run_ids || [];
    frag.appendChild(document.createTextNode(lead + (holders.length ? ", held by " : "")));
    linkList(holders);
    return frag;
  }
  const note = waitNote(r);
  if (!note) return null;
  frag.appendChild(note);
  return frag;
}

// The holder-side note: runs currently blocked on resources THIS run holds
// (r.waiters, derived server-side): cooperative locks (key "k") and
// concurrency-group slots (key "group:g"). The tooltip lists exactly who
// waits on what.
function waitersNote(r) {
  const ws = r.waiters;
  if (!ws || !ws.length) return null;
  return el("span", { class: "wait-note", title: ws.map(waiterText).join("\n") },
    `${ws.length} run${ws.length > 1 ? "s" : ""} waiting on this run`);
}

// The modal's "Held up by this run" row: each waiter a clickable run link
// with its hook and what it waits for.
function waitersDetail(r) {
  const ws = r.waiters;
  if (!ws || !ws.length) return null;
  const frag = document.createDocumentFragment();
  ws.forEach((x, i) => {
    if (i > 0) frag.appendChild(document.createTextNode("; "));
    frag.appendChild(runLink(x.run_id));
    frag.appendChild(document.createTextNode(` (${x.hook_id}) → ${waiterWants(x)}`));
  });
  return frag;
}

// "1st", "2nd", "3rd", "4th", … (11th-13th included) — the queue-position
// vocabulary shared with the timeline's ⧗ badge.
function ordinal(n) {
  const rem = n % 100;
  if (rem >= 11 && rem <= 13) return `${n}th`;
  switch (n % 10) {
    case 1: return `${n}st`;
    case 2: return `${n}nd`;
    case 3: return `${n}rd`;
    default: return `${n}th`;
  }
}

function shortId(id) {
  return id && id.length > 10 ? id.slice(0, 10) + "…" : id || "";
}

// What a waiter entry waits FOR: attachWaiters marks group waits with a
// "group:" key prefix; anything else is a cooperative lock key.
function waiterWants(x) {
  if (!x.key) return "this run";
  if (x.key.startsWith("group:")) return `a slot in group ${x.key.slice(6)}`;
  return `lock ${x.key}`;
}

function waiterText(x) {
  return `${shortId(x.run_id)} (${x.hook_id}) → ${waiterWants(x)}`;
}

// --- Views: the global overview vs the per-app (per-hook) drill-down ------
//
// The fragment #hook=<id> selects the app view; anything else shows the
// overview. Hash routing keeps the page a single embedded document with no
// server-side routes, and hashchange gives back/forward for free.
// An optional &kv=1 suffix (what the overview's State (KV) links append)
// additionally lands the app view scrolled to its State (KV) section.
// Hook IDs are always encodeURIComponent'd into the fragment, so a literal
// "&" in an ID can never be mistaken for the parameter separator.

function currentHookId() {
  const m = location.hash.match(/^#hook=([^&]+)/);
  return m ? decodeURIComponent(m[1]) : null;
}

// The fragment for a hook's app page; kv: true additionally lands it on the
// State (KV) key/value browser (consumed as a one-shot scroll by renderAppKV).
function hookHref(id, opts) {
  return "#hook=" + encodeURIComponent(id) + (opts && opts.kv ? "&kv=1" : "");
}

function hashWantsKV() {
  return /^#hook=[^&]+&kv=1$/.test(location.hash);
}

// One-shot: armed when a navigation lands with &kv=1, consumed by the first
// app render — the poll must never re-scroll a page the operator has since
// scrolled elsewhere.
let pendingKVScroll = hashWantsKV();

function setView(hookId) {
  document.getElementById("overview-view").hidden = !!hookId;
  document.getElementById("app-view").hidden = !hookId;
  document.title = hookId ? `webhook-runner — ${hookId}` : "webhook-runner";
}

// --- Run output: render the model's log as a conversation -----------------
//
// Hooks like pr-describe echo their model I/O into the run log, delimited by
// marker lines:
//   --- model input: system prompt ---
//   --- model input: existing metadata ---
//   --- model input: diff (123 lines) ---
//   --- model output (456 chars) ---
// Joined into one <pre> that's an unreadable wall of text, so we split on
// those markers into role-tagged turns. The hook's own progress/status lines
// (and anything outside a model section) become "log" turns. Output with no
// model markers (a non-conversational hook) returns null -> raw <pre>.

let currentRunLines = [];
let currentRunTimes = [];
let currentRunTurns = null;

function classifyMarker(line) {
  let m = line.match(/^--- model input: (.+?) ---$/);
  if (m) return { role: /^system prompt/.test(m[1]) ? "system" : "user", label: m[1] };
  m = line.match(/^--- model output(?: \((.*)\))? ---$/);
  if (m) return { role: "assistant", label: m[1] ? `model output (${m[1]})` : "model output" };
  return null;
}

// The hook's own log lines (progress, status, retries) that can appear between
// or after model sections -- kept out of the conversation turns.
function isLogLine(line) {
  return (
    /^[^\s/]+\/[^\s/]+#\d+:/.test(line) ||
    /^--- (summarizing part|diff is|\d+ section summaries)/.test(line) ||
    /\battempt \d+ failed\b/.test(line)
  );
}

// entries is an array of { text, time } (time is the line's ISO timestamp or
// null). Turns carry their entries plus the timestamp of their first line so
// the conversation view can show when each turn began.
function parseConversation(entries) {
  if (!entries.some((e) => classifyMarker(e.text))) return null;
  const turns = [];
  let cur = null;
  const pushLog = (e) => {
    const last = turns[turns.length - 1];
    if (last && last.role === "log") last.entries.push(e);
    else turns.push({ role: "log", label: "log", entries: [e], time: e.time });
  };
  for (const e of entries) {
    const marker = classifyMarker(e.text);
    if (marker) {
      cur = { role: marker.role, label: marker.label, entries: [], time: e.time };
      turns.push(cur);
    } else if (isLogLine(e.text)) {
      cur = null;
      pushLog(e);
    } else if (cur) {
      cur.entries.push(e);
    } else {
      pushLog(e);
    }
  }
  return turns;
}

function renderRunOutput(view) {
  const container = document.getElementById("run-detail-output");
  container.innerHTML = "";
  if (view === "conversation" && currentRunTurns) {
    for (const t of currentRunTurns) {
      const body = t.entries.map((e) => e.text).join("\n").replace(/^\n+|\n+$/g, "");
      const clock = fmtClock(t.time);
      container.appendChild(
        el("div", { class: "turn turn-" + t.role },
          el("div", { class: "turn-head" },
            el("span", { class: "role-badge role-" + t.role }, t.role),
            t.label && t.label !== t.role ? el("span", { class: "turn-label" }, t.label) : null,
            clock ? el("span", { class: "turn-time" }, clock) : null,
          ),
          el("pre", { class: "turn-body" }, body || "(empty)"),
        )
      );
    }
  } else if (currentRunLines.length) {
    // Raw view: one row per line, each with its own timestamp column.
    const log = el("div", { class: "raw-log" });
    for (let i = 0; i < currentRunLines.length; i++) {
      log.appendChild(
        el("div", { class: "log-row" },
          el("span", { class: "log-time" }, fmtClock(currentRunTimes[i])),
          el("span", { class: "log-text" }, currentRunLines[i]),
        )
      );
    }
    container.appendChild(log);
  } else {
    container.appendChild(el("pre", { class: "raw-log" }, "(no output)"));
  }
  container.scrollTop = 0;
  for (const b of document.querySelectorAll("#run-detail-view-toggle button")) {
    b.classList.toggle("active", b.dataset.view === view);
  }
}

// -- The section feed ---------------------------------------------------------
//
// One async fetch+render per section. `hooks` also republishes its payload
// for timeline.js (window.whrHooks + whr:hooks-data) — this is the ONLY
// place /hooks is fetched, which fixed the old page-load double-fetch (both
// scripts used to fetch it independently). `runs` is the overview TABLE
// (hidden by default; the timeline is the primary runs view) and fetches
// nothing while hidden. renderKV needs the loaded-hook id set, so the kv
// section reads the roster the hooks section last published.
let lastHookIds = new Set();

function publishHooks(hooks) {
  lastHookIds = new Set(hooks.map((h) => h.id));
  window.whrHooks = hooks;
  window.dispatchEvent(new CustomEvent("whr:hooks-data", { detail: { hooks } }));
}

const sectionFetchers = {
  hooks: async () => {
    const hooks = await fetchJSON("/hooks");
    publishHooks(hooks);
    renderHooks(hooks);
  },
  // The needs-attention surface is GLOBAL: the red banner renders on both
  // views (it lives outside <main>), so this section refetches on its
  // signal whichever view is active — see the push reactor below.
  attention: async () => renderAttention(await fetchJSON("/attention")),
  runs: async () => {
    const runsSection = document.getElementById("runs-section");
    if (!runsSection || runsSection.hidden) return;
    renderRuns(await fetchJSON("/runs?max=50"));
  },
  images: async () => renderImages(await fetchJSON("/images")),
  events: async () => renderEvents(await fetchJSON("/events?max=100")),
  kv: async () => renderKV(await fetchJSON("/kv"), lastHookIds),
  concurrency: async () => renderConcurrency(await fetchJSON("/concurrency")),
  // The hooks-repo reload panel (declared at the bottom of this file;
  // function declarations hoist, so the reference is fine here).
  reload: () => refreshReloadPanel(),
};

function stampUpdated() {
  document.getElementById("updated").textContent =
    "updated " + new Date().toLocaleTimeString();
}

async function refresh() {
  try {
    await fetchJSON("/health");
    setBadge(true);
  } catch {
    setBadge(false);
  }
  const hookId = currentHookId();
  setView(hookId);
  try {
    if (hookId) {
      // The attention banner is global — keep it fresh on the app view too.
      await Promise.all([refreshApp(hookId), sectionFetchers.attention()]);
    } else {
      // hooks first: the kv render keys off the roster it publishes.
      await sectionFetchers.hooks();
      await Promise.all([
        sectionFetchers.attention(),
        sectionFetchers.runs(),
        sectionFetchers.images(),
        sectionFetchers.events(),
        sectionFetchers.kv(),
        sectionFetchers.concurrency(),
        sectionFetchers.reload(),
      ]);
    }
    stampUpdated();
  } catch (e) {
    console.error(e);
  }
}

// -- Push reactor --------------------------------------------------------------
//
// whr:sections-changed marks sections dirty; runSectionWork drains the set —
// leading-edge immediate, bursts coalescing into one trailing pass per
// SECTION_COALESCE_MS — refetching ONLY what changed. On the per-hook app
// view the granular sections collapse into one "app" token (refreshApp is a
// handful of small fetches whose pieces map 1:1 onto the same signals).
// Failed refetches re-mark their sections and retry on the same fixed
// cadence while the stream is live; when it is not, the batch is dropped —
// the fallback poll and the on-reconnect full refresh own recovery then.
const dirtySections = new Set();
const APP_SECTIONS = new Set(["hooks", "events", "kv"]);
let sectionWorkTimer = null;
let sectionWorkRunning = false;
let lastSectionWork = 0;

function scheduleSectionWork() {
  if (sectionWorkTimer !== null || sectionWorkRunning) return;
  const wait = Math.max(0, lastSectionWork + SECTION_COALESCE_MS - Date.now());
  sectionWorkTimer = setTimeout(() => {
    sectionWorkTimer = null;
    void runSectionWork();
  }, wait);
}

async function runSectionWork() {
  if (sectionWorkRunning) return;
  sectionWorkRunning = true;
  lastSectionWork = Date.now();
  const secs = [...dirtySections];
  dirtySections.clear();
  try {
    // Stream down: drop the batch — the fallback poll refreshes everything
    // on its own cadence, and reconnect does a full resync anyway.
    if (window.whrStreamLive !== true) return;
    const hookId = currentHookId();
    if (hookId) {
      // "attention" is the one granular section that renders on the app
      // view too (the global banner); everything else folds into "app".
      if (secs.includes("attention")) await sectionFetchers.attention();
      if (secs.includes("app")) await refreshApp(hookId);
      stampUpdated();
      return;
    }
    if (secs.includes("hooks")) await sectionFetchers.hooks();
    const rest = secs.filter((s) => s !== "hooks" && s !== "app" && sectionFetchers[s]);
    const results = await Promise.allSettled(rest.map((s) => sectionFetchers[s]()));
    results.forEach((r, i) => {
      if (r.status === "rejected") {
        console.error(`section ${rest[i]} refresh failed:`, r.reason);
        dirtySections.add(rest[i]);
      }
    });
    stampUpdated();
  } catch (e) {
    // The hooks fetch (or refreshApp) failed: put the batch back for the
    // fixed-cadence retry.
    console.error("section refresh failed:", e);
    for (const s of secs) dirtySections.add(s);
  } finally {
    sectionWorkRunning = false;
    if (dirtySections.size > 0) scheduleSectionWork();
  }
}

window.addEventListener("whr:sections-changed", (e) => {
  const secs = (e.detail && e.detail.sections) || [];
  const hookId = currentHookId();
  for (const s of secs) {
    if (s === "attention") {
      dirtySections.add("attention"); // global: the banner shows on both views
    } else if (hookId) {
      if (APP_SECTIONS.has(s)) dirtySections.add("app");
    } else if (sectionFetchers[s]) {
      dirtySections.add(s); // unknown future sections are ignored
    }
  }
  if (dirtySections.size > 0) scheduleSectionWork();
});

// Run deltas already stream (timeline.js): they drive the OVERVIEW runs
// table (when shown) and the app view's runs list/stats without any extra
// server-side signal.
window.addEventListener("whr:run-delta", (e) => {
  const hookId = currentHookId();
  const run = e.detail && e.detail.run;
  if (hookId) {
    if (run && run.hook_id === hookId) dirtySections.add("app");
  } else {
    const runsSection = document.getElementById("runs-section");
    if (runsSection && !runsSection.hidden) dirtySections.add("runs");
  }
  if (dirtySections.size > 0) scheduleSectionWork();
});

// A just-revealed runs table starts stale (hidden tables fetch nothing).
window.addEventListener("whr:runs-table-shown", () => {
  if (currentHookId()) return;
  dirtySections.add("runs");
  scheduleSectionWork();
});

// Stream recovery: signals missed while down are unknowable — do ONE full
// refresh per (re)connect, then go push-only again. A live stream is also
// proof of server contact, so the badge flips healthy without a /health
// round trip; while the stream is down the fallback poll's /health probe
// owns the badge.
window.addEventListener("whr:stream-state", (e) => {
  if (!e.detail || !e.detail.live) return;
  setBadge(true);
  dirtySections.clear();
  refresh();
});

function setBadge(ok) {
  const b = document.getElementById("health-badge");
  b.textContent = ok ? "healthy" : "unhealthy";
  b.classList.toggle("ok", ok);
  b.classList.toggle("bad", !ok);
}

// The endpoint a hook is triggered on, shown as a path. Deliberately NOT a
// full URL: this dashboard is served on the ADMIN port/hostname, while hooks
// are served on the separate hook port -- a URL built from location.origin
// looks copyable but points at the wrong host. The path is the part we know;
// .copyable (user-select: all) keeps it one-click selectable.
function triggerPath(id) {
  return [
    el("code", { class: "copyable" }, `/hook/${id}`),
    " ",
    el("span", { class: "port-note" }, "(on the hook port)"),
  ];
}

// --- Operator kill switch --------------------------------------------------
//
// The big red switch: a disabled hook stays loaded but every delivery is
// rejected (503) and scheduled runs are skipped, until re-enabled. Flips
// persist server-side (survive restarts AND hooks-repo reloads), so this
// is the way to stop a runaway hook — no config PR, no repo surgery.

async function toggleHook(id, disable) {
  if (disable && !confirm(`Disable hook ${id}? Deliveries will be rejected.`)) return;
  try {
    const res = await fetch(
      `/hooks/${encodeURIComponent(id)}/${disable ? "disable" : "enable"}`,
      { method: "POST" });
    if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  } catch (err) {
    alert(`Failed to ${disable ? "disable" : "enable"} hook ${id}: ${err.message}`);
  }
  refresh();
}

// ONE control is both the state display and the flip: a slider switch —
// on/green = enabled, off/grey = disabled (replacing the old status pill +
// Enable/Disable button pair). The switch shows SERVER state only: the
// change handler reverts the click's visual flip and lets toggleHook()'s
// refresh move it, so a cancelled confirm or a failed POST leaves the
// switch where the server is.
function hookSwitch(id, disabled) {
  const input = el("input", {
    type: "checkbox",
    role: "switch",
    "aria-label": `Enable hook ${id}`,
  });
  input.checked = !disabled;
  input.addEventListener("change", () => {
    const disable = !input.checked; // the flip the click asked for
    input.checked = disable;        // back to the pre-click (server) state
    toggleHook(id, disable);
  });
  const sw = el("label", {
    class: "switch",
    title: disabled
      ? `Re-enable ${id}: accept deliveries and scheduled runs again`
      : `Disable ${id}: reject deliveries (503) and skip scheduled runs`,
  }, input, el("span", { class: "switch-slider" }));
  sw.addEventListener("click", (e) => e.stopPropagation());
  return sw;
}

function renderHooks(hooks) {
  const tbody = document.querySelector("#hooks-table tbody");
  tbody.innerHTML = "";
  document.getElementById("hooks-empty").hidden = hooks.length > 0;
  for (const h of hooks) {
    tbody.appendChild(
      el("tr", null,
        // Each hook is an "app": its ID links to the per-hook drill-down.
        el("td", null,
          el("a", { href: hookHref(h.id), class: "hook-link" },
            el("code", null, h.id))),
        el("td", null, h.description || ""),
        el("td", null, (h.synchronous ? "sync" : "async") + (h.schedule ? ` · every ${h.schedule}` : "")),
        el("td", { class: "row-actions" }, hookSwitch(h.id, h.disabled)),
        el("td", null, ...triggerPath(h.id)),
      )
    );
  }
}

// --- Needs attention: the misconfiguration cry-for-help ---------------------
//
// GET /attention is the aggregated set of ACTIVE problems (dropped hooks,
// unresolvable ${NAME} references, sops decrypt failures, the zero-hooks
// guard, the containerized-TMPDIR hazard, recognized event-derived
// problems). While any are active a red banner pins itself above BOTH
// views and the overview grows a "Needs attention" panel listing each
// problem with its hook and how long it has been active. Entries
// self-clear server-side when the underlying state resolves (typically on
// the reload that fixes it), so a healthy server shows neither.

// Where an entry derives from, for the panel's Source column.
function attentionSourceLabel(source) {
  switch (source) {
    case "load": return "config (hook dropped)";
    case "zero-hooks": return "config (no hooks)";
    case "secrets": return "secrets / references";
    case "server": return "server (needs restart)";
    case "event": return "runtime event";
    default: return source;
  }
}

// One-shot smooth-scroll to the panel, armed by the banner's "view" link
// when it has to route back to the overview first (the pendingKVScroll
// pattern: consumed by the render, never re-fired by a later refetch).
let pendingAttentionScroll = false;

function renderAttention(data) {
  const entries = (data && data.entries) || [];
  const banner = document.getElementById("attention-banner");
  banner.hidden = entries.length === 0;
  document.getElementById("attention-banner-text").textContent =
    entries.length === 1
      ? "1 problem needs attention"
      : `${entries.length} problems need attention`;

  const section = document.getElementById("attention-section");
  section.hidden = entries.length === 0;
  const tbody = document.querySelector("#attention-table tbody");
  tbody.innerHTML = "";
  for (const e of entries) {
    const tr = el("tr", { class: e.hook ? "attention-hook-row" : "" },
      el("td", { class: "attention-msg" }, e.message),
      el("td", null, e.hook
        ? el("a", { href: hookHref(e.hook), class: "hook-link" }, el("code", null, e.hook))
        : "—"),
      el("td", null, attentionSourceLabel(e.source)),
      // Age since the problem FIRST became active (stable across
      // re-derivations while it persists); tooltip = the absolute time.
      el("td", { class: "attention-age", title: fmtTime(e.since) },
        fmtDuration(Date.now() - new Date(e.since)) || "0s"),
    );
    if (e.hook) {
      tr.addEventListener("click", (ev) => {
        if (ev.target.closest("a")) return;
        location.hash = hookHref(e.hook);
      });
    }
    tbody.appendChild(tr);
  }
  if (pendingAttentionScroll && !currentHookId()) {
    pendingAttentionScroll = false;
    if (entries.length) section.scrollIntoView({ behavior: "smooth", block: "start" });
  }
}

// The banner's "view" jump: on the overview, scroll straight to the panel;
// on an app page, route back to the overview first and let its render
// consume the one-shot scroll.
document.getElementById("attention-banner-link").addEventListener("click", (e) => {
  e.preventDefault();
  if (currentHookId()) {
    pendingAttentionScroll = true;
    location.hash = "";
  } else {
    const section = document.getElementById("attention-section");
    if (!section.hidden) section.scrollIntoView({ behavior: "smooth", block: "start" });
  }
});

// --- Concurrency groups: declared vs effective + live override -------------

async function overrideLimit(name, declared, current) {
  const v = prompt(
    `Override the concurrency limit for group "${name}" (declared ${declared}).\n` +
    "Must be an integer >= 1 — to stop the group's hooks entirely, disable the hooks instead.",
    String(current)
  );
  if (v == null) return;
  const n = Number(v.trim());
  if (!Number.isInteger(n) || n < 1) {
    alert("Limit must be an integer >= 1 (a 0 limit would deadlock queued runs).");
    return;
  }
  try {
    const res = await fetch(`/concurrency/${encodeURIComponent(name)}/limit`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ limit: n }),
    });
    if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  } catch (err) {
    alert(`Failed to override group ${name}: ${err.message}`);
  }
  refresh();
}

async function clearLimitOverride(name, declared) {
  if (!confirm(`Revert group "${name}" to its declared limit (${declared})?`)) return;
  try {
    const res = await fetch(`/concurrency/${encodeURIComponent(name)}/limit`, { method: "DELETE" });
    if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  } catch (err) {
    alert(`Failed to revert group ${name}: ${err.message}`);
  }
  refresh();
}

// Which concurrency group's drill-down row is expanded (persists across the
// poll's re-render, same pattern as appKVOpenKey).
let concurrencyOpenGroup = null;

function renderConcurrency(groups) {
  const tbody = document.querySelector("#concurrency-table tbody");
  tbody.innerHTML = "";
  groups = groups || [];
  document.getElementById("concurrency-empty").hidden = groups.length > 0;
  for (const g of groups) {
    const editBtn = el("button", {
      class: "toggle-btn",
      title: `Override the limit for ${g.name} live (persists across reloads/restarts until reverted)`,
    }, "Override…");
    editBtn.addEventListener("click", () => overrideLimit(g.name, g.declared, g.limit));
    const actions = el("td", { class: "row-actions" }, editBtn);
    if (g.overridden) {
      const revertBtn = el("button", { class: "toggle-btn", title: `Clear the override; the declared limit (${g.declared}) takes effect` }, "Revert");
      revertBtn.addEventListener("click", () => clearLimitOverride(g.name, g.declared));
      actions.appendChild(revertBtn);
    }
    const open = concurrencyOpenGroup === g.name;
    const tr = el("tr", { class: open ? "group-open" : "",
      title: "click to see which runs hold this group's slots and which are queued" },
      el("td", null, el("code", null, g.name)),
      el("td", null, String(g.declared)),
      el("td", null,
        String(g.limit),
        g.overridden ? el("span", { class: "badge warn" }, "overridden") : null,
      ),
      el("td", null, String(g.active)),
      el("td", null, String(g.waiting)),
      actions,
    );
    // The whole row toggles the drill-down; the override buttons keep
    // their own clicks.
    tr.addEventListener("click", (e) => {
      if (e.target.closest("button")) return;
      concurrencyOpenGroup = open ? null : g.name;
      refresh();
    });
    tbody.appendChild(tr);
    if (open) tbody.appendChild(groupDetailRow(g));
  }
}

// The expanded row under a group: which runs hold its slots and which are
// queued, in order — each a run link into the run modal. This is the
// operator's self-serve answer to "the group reads 3/3 with 4 waiting;
// WHAT is holding the slots?".
function groupDetailRow(g) {
  const td = el("td", { colspan: "6" });
  const row = el("tr", { class: "group-detail-row" }, td);
  const runLine = (r, note) => el("div", { class: "group-run" },
    runLink(r.run_id),
    " ",
    r.hook_id ? el("code", null, r.hook_id) : null,
    r.title ? el("span", { class: "group-run-title" }, " " + r.title) : null,
    r.status ? el("span", { class: "status " + r.status, style: "margin-left: 0.4rem" }, r.status) : null,
    el("span", { class: "group-run-since" }, note),
  );
  const holders = g.holders || [];
  const waiting = g.waiting_runs || [];
  if (!holders.length && !waiting.length) {
    td.appendChild(el("div", { class: "group-detail-head" }, "No runs holding or waiting."));
    return row;
  }
  if (holders.length) {
    td.appendChild(el("div", { class: "group-detail-head" },
      `Holding ${holders.length === 1 ? "the slot" : holders.length + " slots"}:`));
    for (const h of holders) {
      td.appendChild(runLine(h, ` — holding for ${fmtDuration(Date.now() - new Date(h.since)) || "0s"}`));
    }
  }
  if (waiting.length) {
    td.appendChild(el("div", { class: "group-detail-head" }, `Waiting (${waiting.length}, in queue order):`));
    waiting.forEach((r, i) => {
      td.appendChild(runLine(r, ` — #${i + 1} in line, waiting ${fmtDuration(Date.now() - new Date(r.since)) || "0s"}`));
    });
  }
  return row;
}

// Run cell for the tables: the friendly title (feature-detected — an
// additive /runs field older servers simply don't send) leads when present,
// with the full run id demoted to a small muted second line; untitled runs
// keep the plain id code exactly as before.
function runCell(r) {
  const code = el("code", null, r.id);
  if (!r.title) return el("td", null, code);
  return el("td", null,
    el("div", { class: "run-title" }, r.title),
    el("div", { class: "run-id-sub" }, code),
  );
}

function renderRuns(rs) {
  const tbody = document.querySelector("#runs-table tbody");
  tbody.innerHTML = "";
  document.getElementById("runs-empty").hidden = rs.length > 0;
  for (const r of rs) {
    const tr = el("tr", { data: { runId: r.id } },
      el("td", null, fmtTime(r.started)),
      el("td", null, el("code", null, r.hook_id)),
      el("td", { class: "status " + r.status }, r.status, waitNote(r), waitersNote(r)),
      el("td", null, String(r.exit_code)),
      runCell(r),
    );
    tr.addEventListener("click", () => showRun(r.id));
    tbody.appendChild(tr);
  }
}

function renderImages(images) {
  const tbody = document.querySelector("#images-table tbody");
  tbody.innerHTML = "";
  document.getElementById("images-empty").hidden = images.length > 0;
  for (const im of images) {
    let state;
    if (im.error) state = el("span", { class: "badge bad" }, "error: " + im.error);
    else if (im.built) state = el("span", { class: "badge ok" }, "built");
    else state = el("span", { class: "badge warn" }, "will build on next run");
    const others = (im.images || [])
      .map((i) => `${i.tag.split(":").pop()} (${i.size}, ${i.created})${i.current ? " *" : ""}`)
      .join(", ");
    tbody.appendChild(
      el("tr", null,
        el("td", null, el("code", null, im.hook_id)),
        el("td", null, el("code", null, im.tag || "-")),
        el("td", null, state),
        el("td", { class: "images-on-disk" }, others || "none"),
      )
    );
  }
}

// Fills an events table body; shared by the overview feed and the per-app
// slice (same columns, different tables). events may be null (nil recorder).
function renderEventRows(tableId, emptyId, events) {
  const tbody = document.querySelector(`#${tableId} tbody`);
  tbody.innerHTML = "";
  events = events || [];
  document.getElementById(emptyId).hidden = events.length > 0;
  for (const ev of events) {
    tbody.appendChild(
      el("tr", null,
        el("td", { class: "event-time" }, fmtTime(ev.time)),
        el("td", null, el("span", { class: "kind " + ev.kind.replace(/\./g, "-") }, ev.kind)),
        el("td", null, ev.msg),
      )
    );
  }
}

function renderEvents(events) {
  renderEventRows("events-table", "events-empty", events);
}

function fmtBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

function renderKV(namespaces, loadedHookIDs) {
  const tbody = document.querySelector("#kv-table tbody");
  tbody.innerHTML = "";
  document.getElementById("kv-empty").hidden = namespaces.length > 0;
  for (const ns of namespaces) {
    // Namespace == hook ID, so each row links to that hook's app page,
    // landed on its State (KV) key/value browser. A namespace whose hook is
    // no longer loaded (leftover state from a removed/renamed hook) links
    // to the SAME place: the app page detects the orphan and still renders
    // the key/value browser — stored data must always be inspectable.
    const orphan = !loadedHookIDs.has(ns.namespace);
    const href = hookHref(ns.namespace, { kv: true });
    const title = orphan
      ? "no loaded hook with this ID — browse the namespace's stored keys and values"
      : "browse this hook's stored keys and values";
    const link = el("a", { href, class: "hook-link", title },
      el("code", null, ns.namespace),
      orphan ? el("span", { class: "badge warn" }, "orphaned") : null,
    );
    const tr = el("tr", null,
      el("td", null, link),
      el("td", null, String(ns.keys)),
      el("td", null, fmtBytes(ns.bytes)),
    );
    // The whole row is the click target (the pointer cursor promises it).
    tr.addEventListener("click", (e) => {
      if (e.target.closest("a")) return; // let the real link handle itself
      location.hash = href;
    });
    tbody.appendChild(tr);
  }
}

// --- Per-app view (app == one hook) ----------------------------------------

async function refreshApp(id) {
  const enc = encodeURIComponent(id);
  let detail;
  try {
    detail = await fetchJSON(`/hooks/${enc}`);
  } catch {
    // 404: not loaded (deleted, or a stale link). Anything else transient
    // lands here too; the next poll retries. Before giving up, check the
    // state store: a removed/renamed hook's namespace can outlive it, and
    // its keys/values must stay inspectable (the overview's State (KV)
    // table links here for exactly that case).
    try {
      const listing = await fetchJSON(`/kv/${enc}`);
      if (listing && listing.keys && listing.keys.length > 0) {
        await renderAppOrphanKV(id, listing);
        return;
      }
    } catch {
      /* no state store / transient failure: fall through to plain missing */
    }
    renderAppMissing(id);
    return;
  }
  const [runs, events, kvKeys] = await Promise.all([
    fetchJSON(`/runs?hook=${enc}&max=50`),
    fetchJSON(`/events?hook=${enc}&max=100`),
    // The KV namespace listing exists only for state:true hooks
    // (namespace == hook ID); skip the fetch entirely otherwise.
    detail.info.state ? fetchJSON(`/kv/${enc}`) : Promise.resolve(null),
  ]);
  renderApp(detail, runs, events);
  await renderAppKV(detail.info, kvKeys);
}

function renderAppMissing(id) {
  document.getElementById("app-title").textContent = id;
  document.getElementById("app-desc").textContent = "";
  const missing = document.getElementById("app-missing");
  missing.textContent = "No such hook.";
  missing.hidden = false;
  document.getElementById("app-body").hidden = true;
  document.getElementById("app-switch").hidden = true;
}

// The app page for a namespace whose hook is gone (orphaned state): the
// hook sections stay hidden, but the State (KV) key/value browser renders
// exactly as it would for a live state hook — leftover data is the case
// the operator most needs to inspect, not the one to hide.
async function renderAppOrphanKV(id, listing) {
  renderAppMissing(id);
  const missing = document.getElementById("app-missing");
  missing.textContent =
    "No such hook is loaded — showing this namespace's stored state (orphaned; left by a removed or renamed hook).";
  document.getElementById("app-body").hidden = false;
  setAppOrphanMode(true);
  await renderAppKV({ id, state: true }, listing);
}

// Orphan mode hides every app-body section that needs a loaded hook,
// leaving only the State (KV) browser; renderApp flips it back off.
function setAppOrphanMode(orphan) {
  document.querySelector("#app-body .app-cards").hidden = orphan;
  document.getElementById("app-runs-section").hidden = orphan;
  document.getElementById("app-events-section").hidden = orphan;
}

function fillDl(dl, rows) {
  dl.innerHTML = "";
  for (const [k, v] of rows) {
    dl.appendChild(el("dt", null, k));
    dl.appendChild(el("dd", null, ...(Array.isArray(v) ? v : [v])));
  }
}

function renderApp(detail, runs, events) {
  const info = detail.info;
  document.getElementById("app-missing").hidden = true;
  document.getElementById("app-body").hidden = false;
  setAppOrphanMode(false);
  document.getElementById("app-title").textContent = info.id;
  document.getElementById("app-desc").textContent = info.description || "";

  // Operator kill switch for this hook: the same single switch as the
  // overview's Status column, next to the title.
  const switchSlot = document.getElementById("app-switch");
  switchSlot.hidden = false;
  switchSlot.replaceChildren(hookSwitch(info.id, detail.disabled));

  fillDl(document.getElementById("app-info"), [
    ["Trigger path", triggerPath(info.id)],
    ["Operator switch", detail.disabled
      ? el("span", { class: "status failure" }, "DISABLED — deliveries rejected (503), scheduled runs skipped")
      : "enabled"],
    ["Mode", info.synchronous ? "sync" : "async"],
    ["Schedule", info.schedule ? `every ${info.schedule}` : "—"],
    ["Concurrency group", info.concurrency_group ? el("code", null, info.concurrency_group) : "—"],
    ["Timeout", `${info.timeout} without output`],
    ["Skip conditions", info.skip_conditions
      ? `${info.skip_conditions} — matched deliveries answer without a container`
      : "none"],
    ["API key", info.api_key ? "configured" : "none"],
    ["Env vars", info.env_keys && info.env_keys.length
      ? el("span", { class: "chips" }, ...info.env_keys.map((k) => el("code", null, k)))
      : "none"],
    ["State (KV)", !info.state ? "off"
      : kvSectionLink(detail.kv
        ? `on — ${detail.kv.keys} key(s), ${fmtBytes(detail.kv.bytes)}`
        : "on — no data yet")],
  ]);

  const st = detail.stats;
  document.getElementById("app-stats-window").textContent = st.retention
    ? `Window: live runs plus completed runs persisted for the last ${st.retention} (survives restarts; runs in flight during a restart are lost).`
    : `Recent window: the last ≤${st.max_tracked} runs held in memory (resets on restart).`;
  const byStatus = Object.entries(st.by_status || {}).map(([k, n]) =>
    el("span", { class: "status " + k }, `${k} ×${n}`));
  fillDl(document.getElementById("app-stats"), [
    ["Runs tracked", String(st.tracked)],
    ["By status", byStatus.length ? el("span", { class: "chips" }, ...byStatus) : "—"],
    ["Success rate", st.completed ? `${Math.round(st.success_rate * 100)}% of ${st.completed} completed` : "—"],
    // Skips are their own bucket: no container ran, so they are excluded
    // from the completed count, the success rate, and every duration/wait
    // figure — counting non-work would dilute all of them.
    ["Skipped", st.skipped
      ? el("span", { class: "status skipped" }, `${st.skipped} (no container — excluded from the figures above)`)
      : "—"],
    // Durations are processing-only (container launch → finish); queue wait
    // is its own pair of figures. wait_sampled counts the completed runs
    // that recorded a launch time — 0 means no wait data (e.g. only history
    // from before the split), not a zero wait.
    ["Avg duration", st.completed ? fmtDuration(st.avg_duration_ms) : "—"],
    ["Max duration", st.completed ? fmtDuration(st.max_duration_ms) : "—"],
    ["Avg wait", st.wait_sampled ? fmtDuration(st.avg_wait_ms) : "—"],
    ["Max wait", st.wait_sampled ? fmtDuration(st.max_wait_ms) : "—"],
    ["Last run", st.last_run
      ? [
          el("span", { class: "status " + st.last_run.status }, st.last_run.status),
          " at " + fmtTime(st.last_run.started) + " — ",
          runLink(st.last_run.id),
        ]
      : "—"],
  ]);

  const im = detail.image;
  let state;
  if (im.error) state = el("span", { class: "badge bad" }, "error: " + im.error);
  else if (im.built) state = el("span", { class: "badge ok" }, "built");
  else state = el("span", { class: "badge warn" }, "will build on next run");
  const others = (im.images || [])
    .map((i) => `${i.tag.split(":").pop()} (${i.size}, ${i.created})${i.current ? " *" : ""}`)
    .join(", ");
  fillDl(document.getElementById("app-image"), [
    ["Current tag", el("code", null, im.tag || "-")],
    ["State", state],
    ["On disk", others || "none"],
  ]);

  const tbody = document.querySelector("#app-runs-table tbody");
  tbody.innerHTML = "";
  document.getElementById("app-runs-empty").hidden = runs.length > 0;
  for (const r of runs) {
    // Queued = accepted; Waited = queue time until launch (live for pending
    // runs); Duration = processing only (live while running).
    const tr = el("tr", null,
      el("td", null, fmtTime(r.started)),
      el("td", { class: "status " + r.status }, r.status, waitNote(r), waitersNote(r)),
      el("td", null, runWaited(r)),
      el("td", null, runDuration(r)),
      el("td", null, String(r.exit_code)),
      runCell(r),
    );
    tr.addEventListener("click", () => showRun(r.id));
    tbody.appendChild(tr);
  }

  renderEventRows("app-events-table", "app-events-empty", events);
}

// --- Per-app State (KV) inspection -----------------------------------------
//
// For state:true hooks the app page lists the hook's KV keys (name, size,
// TTL remaining) from GET /kv/{namespace} and, on click, fetches
// GET /kv/{namespace}/{key} to show the stored value — pretty-printed when
// it parses as JSON, raw text otherwise, base64 for binary. The open value
// row is re-fetched on every poll so it tracks live state; everything is
// rendered via el()'s text nodes, so arbitrary stored bytes can never
// inject markup.

let appKVOpenKey = null; // key whose value row is expanded, or null
let appKVHook = null; // which hook the expansion belongs to

function fmtTTL(seconds) {
  if (seconds == null) return "—";
  return fmtDuration(seconds * 1000);
}

// The Info card's State (KV) summary line jumps down to the key/value
// browser. A click handler rather than a real fragment href: an
// "#app-kv-section" href would replace the #hook= fragment and route
// back to the overview.
function kvSectionLink(text) {
  const a = el("a", { href: "#", class: "kv-jump", title: "view stored keys and values below" }, text);
  a.addEventListener("click", (e) => {
    e.preventDefault();
    document.getElementById("app-kv-section").scrollIntoView({ behavior: "smooth", block: "start" });
  });
  return a;
}

async function renderAppKV(info, listing) {
  const section = document.getElementById("app-kv-section");
  section.hidden = !info.state;
  // Consume the one-shot &kv=1 landing scroll even for a state-less hook,
  // so it can never fire on a later poll of some other page.
  const wantScroll = pendingKVScroll;
  pendingKVScroll = false;
  if (!info.state) return;
  if (appKVHook !== info.id) {
    // Switched to a different hook's page: collapse any open value row.
    appKVHook = info.id;
    appKVOpenKey = null;
  }
  const keys = (listing && listing.keys) || [];
  const tbody = document.querySelector("#app-kv-table tbody");
  tbody.innerHTML = "";
  document.getElementById("app-kv-empty").hidden = keys.length > 0;
  for (const k of keys) {
    const open = appKVOpenKey === k.key;
    const tr = el("tr", { class: open ? "kv-open" : "" },
      el("td", null, el("code", null, k.key)),
      el("td", null, fmtBytes(k.size)),
      el("td", null, fmtTTL(k.ttl_seconds)),
    );
    tr.addEventListener("click", () => {
      appKVOpenKey = open ? null : k.key;
      refresh();
    });
    tbody.appendChild(tr);
    if (open) tbody.appendChild(await kvValueRow(info.id, k.key));
  }
  if (wantScroll) section.scrollIntoView({ behavior: "smooth", block: "start" });
}

// The expanded row under a clicked key: metadata line + the value itself.
// A fetch failure is rendered into the row (e.g. the key expired between
// the listing and the click), never swallowed.
async function kvValueRow(hookId, key) {
  const td = el("td", { colspan: "3" });
  const row = el("tr", { class: "kv-value-row" }, td);
  try {
    const e = await fetchJSON(
      `/kv/${encodeURIComponent(hookId)}/${encodeURIComponent(key)}`);
    const meta = [fmtBytes(e.size)];
    if (e.expires_at) meta.push(`expires ${fmtTime(e.expires_at)} (in ${fmtTTL(e.ttl_seconds)})`);
    let body;
    if (e.value_utf8 != null) {
      body = e.value_utf8;
      try {
        body = JSON.stringify(JSON.parse(e.value_utf8), null, 2);
        meta.push("JSON");
      } catch {
        meta.push("text"); // valid UTF-8 but not JSON: show it verbatim
      }
    } else {
      body = e.value_base64;
      meta.push("binary (shown base64)");
    }
    td.appendChild(el("div", { class: "kv-value-meta" }, meta.join(" · ")));
    td.appendChild(el("pre", { class: "kv-value" }, body === "" ? "(empty value)" : body));
  } catch (err) {
    td.appendChild(el("div", { class: "kv-value-meta kv-value-error" },
      `failed to load value: ${err.message}`));
  }
  return row;
}

// A run ID that opens the same output modal the runs tables use.
function runLink(id) {
  const a = el("a", { href: "#", class: "run-link" }, el("code", null, id));
  a.addEventListener("click", (e) => {
    e.preventDefault();
    showRun(id);
  });
  return a;
}

// --- Run-detail modal: live view of one run -------------------------------
//
// The modal is LIVE while open: a fixed 3s poll of /runs/{id} re-renders
// meta and output in place — no close/reopen to see a status change, a new
// output line, or a cancel taking effect. The poll is deliberately simple
// (fixed cadence, no backoff, structurally cannot stop itself): it runs
// whenever the dialog is open for a non-terminal run and stops only when
// the dialog closes or the run reaches a terminal status (whose render IS
// the final state — a terminal run never changes again). A later PR
// upgrades the refresh trigger to server-push deltas (/runs/stream);
// polling stays as its fallback.

const TERMINAL_RUN_STATUSES = ["success", "failure", "timeout", "error", "cancelled", "skipped"];
const RUN_DETAIL_POLL_MS = 3000;

let currentRunId = null; // run shown in the open modal, null when closed
let currentRunView = null; // the user's raw/conversation choice, null = auto
let currentRunTerminal = false; // last rendered status was terminal

async function showRun(id) {
  currentRunId = id;
  currentRunView = null;
  currentRunTerminal = false;
  await refreshRunDetail(true);
}

// Re-fetches the modal's run and re-renders in place. openDialog is true on
// the initial open only (shows the dialog, resets output scroll); refreshes
// preserve scroll position and the raw/conversation toggle choice.
async function refreshRunDetail(openDialog) {
  const id = currentRunId;
  if (!id) return;
  try {
    const r = await fetchJSON(`/runs/${id}`);
    if (currentRunId !== id) return; // modal moved on while fetching
    renderRunDetail(r, openDialog);
  } catch (e) {
    // Transient failure: keep showing the last rendered state; the next
    // poll (or delta) retries. Never blank an open modal over one error.
    console.error("run detail refresh:", e);
  }
}

function renderRunDetail(r, openDialog) {
  currentRunTerminal = TERMINAL_RUN_STATUSES.includes(r.status);
  // Title primary when present ("wow-look-at-my/go-toolchain#47"), the
  // generic "Run" word otherwise; the full id always sits beside it in
  // the (small, muted) code chip.
  document.getElementById("run-detail-name").textContent = r.title || "Run";
  document.getElementById("run-detail-id").textContent = r.id;
  const dl = document.getElementById("run-detail-meta");
  dl.innerHTML = "";
  // Queued→Started is the concurrency-group wait; Started→Finished is the
  // actual container time — kept separate so a long queue never reads as
  // a slow run.
  const rows = [
    ["Hook", r.hook_id],
    ["Status", el("span", { class: "status " + r.status }, r.status,
      r.cancel_requested && !currentRunTerminal ? el("span", { class: "wait-note" }, "cancel requested…") : null)],
    ["Exit code", String(r.exit_code)],
    ["Queued", fmtTime(r.started)],
    ["Started", tsPresent(r.started_at) ? fmtTime(r.started_at) : "—"],
    ["Finished", tsPresent(r.finished) ? fmtTime(r.finished) : "—"],
    ["Waited", runWaited(r)],
    ["Duration", runDuration(r)],
  ];
  // A live pause gets its own row, whatever its kind: a declared sleep, a
  // blocked lock acquire (holder linked), or a concurrency-group queue
  // wait (position + holders linked).
  const wd = waitDetail(r);
  if (wd) rows.push(["Waiting", wd]);
  // The holder-side view: who is blocked on locks or group slots this run
  // holds, each waiter a clickable run link.
  const wds = waitersDetail(r);
  if (wds) rows.push(["Held up by this run", wds]);
  if (r.error) rows.push(["Error", r.error]);
  for (const [k, v] of rows) {
    dl.appendChild(el("dt", null, k));
    dl.appendChild(el("dd", null, v));
  }
  // Output: preserve the reading position across refreshes — restore the
  // scroll offset, or stay pinned to the bottom when the operator was
  // tailing the end (renderRunOutput itself resets to the top).
  const out = document.getElementById("run-detail-output");
  const atBottom = out.scrollTop + out.clientHeight >= out.scrollHeight - 4;
  const prevScroll = out.scrollTop;
  currentRunLines = r.output || [];
  currentRunTimes = r.output_times || [];
  const entries = currentRunLines.map((text, i) => ({ text, time: currentRunTimes[i] }));
  currentRunTurns = parseConversation(entries);
  document.getElementById("run-detail-view-toggle").hidden = !currentRunTurns;
  document.getElementById("run-detail-copy").disabled = currentRunLines.length === 0;
  let view = currentRunView || (currentRunTurns ? "conversation" : "raw");
  if (view === "conversation" && !currentRunTurns) view = "raw"; // choice kept, content can't honor it
  renderRunOutput(view);
  if (!openDialog) out.scrollTop = atBottom ? out.scrollHeight : prevScroll;
  const dlg = document.getElementById("run-detail");
  if (openDialog && !dlg.open) dlg.showModal();
}

const runDetailDialog = document.getElementById("run-detail");
document.getElementById("run-detail-close").addEventListener("click", () => {
  runDetailDialog.close();
});
// Click outside the modal box (on the backdrop) closes it; Escape already does.
runDetailDialog.addEventListener("click", (e) => {
  if (e.target === runDetailDialog) runDetailDialog.close();
});
// Closing the dialog (button, backdrop, Escape — all paths fire "close")
// stops the live refresh.
runDetailDialog.addEventListener("close", () => {
  currentRunId = null;
});
// Switch between the conversation and raw-log views of the same run output.
// The choice persists across live refreshes until the modal is reopened.
document.getElementById("run-detail-view-toggle").addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-view]");
  if (btn) {
    currentRunView = btn.dataset.view;
    renderRunOutput(btn.dataset.view);
  }
});
// The live refresh loop: one fixed-cadence interval for the page's life,
// gated on "modal open, run known, not yet rendered terminal". A terminal
// render is final — the run cannot change — so polling stops there; errors
// inside refreshRunDetail are caught (the loop itself can never die).
// PUSH-FIRST: while timeline.js's /runs/stream EventSource is live
// (window.whrStreamLive), deltas drive the refresh at push latency and
// this poll stands down; it takes over automatically whenever the stream
// is down (or the timeline module never loaded — whrStreamLive undefined).
setInterval(() => {
  if (!currentRunId || currentRunTerminal) return;
  if (!runDetailDialog.open) return;
  if (window.whrStreamLive === true) return; // stream deltas own the refresh
  void refreshRunDetail(false);
}, RUN_DETAIL_POLL_MS);
// Stream deltas: refresh the open modal the moment ITS run changes (the
// delta payload is list-shaped/output-stripped, so re-fetch /runs/{id}
// for the full output rather than rendering the delta directly).
window.addEventListener("whr:run-delta", (e) => {
  if (!currentRunId || !runDetailDialog.open) return;
  const d = e.detail;
  if (d && d.id === currentRunId) void refreshRunDetail(false);
});
// Stream recovery: a change the modal's run made while the stream was down
// may never re-emit a delta (e.g. it went terminal in the gap) — one
// refresh on reconnect closes that hole.
window.addEventListener("whr:stream-state", (e) => {
  if (!e.detail || !e.detail.live) return;
  if (!currentRunId || currentRunTerminal || !runDetailDialog.open) return;
  void refreshRunDetail(false);
});

// The full log as plain text, each line prefixed with its timestamp when one
// is known — the same content the raw view shows, ready to paste into a report.
function buildCopyText() {
  return currentRunLines
    .map((text, i) => {
      const clock = fmtClock(currentRunTimes[i]);
      return clock ? `${clock}  ${text}` : text;
    })
    .join("\n");
}

// navigator.clipboard needs a secure context (the admin port is behind HTTPS
// zero-trust, so it normally works); fall back to a hidden textarea + execCommand
// for plain-HTTP access (e.g. port-forwarding over http://localhost).
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through to the legacy path */
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}

const copyBtn = document.getElementById("run-detail-copy");
copyBtn.addEventListener("click", async () => {
  const text = buildCopyText();
  if (!text) return;
  const ok = await copyToClipboard(text);
  copyBtn.textContent = ok ? "Copied!" : "Copy failed";
  copyBtn.classList.toggle("copied", ok);
  setTimeout(() => {
    copyBtn.textContent = "Copy log";
    copyBtn.classList.remove("copied");
  }, 1500);
});

function parseGitHubURL(repoURL) {
  let m = repoURL.match(/github\.com[:/]([^/]+\/[^/]+?)(?:\.git)?$/);
  if (m) return m[1];
  return null;
}

async function loadConfig() {
  try {
    const cfg = await fetchJSON("/config");
    if (!cfg.hooks_repo) return;

    const section = document.getElementById("setup-section");
    const content = document.getElementById("setup-content");
    section.hidden = false;

    const ghPath = parseGitHubURL(cfg.hooks_repo);
    const repoLink = ghPath
      ? `https://github.com/${ghPath}`
      : cfg.hooks_repo;

    // The repo identity stays visible; the one-time webhook setup recipe
    // lives inside the (collapsed) <details>.
    const repoP = document.getElementById("setup-repo");
    repoP.appendChild(document.createTextNode("Repository: "));
    repoP.appendChild(
      ghPath
        ? el("a", { href: repoLink, target: "_blank" }, ghPath)
        : el("code", null, cfg.hooks_repo)
    );

    const nodes = [];

    const reloadURL = cfg.hook_base_url
      ? cfg.hook_base_url.replace(/\/$/, "") + "/_reload"
      : "/_reload";

    const intro = el("p", null,
      "To auto-reload hooks on push, add a webhook to the repo:"
    );
    nodes.push(intro);

    if (ghPath) {
      const settingsURL = `https://github.com/${ghPath}/settings/hooks/new`;
      const link = el("p", null,
        el("a", { href: settingsURL, target: "_blank", class: "btn" },
          "Add webhook on GitHub")
      );
      nodes.push(link);
    }

    const dl = document.createElement("dl");
    const fields = [
      ["Payload URL", reloadURL],
      ["Content type", "application/json"],
      ["Secret", cfg.reload_secret || "(not configured)"],
      ["Events", "push + status (status is what green-lights a gated reload)"],
    ];
    for (const [k, v] of fields) {
      dl.appendChild(el("dt", null, k));
      const dd = el("dd", null, el("code", { class: "copyable" }, v));
      dl.appendChild(dd);
    }
    nodes.push(dl);

    for (const n of nodes) content.appendChild(n);
  } catch (e) {
    console.error("loadConfig:", e);
  }
}

// One-shot footer stamp: which build is this host running? The tooltip
// carries the VCS revision/commit time when the build has them.
async function loadVersion() {
  try {
    const v = await fetchJSON("/version");
    const span = document.getElementById("server-version");
    if (!span || !v.version) return;
    span.textContent = v.version;
    if (v.revision) span.title = v.revision + (v.time ? " @ " + v.time : "");
  } catch (e) {
    console.error("loadVersion:", e);
  }
}

// Switching between the overview and a per-app page re-renders immediately;
// the poll keeps whichever view is active fresh. Navigation is also what
// (re-)arms the one-shot State (KV) landing scroll.
window.addEventListener("hashchange", () => {
  pendingKVScroll = hashWantsKV();
  refresh();
});

loadConfig();
loadVersion();
// Boot paint immediately; after that the stream's push signals own
// freshness and this interval is the pure FALLBACK — a no-op while the
// stream is live, a fixed-cadence full refresh while it is down (or if
// timeline.js never loaded, leaving whrStreamLive undefined). Armed once,
// never cleared, never grows: the runs-feed supervisor pattern.
refresh();
setInterval(() => {
  if (window.whrStreamLive === true) return;
  refresh();
}, FALLBACK_POLL_MS);

// --- Hooks repo reload panel -------------------------------------------------
//
// Which commit of the hooks repo is LIVE, what the CI reload gate is
// holding, and the operator's manual controls: "Check & reload now" (one
// on-demand reconcile pass — POST /reload/check; in legacy mode the plain
// pull+reload), a recent-commits picker with per-commit CI + src-layout
// badges and a "Make live" action, and a paste-a-ref switch. Switching is
// SERVER-gated: POST /reload/switch without override answers 409 naming
// every reason when the commit is not CI-green (unknown counts as not
// green) or its tree lacks src/hooks; only after the operator confirms the
// quoted reasons does the retry carry override:true. The section refreshes
// by push (the "reload" section signal — every reload.*/git.*/push event);
// the commits list itself is fetched only while its <details> is open,
// because listing fetches origin and probes CI.

let reloadMode = null; // "gated" | "legacy" | null (panel hidden)
let reloadSwitchInFlight = false;

function reloadCIBadge(state) {
  const s = state || "unknown";
  let cls = "badge";
  if (s === "success") cls += " ok";
  else if (s === "failure" || s === "error") cls += " bad";
  else if (s === "pending") cls += " warn";
  const titles = {
    success: "the gating CI context reports green for this commit",
    failure: "the gating CI context reports FAILURE for this commit",
    error: "the gating CI context reports ERROR for this commit",
    pending: "the gating CI context is still running for this commit",
    none: "CI has not reported the gating context for this commit yet",
    unknown: "the CI state could not be read (no token / API unreachable) — treated as not green, never guessed",
  };
  return el("span", { class: cls, title: titles[s] || "" }, "CI: " + s);
}

function reloadSrcBadge(has) {
  return has
    ? el("span", { class: "badge ok", title: "the commit's tree contains src/hooks — the layout this fleet loads" }, "src ok")
    : el("span", { class: "badge bad", title: "the commit's tree has NO src/hooks directory — reloading from it would load zero hooks" }, "no src/hooks");
}

function renderReloadStatus(data) {
  const section = document.getElementById("reload-section");
  const usable = !!data && (data.mode === "gated" || data.mode === "legacy");
  section.hidden = !usable;
  reloadMode = usable ? data.mode : null;
  if (!usable) return;
  // Per-commit switching needs the gate; legacy mode keeps the live view
  // and the Check & reload (pull to tip) but hides the picker.
  document.getElementById("reload-picker").hidden = data.mode !== "gated";

  const box = document.getElementById("reload-live");
  box.innerHTML = "";
  const live = data.live || {};
  box.appendChild(el("div", { class: "reload-live-row" },
    el("span", { class: "reload-label" }, "Live commit"),
    el("code", { title: live.sha || "" }, live.short || "(unknown)"),
    data.hooks_branch ? el("span", { class: "reload-branch" }, "on " + data.hooks_branch) : null,
    reloadCIBadge(live.ci_state),
    reloadSrcBadge(!!live.has_src),
    data.mode === "legacy"
      ? el("span", { class: "badge warn", title: "The CI reload gate is disabled (WEBHOOK_RUNNER_HOOKS_GATE_CONTEXT is empty): any signed push reloads, and per-commit switching is unavailable." }, "gate disabled (legacy)")
      : null,
    data.verified === false
      ? el("span", { class: "badge warn", title: "No recorded " + (data.gate_context || "CI") + " green vouches for the serving tree yet; it verifies on its next green (or an operator switch)." }, "unverified")
      : null,
  ));
  if (live.subject) {
    box.appendChild(el("div", { class: "reload-subject" },
      live.subject + (live.date ? " · " + fmtTime(live.date) : "")));
  }
  if (data.pending) {
    const p = data.pending;
    box.appendChild(el("div", { class: "reload-pending" },
      el("span", { class: "reload-label" }, "Held"),
      el("code", { title: p.sha || "" }, p.short || ""),
      p.subject ? el("span", { class: "reload-subject-inline" }, p.subject) : null,
      el("span", { class: "wait-note" }, p.why || "awaiting CI"),
      reloadCIBadge(p.ci_state),
      reloadSrcBadge(!!p.has_src),
    ));
  }
}

async function refreshReloadCommits() {
  const note = document.getElementById("reload-commits-note");
  try {
    renderReloadCommits(await fetchJSON("/reload/commits"));
  } catch (err) {
    note.textContent = "Failed to list commits: " + err.message;
    note.hidden = false;
  }
}

function renderReloadCommits(data) {
  const commits = (data && data.commits) || [];
  const tbody = document.querySelector("#reload-commits-table tbody");
  tbody.innerHTML = "";
  const note = document.getElementById("reload-commits-note");
  note.textContent = "No commits found.";
  note.hidden = commits.length > 0;
  for (const c of commits) {
    let action;
    if (c.is_live) {
      action = el("span", { class: "badge ok" }, "live");
    } else {
      action = el("button", { class: "toggle-btn", title: "Switch the serving hooks tree to this commit" }, "Make live");
      action.addEventListener("click", () => reloadSwitchTo(c.sha, c.short));
    }
    tbody.appendChild(el("tr", { class: c.is_live ? "reload-live-commit" : "" },
      el("td", null, el("code", { title: c.sha }, c.short)),
      el("td", { class: "reload-commit-subject" }, c.subject || ""),
      el("td", null, fmtTime(c.date)),
      el("td", null, reloadCIBadge(c.ci_state)),
      el("td", null, reloadSrcBadge(!!c.has_src)),
      el("td", { class: "row-actions" }, action),
    ));
  }
}

// The section fetcher (registered in sectionFetchers as "reload"): the
// cheap status view always; the origin-fetching commits list only while
// the picker is open.
async function refreshReloadPanel() {
  renderReloadStatus(await fetchJSON("/reload/status"));
  if (reloadMode === "gated" && document.getElementById("reload-picker").open) {
    await refreshReloadCommits();
  }
}

async function postJSON(url, body) {
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: body == null ? undefined : JSON.stringify(body),
    // A switch fetches origin, probes CI, and reloads hooks — allow it
    // time, but never hang the panel forever.
    signal: AbortSignal.timeout(60000),
  });
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* non-JSON error body: fall back to the status code */
  }
  return { res, data };
}

async function reloadCheckNow() {
  const btn = document.getElementById("reload-check");
  const resultEl = document.getElementById("reload-check-result");
  btn.disabled = true;
  resultEl.textContent = "checking…";
  try {
    const { res, data } = await postJSON("/reload/check", null);
    if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
    // Gated mode reports the reconcile outcome; legacy reports status.
    resultEl.textContent = "outcome: " + ((data && (data.outcome || data.status)) || "done");
  } catch (err) {
    resultEl.textContent = "check failed: " + err.message;
  } finally {
    btn.disabled = false;
    void refreshReloadPanel();
  }
}

// The informed-override flow. The server stays authoritative: the first
// attempt NEVER carries override, and only its 409 (with the server's own
// reasons) leads to a confirmation that quotes them verbatim; the retry —
// and only the retry — carries override:true.
async function reloadSwitchTo(ref, label) {
  if (reloadSwitchInFlight) return;
  const name = label || ref;
  if (!confirm(`Make ${name} the live hooks commit?\n\nThe serving tree switches to it and hooks reload.`)) return;
  reloadSwitchInFlight = true;
  const resultEl = document.getElementById("reload-check-result");
  try {
    let { res, data } = await postJSON("/reload/switch", { ref, override: false });
    if (res.status === 409 && data && data.requires_override) {
      const reasons = (data.reasons || []).map((r) => "  - " + r).join("\n");
      const msg =
        `Switching to ${name} is BLOCKED by the reload safety gate:\n\n${reasons}\n\n` +
        "Proceed anyway? This OVERRIDES the CI / src-layout safety gate and reloads the hooks " +
        "tree from that commit. The override is recorded on the activity feed.";
      if (!confirm(msg)) {
        resultEl.textContent = "switch cancelled";
        return;
      }
      ({ res, data } = await postJSON("/reload/switch", { ref, override: true }));
    }
    if (!res.ok) {
      const errMsg = (data && data.error) || `HTTP ${res.status}`;
      alert(`Switch to ${name} failed: ${errMsg}`);
      resultEl.textContent = "switch failed";
      return;
    }
    resultEl.textContent = data && data.overridden
      ? `switched to ${name} (gate overridden)`
      : `switched to ${name}`;
  } catch (err) {
    alert(`Switch to ${name} failed: ${err.message}`);
  } finally {
    reloadSwitchInFlight = false;
    void refreshReloadPanel();
  }
}

document.getElementById("reload-check").addEventListener("click", () => void reloadCheckNow());
document.getElementById("reload-ref-switch").addEventListener("click", () => {
  const ref = (document.getElementById("reload-ref-input").value || "").trim();
  if (!ref) {
    alert("Enter a commit sha or branch/tag name first.");
    return;
  }
  void reloadSwitchTo(ref, ref);
});
document.getElementById("reload-ref-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") document.getElementById("reload-ref-switch").click();
});
// The commits list is fetched lazily: opening the picker is the operator
// asking for it (it fetches origin and probes CI per commit).
document.getElementById("reload-picker").addEventListener("toggle", () => {
  if (document.getElementById("reload-picker").open) void refreshReloadCommits();
});
