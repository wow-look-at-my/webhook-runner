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

async function refresh() {
  try {
    await fetchJSON("/health");
    setBadge(true);
  } catch {
    setBadge(false);
  }
  try {
    const [hooks, runs] = await Promise.all([
      fetchJSON("/hooks"),
      fetchJSON("/runs?max=50"),
    ]);
    renderHooks(hooks);
    renderRuns(runs);
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
        el("td", null, el("code", null, h.id)),
        el("td", null, h.description || ""),
        el("td", null, h.synchronous ? "sync" : "async"),
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
    const out = document.getElementById("run-detail-output");
    out.textContent = (r.output || []).join("\n") || "(no output)";
    document.getElementById("run-detail").hidden = false;
    document.getElementById("run-detail").scrollIntoView({ behavior: "smooth" });
  } catch (e) {
    console.error(e);
  }
}

document.getElementById("run-detail-close").addEventListener("click", () => {
  document.getElementById("run-detail").hidden = true;
});

refresh();
setInterval(refresh, POLL_MS);
