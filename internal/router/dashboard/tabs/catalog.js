// Catalog tab — "what can I name, and what does it cost?" The models table
// with its filter chips and toggles, plus the Model Aliases list. Ported
// behaviour-for-behaviour from the legacy render() on 2026-09-12. Filter
// state lives in the hash query (#catalog?node=hypatia&cap=vision,paid&
// hidden=1&cloud=0) so the Fleet tab can deep-link and a reload keeps it.
// Data: /api/catalog every 30 s (no node probe on that path) + /api/node-metrics
// every 2 s, merged the way the legacy pollMetrics did.

const SHOW_TAGS = [
  "tts",
  "stt",
  "image_gen",
  "image_edit",
  "music_gen",
  "fim",
  "gui_agent",
  "embedding",
  "reranker",
  "thinking",
  "chain",
  "paid",
  "openrouter",
  "zen",
  "go",
  "discovered",
];

let root = null;
let ctx = null;
let data = null;
let lastStateKey = "";
const filters = { node: null, caps: new Set(), hidden: false, cloud: false };
let openModel = null; // the drawer's model id, mirrored into #catalog?model=
let fleetRoles = null; // /api/fleet roles, fetched when a drawer opens
let lastFocus = null;

function readHash() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  const q = new URLSearchParams(qs || "");
  filters.node = q.get("node") || null;
  filters.caps = new Set((q.get("cap") || "").split(",").filter(Boolean));
  filters.hidden = q.get("hidden") === "1";
  filters.cloud = q.get("cloud") === "1";
  openModel = q.get("model") || null;
}
function writeHash() {
  const q = new URLSearchParams();
  if (filters.node) q.set("node", filters.node);
  if (filters.caps.size) q.set("cap", [...filters.caps].join(","));
  if (filters.hidden) q.set("hidden", "1");
  if (filters.cloud) q.set("cloud", "1");
  if (openModel) q.set("model", openModel);
  const s = q.toString();
  history.replaceState(null, "", `#catalog${s ? "?" + s : ""}`);
}

function mergeMetrics(nm) {
  const state = {},
    reqs = {};
  for (const m of Object.values(nm)) {
    for (const mdl of m.models || []) {
      state[mdl.model_id] = mdl.state;
      reqs[mdl.model_id] = {
        running: mdl.requests_running || 0,
        waiting: mdl.requests_waiting || 0,
        avg_tok_per_s: mdl.avg_tok_per_s || null,
        total_requests: mdl.total_requests || 0,
      };
    }
  }
  for (const m of data.models) {
    m.agent_state = state[m.id] || null;
    const r = reqs[m.id] || {};
    m.requests_running = r.running || 0;
    m.requests_waiting = r.waiting || 0;
    m.avg_tok_per_s = r.avg_tok_per_s || null;
    m.total_requests = r.total_requests || 0;
  }
  return JSON.stringify([state, reqs]);
}

function rowFor(m) {
  const { escHtml, fmtCtx, quantLabel, modelType, isRouterModel, isCloudModel } = ctx.fmt;
  const nodeStr = m.nodes.map((n) => (n === m.head_node && m.nodes.length > 1 ? `<strong>${escHtml(n)}</strong>` : escHtml(n))).join(", ");
  const aliases = m.aliases.map((a) => `<span class="badge badge-alias">${escHtml(a)}</span>`).join(" ");
  const caps = m.capabilities.map((c) => `<span class="badge badge-cap">${escHtml(c)}</span>`).join(" ");
  let flags = "";
  if (m.enabled === false) flags += '<span class="badge badge-off">disabled</span> ';
  if (m.always_on) flags += '<span class="badge badge-on">always-on</span> ';
  else flags += '<span class="badge badge-off">on-demand</span> ';
  if (m.tool_proxy) flags += '<span class="badge badge-tool">tool-proxy</span> ';
  if (m.nodes.length > 1) flags += '<span class="badge badge-multi">multi-node</span> ';
  for (const t of m.tags || []) flags += `<span class="badge badge-tag">${escHtml(t)}</span> `;

  // Status: router stubs first, then the tracker's verdicts that outrank the
  // agent (absent, warming), then the agent's state, then the registry's.
  let statusClass, statusLabel;
  if (isRouterModel(m)) {
    statusClass = "health-healthy";
    statusLabel = "router";
  } else if (m.availability === "absent") {
    statusClass = "health-error";
    statusLabel = "absent";
  } else if (m.availability === "warming") {
    statusClass = "health-starting";
    statusLabel = "warming";
  } else if (m.agent_state) {
    statusClass = `health-${m.agent_state}`;
    statusLabel = m.agent_state;
  } else {
    statusClass = `health-${m.health}`;
    statusLabel = m.health === "routed" ? "healthy" : m.health;
  }
  const parts = [];
  const running = m.requests_running || 0,
    waiting = m.requests_waiting || 0;
  if (running > 0 || waiting > 0) parts.push(`<span style="color:var(--yellow)">⚡ ${running} active${waiting > 0 ? ` +${waiting}w` : ""}</span>`);
  if (m.avg_tok_per_s) parts.push(`<span style="color:var(--text-dim)">${m.avg_tok_per_s} tok/s</span>`);
  if (m.total_requests > 0) parts.push(`<span style="color:var(--text-dim)">${m.total_requests} reqs</span>`);
  const healthExtra = parts.length ? `<div style="font-size:0.7rem;margin-top:2px">${parts.join(" · ")}</div>` : "";

  const isHidden = m.enabled === false || m.agent_state === "stopped";
  const isCloud = isCloudModel(m);
  let visible = true;
  if (isHidden && !filters.hidden) visible = false;
  if (isCloud && filters.cloud) visible = false;
  if (filters.node && !m.nodes.includes(filters.node)) visible = false;
  if (filters.caps.size > 0) {
    const all = [...m.capabilities, ...(m.tags || [])];
    if (![...filters.caps].some((c) => all.includes(c))) visible = false;
  }
  const ctxLabel = (() => {
    if (!m.context_length) return "";
    const win = fmtCtx(m.context_length);
    return m.effective_context ? `${win} (good to ${fmtCtx(m.effective_context)})` : win;
  })();
  const infoRows = [
    ["repo", m.hf_repo.split("#")[0]],
    ["file", m.gguf_file || ""],
    ["quant", quantLabel(m)],
    ["context", ctxLabel],
  ]
    .filter(([, v]) => v)
    .map(([k, v]) => `<div class="mi-row"><span class="mi-label">${k}</span><span>${escHtml(v)}</span></div>`)
    .join("");
  const mt = modelType(m);
  const title = m.availability_reason ? ` title="${escHtml(m.availability_reason)}"` : "";
  return `<tr data-model="${escHtml(m.id)}" tabindex="0" class="cat-row${isHidden ? " disabled" : ""}${openModel === m.id ? " cat-row-open" : ""}"${!visible ? ' style="display:none"' : ""}>
      <td class="model-cell"><div class="model-id">${escHtml(m.id)}</div><div class="model-info">${infoRows}</div></td>
      <td><span class="badge ${mt.badge}">${escHtml(mt.label)}</span></td>
      <td>${nodeStr}</td>
      <td>${m.vram_gb ? m.vram_gb + " GB" : '<span style="color:var(--text-dim)">—</span>'}</td>
      <td>${aliases || '<span style="color:var(--text-dim)">—</span>'}</td>
      <td>${caps}</td>
      <td>${flags}</td>
      <td><span style="white-space:nowrap"${title}><span class="health-dot ${statusClass}"></span>${escHtml(statusLabel)}</span>${healthExtra}</td>
      <td><span class="api-base">${escHtml(m.api_base)}</span></td>
    </tr>`;
}

// ---------------------------------------------------------------------------
// Detail drawer: everything the registry and the tracker know about one entry.
// ---------------------------------------------------------------------------

function referencesTo(id) {
  // Chains whose fallbacks include it, and roles whose candidates/overflow do.
  const chains = (data?.models || []).filter((m) => (m.fallbacks || []).includes(id)).map((m) => m.id);
  const roles = [];
  for (const r of fleetRoles || []) {
    if ((r.candidates || []).includes(id)) roles.push(r.role);
    else if ((r.overflow || []).includes(id)) roles.push(r.role + " (overflow)");
  }
  return { chains, roles };
}

function renderDrawer() {
  const { escHtml, fmtCtx, quantLabel, modelType, fmtAgoIso } = ctx.fmt;
  let el = root.querySelector("#cat-drawer");
  if (!openModel) {
    if (el) el.remove();
    return;
  }
  const m = (data?.models || []).find((x) => x.id === openModel);
  if (!el) {
    el = document.createElement("aside");
    el.id = "cat-drawer";
    el.className = "cat-drawer";
    el.setAttribute("role", "dialog");
    el.setAttribute("aria-label", "model details");
    root.appendChild(el);
  }
  if (!m) {
    el.innerHTML = `<div class="cat-drawer-head"><strong>${escHtml(openModel)}</strong><button class="chat-x" data-action="drawer-close" title="Close">&#10005;</button></div><p class="chat-placeholder">not in the catalog</p>`;
    return;
  }
  const row = (k, v) =>
    v === "" || v == null || (Array.isArray(v) && !v.length) ? "" : `<div class="mi-row"><span class="mi-label">${k}</span><span>${v}</span></div>`;
  const badges = (xs, cls) => (xs || []).map((x) => `<span class="badge ${cls}">${escHtml(x)}</span>`).join(" ");
  const price =
    m.input_cost_per_million != null || m.output_cost_per_million != null
      ? `$${m.input_cost_per_million ?? 0} in · $${m.output_cost_per_million ?? 0} out per M`
      : "";
  const ctxLabel = m.context_length ? fmtCtx(m.context_length) + (m.effective_context ? ` (good to ${fmtCtx(m.effective_context)})` : "") : "";
  const refs = referencesTo(m.id);
  let status = m.availability ? escHtml(m.availability) : m.agent_state ? escHtml(m.agent_state) : escHtml(m.health || "");
  if (m.availability_reason) status += ` — ${escHtml(m.availability_reason)}`;
  if (m.availability_since && !m.availability_since.startsWith("0001"))
    status += ` <span class="act-dim">since ${fmtAgoIso(m.availability_since)}</span>`;
  el.innerHTML = `
    <div class="cat-drawer-head"><div><strong>${escHtml(m.id)}</strong> <span class="badge ${modelType(m).badge}">${escHtml(modelType(m).label)}</span>${m.discovered ? ' <span class="badge badge-tag">discovered</span>' : ""}</div><button class="chat-x" data-action="drawer-close" title="Close">&#10005;</button></div>
    <div class="model-info" style="font-size:0.8rem">
      ${row("status", status)}
      ${row("repo", escHtml((m.hf_repo || "").split("#")[0]))}
      ${row("file", escHtml(m.gguf_file || ""))}
      ${row("quant", escHtml(quantLabel(m)))}
      ${row("backend", escHtml(m.backend || ""))}
      ${row("node(s)", (m.nodes || []).length ? escHtml(m.nodes.join(", ")) : "")}
      ${row("api base", m.api_base ? `<span class="api-base">${escHtml(m.api_base)}</span>` : "")}
      ${row("api class", escHtml(m.api_class || ""))}
      ${row("context", ctxLabel)}
      ${row("max output", m.max_output_tokens ? fmtCtx(m.max_output_tokens) : "")}
      ${row("price", price)}
      ${row("aliases", badges(m.aliases, "badge-alias"))}
      ${row("capabilities", badges(m.capabilities, "badge-cap"))}
      ${row("tags", badges(m.tags, "badge-tag"))}
      ${row("flags", [m.enabled === false ? "disabled" : "", m.always_on ? "always-on" : "on-demand", m.tool_proxy ? "tool-proxy" : ""].filter(Boolean).join(" · "))}
      ${row("chain of", badges(m.fallbacks, "badge-tag"))}
      ${row("in chains", badges(refs.chains, "badge-tag"))}
      ${row("in roles", fleetRoles ? badges(refs.roles, "badge-tool") || '<span class="act-dim">none</span>' : '<span class="act-dim">loading…</span>')}
      ${m.discovered ? row("provenance", `adopted from the live listing at <span class="api-base">${escHtml(m.api_base || "")}</span>; no models.yaml entry`) : ""}
    </div>`;
}

function openDrawer(id, focusEl) {
  openModel = id;
  lastFocus = focusEl || null;
  writeHash();
  render();
  if (!fleetRoles) {
    ctx.api
      .get("/api/fleet")
      .then((f) => {
        fleetRoles = f.roles || [];
        if (openModel) renderDrawer();
      })
      .catch(() => {});
  }
}
function closeDrawer() {
  if (!openModel) return;
  const id = openModel;
  openModel = null;
  writeHash();
  render();
  const back = lastFocus && root.contains(lastFocus) ? lastFocus : root.querySelector(`tr[data-model="${CSS.escape(id)}"]`);
  if (back) back.focus();
}

function render() {
  if (!root || !data) return;
  const { escHtml, isCloudModel, isRouterModel, modelType } = ctx.fmt;
  const models = data.models || [];
  const hiddenCount = models.filter((m) => m.enabled === false || m.agent_state === "stopped").length;
  const cloudCount = models.filter(isCloudModel).length;

  // One merged count per chip label: a name that is both a capability and a
  // tag on some model (image_gen) must not produce two identical chips.
  const chipCounts = {};
  for (const m of models) {
    if (m.enabled === false) continue;
    const seen = new Set(m.capabilities);
    for (const t of m.tags || []) if (SHOW_TAGS.includes(t)) seen.add(t);
    for (const k of seen) chipCounts[k] = (chipCounts[k] || 0) + 1;
  }
  const hasFilters = filters.node || filters.caps.size > 0;
  let html = `<div class="section-title">Models${filters.node ? ` <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">on ${escHtml(filters.node)}</span>` : ""}</div><div class="filter-bar">`;
  for (const [label, n] of Object.entries(chipCounts).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))) {
    html += `<span class="filter-chip${filters.caps.has(label) ? " active" : ""}" data-cap="${escHtml(label)}">${escHtml(label)} <span class="count">${n}</span></span>`;
  }
  if (hasFilters) html += `<button class="clear-btn" data-action="clear">clear filters</button>`;
  html += `</div>`;
  if (hiddenCount > 0)
    html += `<div class="toggle-row"><input type="checkbox" id="show-hidden" ${filters.hidden ? "checked" : ""}><label for="show-hidden">Show disabled/stopped models (${hiddenCount})</label></div>`;
  if (cloudCount > 0)
    html += `<div class="toggle-row"><input type="checkbox" id="hide-cloud" ${filters.cloud ? "checked" : ""}><label for="hide-cloud">Hide cloud-only models (${cloudCount}) &mdash; keeps node-pinned services and the auto-routers</label></div>`;
  html += `<table><thead><tr><th>Model</th><th>Backend</th><th>Node(s)</th><th>VRAM</th><th>Aliases</th><th>Capabilities</th><th>Flags</th><th>Health</th><th>API Base</th></tr></thead><tbody>`;
  for (const m of models) html += rowFor(m);
  html += `</tbody></table>`;

  // Alias descriptions — auto-generated from enabled models with aliases.
  const aliasItems = [];
  for (const m of models) {
    if (m.enabled === false || isRouterModel(m)) continue;
    for (const a of m.aliases) aliasItems.push({ name: a, model: m.id, type: modelType(m).label });
  }
  html += `<details class="alias-details"><summary class="section-title" style="cursor:pointer;user-select:none">Model Aliases <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(${aliasItems.length}, click to expand)</span></summary><div class="alias-list" style="grid-template-columns: repeat(4, 1fr); margin-top:0.5rem">`;
  for (const a of aliasItems)
    html += `<div class="alias-item"><span class="alias-name">${escHtml(a.name)}</span><span class="alias-desc">${escHtml(a.model)}</span></div>`;
  html += `</div></details>`;
  // Keep the aliases <details> open state across re-renders.
  const wasOpen = root.querySelector(".alias-details")?.open;
  root.innerHTML = html;
  if (wasOpen) root.querySelector(".alias-details").open = true;
  renderDrawer();
}

function onKey(ev) {
  if (ev.key === "Escape" && openModel) {
    ev.preventDefault();
    closeDrawer();
    return;
  }
  if (ev.key === "Enter" && ev.target.matches?.("tr[data-model]")) openDrawer(ev.target.dataset.model, ev.target);
}

function onClick(ev) {
  if (ev.target.closest('[data-action="drawer-close"]')) {
    closeDrawer();
    return;
  }
  if (ev.target.closest("#cat-drawer")) return; // clicks inside the drawer do nothing else
  const row = ev.target.closest("tr[data-model]");
  if (row) {
    openDrawer(row.dataset.model, row);
    return;
  }
  if (openModel && !ev.target.closest(".filter-chip, .toggle-row, [data-action]")) closeDrawer();
  const chip = ev.target.closest(".filter-chip");
  if (chip) {
    const c = chip.dataset.cap;
    if (filters.caps.has(c)) filters.caps.delete(c);
    else filters.caps.add(c);
    writeHash();
    render();
    return;
  }
  if (ev.target.closest('[data-action="clear"]')) {
    filters.node = null;
    filters.caps.clear();
    writeHash();
    render();
  }
}
function onChange(ev) {
  if (ev.target.id === "show-hidden") {
    filters.hidden = ev.target.checked;
    writeHash();
    render();
  }
  if (ev.target.id === "hide-cloud") {
    filters.cloud = ev.target.checked;
    writeHash();
    render();
  }
}

export default {
  id: "catalog",
  label: "Catalog",
  mount(r, c) {
    root = r;
    ctx = c;
    readHash();
    root.innerHTML = `<p class="loading">Loading&#8230;</p>`;
    root.addEventListener("click", onClick);
    root.addEventListener("change", onChange);
    document.addEventListener("keydown", onKey);
    ctx.poll(async () => {
      data = await ctx.api.get("/api/catalog");
      lastStateKey = mergeMetrics(data.node_metrics || {});
      render();
    }, 30000);
    // Live agent state and request counts; re-render only when they changed
    // so a reader's text selection is not torn up every two seconds.
    ctx.poll(async () => {
      if (!data) return;
      const key = mergeMetrics(await ctx.api.get("/api/node-metrics"));
      if (key !== lastStateKey) {
        lastStateKey = key;
        render();
      }
    }, 2000);
  },
  unmount() {
    if (root) {
      root.removeEventListener("click", onClick);
      root.removeEventListener("change", onChange);
    }
    document.removeEventListener("keydown", onKey);
    root = null;
    data = null;
    lastStateKey = "";
    openModel = null;
    fleetRoles = null;
    lastFocus = null;
  },
};
