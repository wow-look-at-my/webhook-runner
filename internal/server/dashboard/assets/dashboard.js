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

// Started→Finished span of a run from the /runs list; a still-active run
// shows its live elapsed time (the poll keeps it ticking).
function runDuration(r) {
  const started = new Date(r.started);
  if (isNaN(started)) return "";
  if (r.finished) return fmtDuration(new Date(r.finished) - started);
  return fmtDuration(Date.now() - started) + "…";
}

// --- Views: the global overview vs the per-app (per-hook) drill-down ------
//
// The fragment #hook=<id> selects the app view; anything else shows the
// overview. Hash routing keeps the page a single embedded document with no
// server-side routes, and hashchange gives back/forward for free.

function currentHookId() {
  const m = location.hash.match(/^#hook=(.+)$/);
  return m ? decodeURIComponent(m[1]) : null;
}

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
      const [hooks, runs, images, events, kv] = await Promise.all([
        fetchJSON("/hooks"),
        fetchJSON("/runs?max=50"),
        fetchJSON("/images"),
        fetchJSON("/events?max=100"),
        fetchJSON("/kv"),
      ]);
      renderHooks(hooks);
      renderRuns(runs);
      renderImages(images);
      renderEvents(events);
      renderKV(kv);
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

function renderHooks(hooks) {
  const tbody = document.querySelector("#hooks-table tbody");
  tbody.innerHTML = "";
  document.getElementById("hooks-empty").hidden = hooks.length > 0;
  for (const h of hooks) {
    const url = `${location.origin}/hook/${h.id}`;
    tbody.appendChild(
      el("tr", null,
        // Each hook is an "app": its ID links to the per-hook drill-down.
        el("td", null,
          el("a", { href: "#hook=" + encodeURIComponent(h.id), class: "hook-link" },
            el("code", null, h.id))),
        el("td", null, h.description || ""),
        el("td", null, (h.synchronous ? "sync" : "async") + (h.schedule ? ` · every ${h.schedule}` : "")),
        el("td", null, el("code", null, url)),
      )
    );
  }
}

function renderRuns(rs) {
  const tbody = document.querySelector("#runs-table tbody");
  tbody.innerHTML = "";
  document.getElementById("runs-empty").hidden = rs.length > 0;
  for (const r of rs) {
    const tr = el("tr", { data: { runId: r.id } },
      el("td", null, fmtTime(r.started)),
      el("td", null, el("code", null, r.hook_id)),
      el("td", { class: "status " + r.status }, r.status),
      el("td", null, String(r.exit_code)),
      el("td", null, el("code", null, r.id)),
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

function renderKV(namespaces) {
  const tbody = document.querySelector("#kv-table tbody");
  tbody.innerHTML = "";
  document.getElementById("kv-empty").hidden = namespaces.length > 0;
  for (const ns of namespaces) {
    tbody.appendChild(
      el("tr", null,
        el("td", null, el("code", null, ns.namespace)),
        el("td", null, String(ns.keys)),
        el("td", null, fmtBytes(ns.bytes)),
      )
    );
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
    // lands here too; the next poll retries.
    renderAppMissing(id);
    return;
  }
  const [runs, events] = await Promise.all([
    fetchJSON(`/runs?hook=${enc}&max=50`),
    fetchJSON(`/events?hook=${enc}&max=100`),
  ]);
  renderApp(detail, runs, events);
}

function renderAppMissing(id) {
  document.getElementById("app-title").textContent = id;
  document.getElementById("app-desc").textContent = "";
  document.getElementById("app-missing").hidden = false;
  document.getElementById("app-body").hidden = true;
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
  document.getElementById("app-title").textContent = info.id;
  document.getElementById("app-desc").textContent = info.description || "";

  fillDl(document.getElementById("app-info"), [
    ["Trigger URL", el("code", null, `${location.origin}/hook/${info.id}`)],
    ["Mode", info.synchronous ? "sync" : "async"],
    ["Schedule", info.schedule ? `every ${info.schedule}` : "—"],
    ["Concurrency group", info.concurrency_group ? el("code", null, info.concurrency_group) : "—"],
    ["Timeout", info.timeout],
    ["API key", info.api_key ? "configured" : "none"],
    ["Env vars", info.env_keys && info.env_keys.length
      ? info.env_keys.map((k) => el("code", { class: "env-key" }, k))
      : "none"],
    ["State (KV)", !info.state ? "off"
      : detail.kv ? `on — ${detail.kv.keys} key(s), ${fmtBytes(detail.kv.bytes)}`
      : "on — no data yet"],
  ]);

  const st = detail.stats;
  document.getElementById("app-stats-window").textContent =
    `Recent window: the last ≤${st.max_tracked} runs held in memory (resets on restart).`;
  const byStatus = Object.entries(st.by_status || {}).map(([k, n]) =>
    el("span", { class: "status " + k }, `${k} ×${n} `));
  fillDl(document.getElementById("app-stats"), [
    ["Runs tracked", String(st.tracked)],
    ["By status", byStatus.length ? byStatus : "—"],
    ["Success rate", st.completed ? `${Math.round(st.success_rate * 100)}% of ${st.completed} completed` : "—"],
    ["Avg duration", st.completed ? fmtDuration(st.avg_duration_ms) : "—"],
    ["Max duration", st.completed ? fmtDuration(st.max_duration_ms) : "—"],
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
    const tr = el("tr", null,
      el("td", null, fmtTime(r.started)),
      el("td", { class: "status " + r.status }, r.status),
      el("td", null, runDuration(r)),
      el("td", null, String(r.exit_code)),
      el("td", null, el("code", null, r.id)),
    );
    tr.addEventListener("click", () => showRun(r.id));
    tbody.appendChild(tr);
  }

  renderEventRows("app-events-table", "app-events-empty", events);
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
    document.getElementById("run-detail-id").textContent = r.id;
    const dl = document.getElementById("run-detail-meta");
    dl.innerHTML = "";
    const rows = [
      ["Hook", r.hook_id],
      ["Status", r.status],
      ["Exit code", String(r.exit_code)],
      ["Started", fmtTime(r.started)],
      ["Finished", r.finished ? fmtTime(r.finished) : "-"],
    ];
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
// the poll keeps whichever view is active fresh.
window.addEventListener("hashchange", refresh);

loadConfig();
loadVersion();
refresh();
setInterval(refresh, POLL_MS);
