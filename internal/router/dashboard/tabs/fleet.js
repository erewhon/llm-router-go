// Fleet tab — "what is up right now, and what does each role mean at this
// moment?" Nodes (live load), Roles (bindings), Live inventory (per-base
// listings). Ported behaviour-for-behaviour from the legacy render() on
// 2026-09-12; data still comes from /api/models + /api/node-metrics until the
// /api/fleet split leaf lands.

let root = null;
let ctx = null;
let data = null; // last /api/models payload
let selectedNode = null; // tab-local, mirrored into #fleet?node=
const nodeHistory = {}; // per node: {vram: [], gpu: []} sparkline samples
const SPARK_MAX = 60; // ~2 min at 2 s intervals

function readHash() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  return new URLSearchParams(qs || "");
}
function writeHash() {
  const q = new URLSearchParams();
  if (selectedNode) q.set("node", selectedNode);
  const s = q.toString();
  history.replaceState(null, "", `#fleet${s ? "?" + s : ""}`);
}

function updateHistory(nm) {
  for (const [name, m] of Object.entries(nm)) {
    if (!nodeHistory[name]) nodeHistory[name] = { vram: [], gpu: [] };
    if (!m.reachable) continue;
    // memory-utilisation history: VRAM on GPU nodes, RAM on CPU-only nodes
    const memPct = m.vram_pct != null ? m.vram_pct : m.ram_pct != null ? m.ram_pct : null;
    if (memPct !== null) {
      nodeHistory[name].vram.push(memPct);
      if (nodeHistory[name].vram.length > SPARK_MAX) nodeHistory[name].vram.shift();
    }
    if (m.gpu_busy_pct !== null && m.gpu_busy_pct !== undefined) {
      nodeHistory[name].gpu.push(m.gpu_busy_pct);
      if (nodeHistory[name].gpu.length > SPARK_MAX) nodeHistory[name].gpu.shift();
    }
  }
}

function bar(label, pct, color, text, spark = "") {
  return `
        <div class="metric-row">
          <span class="metric-label">${label}</span>
          <div class="vram-bar-track"><div class="vram-bar-fill ${color}" style="width:${pct}%"></div></div>
          <span class="vram-text">${text}</span>
          ${spark}
        </div>`;
}
const busyColor = (b) => (b >= 90 ? "red" : b >= 50 ? "yellow" : "green");

function renderStats(nodes, nm) {
  // Fleet VRAM totals, and CPU RAM summed over nodes whose RAM is a distinct
  // pool from VRAM (unified-memory Sparks are already in the VRAM figure).
  let vT = 0,
    vU = 0,
    rT = 0,
    rU = 0;
  for (const [name, m] of Object.entries(nm)) {
    if (!m.reachable) continue;
    if (m.vram_total_gb) {
      vT += m.vram_total_gb;
      vU += m.vram_used_gb || 0;
    }
    const node = nodes[name] || {};
    if (m.ram_total_gb && !node.unified_memory) {
      rT += m.ram_total_gb;
      rU += m.ram_used_gb || 0;
    }
  }
  const reachable = Object.values(nm).filter((m) => m.reachable).length;
  const tile = (v, l) => `<div class="stat"><div class="stat-value">${v}</div><div class="stat-label">${l}</div></div>`;
  const dim = (s) => `<span style="font-size:0.9rem;color:var(--text-dim)">${s}</span>`;
  return `<div class="stats">
    ${tile(`${reachable}${dim(` / ${Object.keys(nodes).length}`)}`, "Nodes reachable")}
    ${tile(`${vU.toFixed(0)}${dim(` / ${vT.toFixed(0)} GB`)}`, "Fleet VRAM")}
    ${tile(`${rU.toFixed(0)}${dim(` / ${rT.toFixed(0)} GB`)}`, "Fleet CPU RAM")}
  </div>`;
}

function renderNodes(nodes, nm) {
  const { sparklineSvg, vramBarColor, escHtml } = ctx.fmt;
  let html = `<div class="section-title">Nodes</div><div class="node-grid">`;
  for (const [name, n] of Object.entries(nodes)) {
    const m = nm[name] || {};
    const reachable = m.reachable === true;
    const isActive = selectedNode === name;
    // A node powered down inside its declared window is planned, not broken.
    const scheduledOff = !reachable && n.expected_down === true;
    const nodeBadge = scheduledOff
      ? ` <span class="badge badge-off">off (scheduled)</span>`
      : !reachable
        ? ` <span class="badge" style="background:rgba(248,113,113,0.15);color:var(--red)">unreachable</span>`
        : "";
    html += `<div class="node-card${isActive ? " active" : ""}" data-node="${escHtml(name)}">
        <div class="node-name">${escHtml(name)}${nodeBadge}</div>
        <div class="node-detail">Host: <span>${escHtml(n.host)}</span></div>
        ${
          n.gpu === "none"
            ? `<div class="node-detail">Compute: <span>CPU</span> &middot; <span>no GPU</span></div>`
            : `<div class="node-detail">GPU: <span>${escHtml(String(n.gpu).toUpperCase())}</span> &middot; <span>${n.vram_gb} GB</span></div>`
        }`;
    const hist = nodeHistory[name] || { vram: [], gpu: [] };
    if (reachable && n.gpu === "none" && m.ram_pct != null) {
      // CPU-only node: RAM is the memory signal in place of VRAM.
      html += bar(
        "RAM",
        m.ram_pct,
        vramBarColor(m.ram_pct),
        `<strong>${m.ram_used_gb}</strong> / ${m.ram_total_gb} GB`,
        sparklineSvg(hist.vram, "var(--accent)"),
      );
    } else if (reachable && m.gpus && m.gpus.length > 1) {
      // Multi-GPU node (talos 2x B70): one MEM + busy pair per card.
      for (const g of m.gpus) {
        html += bar(
          `MEM${g.index}`,
          g.vram_pct,
          vramBarColor(g.vram_pct),
          `<strong>${g.vram_used_gb}</strong> / ${g.vram_total_gb} GB`,
        );
        if (g.busy_pct != null)
          html += bar(`GPU${g.index}`, g.busy_pct, busyColor(g.busy_pct), `<strong>${g.busy_pct}%</strong>`);
      }
      html += `<div class="metric-row"><span class="metric-label">ALL</span>${sparklineSvg(hist.vram, "var(--accent)")}${sparklineSvg(hist.gpu, "var(--green)")}</div>`;
    } else if (reachable && m.vram_pct !== null && m.vram_pct !== undefined) {
      html += bar(
        "MEM",
        m.vram_pct,
        vramBarColor(m.vram_pct),
        `<strong>${m.vram_used_gb}</strong> / ${m.vram_total_gb} GB`,
        sparklineSvg(hist.vram, "var(--accent)"),
      );
      if (m.gpu_busy_pct != null)
        html += bar(
          "GPU",
          m.gpu_busy_pct,
          busyColor(m.gpu_busy_pct),
          `<strong>${m.gpu_busy_pct}%</strong>`,
          sparklineSvg(hist.gpu, "var(--green)"),
        );
    } else if (reachable) {
      html += `<div class="node-offline">No metrics</div>`;
    } else {
      html += `<div class="node-offline">Agent offline</div>`;
    }
    if (reachable && m.disk_free_gb != null && m.disk_total_gb != null) {
      const used = m.disk_total_gb - m.disk_free_gb;
      const pct = ((used / m.disk_total_gb) * 100).toFixed(1);
      html += bar(
        "DISK",
        pct,
        pct >= 90 ? "red" : pct >= 75 ? "yellow" : "green",
        `<strong>${used.toFixed(0)}</strong> / ${m.disk_total_gb.toFixed(0)} GB (${m.disk_free_gb.toFixed(0)} free)`,
      );
    }
    for (const svc of m.services || []) {
      const dot = svc.reachable ? "health-running" : "health-stopped";
      html += `<div style="margin-top:.5rem;padding-top:.5rem;border-top:1px solid var(--border)">
        <div style="font-size:0.8rem;font-weight:500"><span class="health-dot ${dot}"></span>${escHtml(svc.label || svc.name)}</div>`;
      if (svc.reachable && svc.vram_used_gb != null && svc.vram_total_gb != null) {
        const p = ((svc.vram_used_gb / svc.vram_total_gb) * 100).toFixed(1);
        html += bar("MEM", p, vramBarColor(parseFloat(p)), `<strong>${svc.vram_used_gb}</strong> / ${svc.vram_total_gb} GB`);
      }
      if (svc.queue_running > 0 || svc.queue_pending > 0)
        html += `<div style="font-size:0.7rem;color:var(--text-dim);margin-top:2px">Queue: ${svc.queue_running} running, ${svc.queue_pending} pending</div>`;
      html += `</div>`;
    }
    if (isActive)
      html += `<div class="node-detail" style="margin-top:.5rem"><a href="#catalog?node=${encodeURIComponent(name)}">models on ${escHtml(name)} &rarr;</a></div>`;
    html += `</div>`;
  }
  return html + `</div>`;
}

function renderRoles(roles, models) {
  const { escHtml, isRouterModel } = ctx.fmt;
  if (!roles.length) return "";
  const modelById = {};
  for (const m of models) modelById[m.id] = m;
  let html = `<div class="section-title">Roles</div><div class="roles">`;
  for (const r of roles) {
    const cls = !r.available ? " role-down" : r.overflowed ? " role-overflow" : "";
    const target = r.available
      ? `<span style="color:var(--green)">&#9679;</span> ${escHtml(r.target)}`
      : `<span style="color:var(--red)">&#9679;</span> <span style="color:var(--red)">no model available</span>`;
    html += `<div class="role-card${cls}">
        <div class="role-name">${escHtml(r.role)}${r.overflowed ? ' <span class="badge badge-tool">overflow</span>' : ""}</div>
        <div class="role-target">${target}</div>`;
    if (r.description) html += `<div class="role-desc">${escHtml(r.description)}</div>`;
    const hops = [];
    for (const c of r.candidates || []) hops.push({ id: c, overflow: false });
    for (const c of r.overflow || []) hops.push({ id: c, overflow: true });
    if (hops.length) {
      html += `<div class="role-chain">`;
      for (const h of hops) {
        const m = modelById[h.id];
        const up = m && (m.agent_state === "running" || m.agent_state == null || isRouterModel(m));
        let hopCls = h.overflow ? "role-hop-overflow" : up ? "role-hop-up" : "role-hop-down";
        // A chain candidate serves via a provider entry: match the suffix too.
        if (h.id === r.target || (r.target || "").endsWith("/" + h.id)) hopCls = "role-hop-active";
        html += `<span class="role-hop ${hopCls}">${escHtml(h.id)}</span>`;
      }
      html += `</div>`;
    }
    if (!r.available && (r.reasons || []).length)
      html += `<div class="role-desc" style="color:var(--red)">${escHtml(r.reasons.join("; "))}</div>`;
    html += `</div>`;
  }
  return html + `</div>`;
}

function renderInventory(inventory) {
  const { escHtml, fmtAgoS } = ctx.fmt;
  if (!inventory.length) return "";
  const absentTotal = inventory.reduce((n, b) => n + (b.absent || []).length, 0);
  const discTotal = inventory.reduce((n, b) => n + (b.discovered || []).length, 0);
  const staleTotal = inventory.filter((b) => b.error).length;
  let html = `<div class="section-title">Live inventory <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(${inventory.length} bases &middot; ${discTotal} discovered &middot; ${absentTotal} absent${staleTotal ? ` &middot; ${staleTotal} unreachable` : ""})</span></div><div class="roles">`;
  for (const b of inventory) {
    const absent = b.absent || [],
      disc = b.discovered || [];
    const cls = absent.length ? " role-down" : b.error ? " role-overflow" : "";
    html += `<div class="role-card${cls}">
        <div class="role-name" style="font-size:0.8rem;word-break:break-all">${escHtml(b.base)}</div>
        <div class="role-target">${b.ids} listed &middot; ${(b.members || []).length} registered &middot; ${b.age_s == null ? "never fetched" : fmtAgoS(b.age_s)}${b.stale ? ' <span class="badge badge-off">stale</span>' : ""}</div>`;
    if (b.error) html += `<div class="role-desc" style="color:var(--red)">${escHtml(b.error)}</div>`;
    if (absent.length)
      html += `<div class="role-desc">absent (registered here, not in the listing)</div><div class="role-chain">${absent.map((id) => `<span class="role-hop role-hop-down">${escHtml(id)}</span>`).join("")}</div>`;
    if (disc.length)
      html += `<div class="role-desc">discovered (adopted from the listing)</div><div class="role-chain">${disc.map((id) => `<span class="role-hop role-hop-active">${escHtml(id)}</span>`).join("")}</div>`;
    html += `</div>`;
  }
  return html + `</div>`;
}

function render() {
  if (!root || !data) return;
  const nodes = data.nodes || {};
  const nm = data.node_metrics || {};
  root.innerHTML =
    renderStats(nodes, nm) +
    renderRoles(data.roles || [], data.models || []) +
    renderInventory(data.inventory || []) +
    renderNodes(nodes, nm);
}

export default {
  id: "fleet",
  label: "Fleet",
  mount(r, c) {
    root = r;
    ctx = c;
    selectedNode = readHash().get("node");
    root.innerHTML = `<p class="loading">Loading&#8230;</p>`;
    root.addEventListener("click", onClick);
    // Registry-shaped things (nodes, roles, inventory) every 30 s; live load
    // every 2 s, merged into the last payload the way the legacy page did.
    ctx.poll(async () => {
      data = await ctx.api.get("/api/models");
      updateHistory(data.node_metrics || {});
      render();
    }, 30000);
    ctx.poll(async () => {
      if (!data) return;
      const nm = await ctx.api.get("/api/node-metrics");
      data.node_metrics = nm;
      // Fresh agent state per model, so the Roles hops colour correctly.
      const state = {};
      for (const m of Object.values(nm)) for (const mdl of m.models || []) state[mdl.model_id] = mdl.state;
      for (const m of data.models || []) m.agent_state = state[m.id] || null;
      updateHistory(nm);
      render();
    }, 2000);
  },
  unmount() {
    if (root) root.removeEventListener("click", onClick);
    root = null;
    data = null;
  },
};

function onClick(ev) {
  if (ev.target.closest("a")) return; // the "models on X" link navigates
  const card = ev.target.closest(".node-card");
  if (!card) return;
  const name = card.dataset.node;
  selectedNode = selectedNode === name ? null : name;
  writeHash();
  render();
}
