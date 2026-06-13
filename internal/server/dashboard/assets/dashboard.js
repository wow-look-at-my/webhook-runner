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
    const [hooks, runs, images, events] = await Promise.all([
      fetchJSON("/hooks"),
      fetchJSON("/runs?max=50"),
      fetchJSON("/images"),
      fetchJSON("/events?max=100"),
    ]);
    renderHooks(hooks);
    renderRuns(runs);
    renderImages(images);
    renderEvents(events);
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

function renderEvents(events) {
  const tbody = document.querySelector("#events-table tbody");
  tbody.innerHTML = "";
  document.getElementById("events-empty").hidden = events.length > 0;
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

loadConfig();
refresh();
setInterval(refresh, POLL_MS);
