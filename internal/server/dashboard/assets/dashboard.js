"use strict";

const POLL_MS = 3000;

async function fetchJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" } });
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
// holds it (run + hook).
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
  if (!tsPresent(w.until)) return null;
  const left = new Date(w.until) - Date.now();
  const t = left > 0 ? fmtDuration(left) : "0s";
  return el("span", { class: "wait-note" },
    `waiting ${t}${w.reason ? ": " + w.reason : ""}`);
}

// The holder-side note: runs currently blocked on locks THIS run holds
// (r.waiters, derived server-side). The tooltip lists exactly who waits on
// which key.
function waitersNote(r) {
  const ws = r.waiters;
  if (!ws || !ws.length) return null;
  return el("span", { class: "wait-note", title: ws.map(waiterText).join("\n") },
    `${ws.length} waiting on this run's lock${ws.length > 1 ? "s" : ""}`);
}

function shortId(id) {
  return id && id.length > 10 ? id.slice(0, 10) + "…" : id || "";
}

function waiterText(x) {
  return `${shortId(x.run_id)} (${x.hook_id})${x.key ? ` → lock ${x.key}` : ""}`;
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
      await refreshApp(hookId);
    } else {
      const [hooks, runs, images, events, kv, groups] = await Promise.all([
        fetchJSON("/hooks"),
        fetchJSON("/runs?max=50"),
        fetchJSON("/images"),
        fetchJSON("/events?max=100"),
        fetchJSON("/kv"),
        fetchJSON("/concurrency"),
      ]);
      renderHooks(hooks);
      renderRuns(runs);
      renderImages(images);
      renderEvents(events);
      renderKV(kv, new Set(hooks.map((h) => h.id)));
      renderConcurrency(groups);
    }
    document.getElementById("updated").textContent =
      "updated " + new Date().toLocaleTimeString();
  } catch (e) {
    console.error(e);
  }
}

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

function hookToggleButton(id, disabled) {
  const btn = el("button", {
    class: "toggle-btn" + (disabled ? "" : " danger"),
    title: disabled
      ? `Re-enable ${id}: accept deliveries and scheduled runs again`
      : `Disable ${id}: reject deliveries (503) and skip scheduled runs`,
  }, disabled ? "Enable" : "Disable");
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    toggleHook(id, !disabled);
  });
  return btn;
}

function hookStatusBadge(disabled) {
  return disabled
    ? el("span", { class: "badge bad" }, "DISABLED")
    : el("span", { class: "badge ok" }, "enabled");
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
        el("td", { class: "row-actions" }, hookStatusBadge(h.disabled), hookToggleButton(h.id, h.disabled)),
        el("td", null, ...triggerPath(h.id)),
      )
    );
  }
}

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
  document.getElementById("app-disabled-badge").hidden = true;
  document.getElementById("app-toggle").hidden = true;
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

  // Operator kill switch for this hook: badge + toggle next to the title.
  document.getElementById("app-disabled-badge").hidden = !detail.disabled;
  const toggle = document.getElementById("app-toggle");
  toggle.hidden = false;
  toggle.textContent = detail.disabled ? "Enable hook" : "Disable hook";
  toggle.classList.toggle("danger", !detail.disabled);
  toggle.onclick = () => toggleHook(info.id, !detail.disabled);

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

async function showRun(id) {
  try {
    const r = await fetchJSON(`/runs/${id}`);
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
      ["Status", r.status],
      ["Exit code", String(r.exit_code)],
      ["Queued", fmtTime(r.started)],
      ["Started", tsPresent(r.started_at) ? fmtTime(r.started_at) : "—"],
      ["Finished", tsPresent(r.finished) ? fmtTime(r.finished) : "—"],
      ["Waited", runWaited(r)],
      ["Duration", runDuration(r)],
    ];
    // A live pause gets its own row (same text as the run-row note): a
    // declared sleep, or the lock (and holder) a blocked acquire waits on.
    const wn = waitNote(r);
    if (wn) rows.push(["Waiting", wn]);
    // The holder-side view: who is blocked on locks this run holds.
    if (r.waiters && r.waiters.length) {
      rows.push(["Lock waiters", r.waiters.map(waiterText).join("; ")]);
    }
    if (r.error) rows.push(["Error", r.error]);
    for (const [k, v] of rows) {
      dl.appendChild(el("dt", null, k));
      dl.appendChild(el("dd", null, v));
    }
    currentRunLines = r.output || [];
    currentRunTimes = r.output_times || [];
    const entries = currentRunLines.map((text, i) => ({ text, time: currentRunTimes[i] }));
    currentRunTurns = parseConversation(entries);
    document.getElementById("run-detail-view-toggle").hidden = !currentRunTurns;
    document.getElementById("run-detail-copy").disabled = currentRunLines.length === 0;
    renderRunOutput(currentRunTurns ? "conversation" : "raw");
    const dlg = document.getElementById("run-detail");
    if (!dlg.open) dlg.showModal();
  } catch (e) {
    console.error(e);
  }
}

const runDetailDialog = document.getElementById("run-detail");
document.getElementById("run-detail-close").addEventListener("click", () => {
  runDetailDialog.close();
});
// Click outside the modal box (on the backdrop) closes it; Escape already does.
runDetailDialog.addEventListener("click", (e) => {
  if (e.target === runDetailDialog) runDetailDialog.close();
});
// Switch between the conversation and raw-log views of the same run output.
document.getElementById("run-detail-view-toggle").addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-view]");
  if (btn) renderRunOutput(btn.dataset.view);
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
      ["Events", "Just the push event"],
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
refresh();
setInterval(refresh, POLL_MS);
