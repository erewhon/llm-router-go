// Activity tab — "what is happening right now, and where did each request
// go?" A topology (callers → roles → seats/providers) with every in-flight
// request as a moving dot, and a Jobs table of recent requests underneath.
// PAIR's Overview shows node cards with live load and a Jobs list naming the
// serving node; the router knows more — the role asked for, the seat chosen,
// failovers, overflow, the principal — so those decisions are drawn as they
// happen. Data: /api/events (SSE), /api/fleet every 10 s (nodes, roles, live
// load), /api/catalog every 30 s (seats and verdicts), /api/node-metrics
// every 5 s. SVG + requestAnimationFrame, no library.
//
// The reducer half (applyEvent, buildTopology, placeEvent) is pure and
// exported so it can be tested in Node without a DOM.

// ---------------------------------------------------------------------------
// Pure state
// ---------------------------------------------------------------------------

export const CALLER_WINDOW_MS = 60_000;
export const JOBS_MAX = 200;

// newState builds the reducer's state.
export function newState() {
  return {
    inflight: new Map(), // request_id → {events: [started...], startedAt}
    jobs: [], // newest first; finished rows and in-flight rows
    callers: new Map(), // principal → last seen ms
  };
}

export function principalOf(e) {
  return e.principal || "(unattributed)";
}

// applyEvent folds one event in. Returns a list of "motions" the renderer
// turns into dots: {kind: "start"|"hop"|"finish", id, event}.
export function applyEvent(state, e, now = Date.now()) {
  const motions = [];
  const id = e.request_id || `anon-${now}-${Math.random()}`;
  state.callers.set(principalOf(e), now);
  if (e.type === "started") {
    const cur = state.inflight.get(id);
    if (cur) {
      cur.events.push(e);
      motions.push({ kind: "hop", id, event: e, from: cur.events[cur.events.length - 2] });
    } else {
      state.inflight.set(id, { events: [e], startedAt: now });
      motions.push({ kind: "start", id, event: e });
    }
    upsertJob(state, { ...e, request_id: id, inflight: true });
  } else if (e.type === "finished") {
    const cur = state.inflight.get(id);
    state.inflight.delete(id);
    motions.push({ kind: "finish", id, event: e, had: !!cur });
    upsertJob(state, { ...e, request_id: id, inflight: false });
  }
  return motions;
}

function upsertJob(state, row) {
  const i = state.jobs.findIndex((j) => j.request_id === row.request_id);
  if (i >= 0) state.jobs.splice(i, 1);
  state.jobs.unshift(row);
  if (state.jobs.length > JOBS_MAX) state.jobs.length = JOBS_MAX;
}

// activeCallers returns principals seen within the window, most recent first.
export function activeCallers(state, now = Date.now()) {
  const out = [];
  for (const [p, t] of state.callers) {
    if (now - t <= CALLER_WINDOW_MS) out.push({ principal: p, lastSeen: t });
    else state.callers.delete(p);
  }
  return out.sort((a, b) => b.lastSeen - a.lastSeen);
}

// providerKey normalises an api_base to the card it belongs on:
// "https://opencode.ai/zen/v1" → "opencode.ai/zen".
export function providerKey(apiBase) {
  if (!apiBase) return "";
  return apiBase
    .replace(/^https?:\/\//, "")
    .replace(/\/+$/, "")
    .replace(/\/v1$/, "");
}

const ROUTER_IDS = ["auto", "auto-free", "auto-full", "coder-resilient"];

// buildTopology derives the static picture from the merged /api/fleet +
// /api/catalog payload:
// nodes with their seats, providers with their models, roles.
export function buildTopology(data) {
  const nodes = data.nodes || {};
  const models = data.models || [];
  const nm = data.node_metrics || {};
  const seatsByNode = {};
  const providers = new Map();
  const modelById = {};
  for (const m of models) modelById[m.id] = m;
  for (const m of models) {
    if (m.enabled === false) continue;
    if (m.nodes && m.nodes.length) {
      const n = m.head_node || m.nodes[0];
      (seatsByNode[n] ||= []).push(m);
    } else if (m.backend === "external" && m.api_base && !ROUTER_IDS.includes(m.id)) {
      // Virtual chain entries have no api_base of their own: a request through
      // one resolves to a member, whose card gets the dot.
      const key = providerKey(m.api_base);
      if (!providers.has(key)) providers.set(key, { key, models: [], discovered: 0 });
      const p = providers.get(key);
      p.models.push(m);
      if (m.discovered) p.discovered++;
    }
  }
  const nodeList = Object.keys(nodes)
    .sort()
    .map((name) => ({
      name,
      def: nodes[name],
      metrics: nm[name] || {},
      seats: (seatsByNode[name] || []).sort((a, b) => a.id.localeCompare(b.id)),
    }));
  const roles = (data.roles || []).map((r) => r.role).sort();
  return { nodes: nodeList, providers: [...providers.values()].sort((a, b) => a.key.localeCompare(b.key)), roles, modelById };
}

// placeEvent says which router node and which target a started event lands
// on: {router: "coder"|"direct", node?: "hypatia", seat?: "qwen3.6-hypatia",
// provider?: "openrouter.ai/api"}.
export function placeEvent(topo, e) {
  const router = e.role || (e.chain ? "direct" : "direct");
  if (e.node) return { router, node: e.node, seat: e.resolved_via };
  const m = topo.modelById[e.resolved_via];
  if (m && m.api_base) return { router, provider: providerKey(m.api_base) };
  if (e.provider) {
    // Fall back to a host match when the id is unknown (a retired discovered id).
    const p = topo.providers.find((p) => p.key.startsWith(e.provider));
    return { router, provider: p ? p.key : e.provider };
  }
  return { router };
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

const W = 1200; // viewBox width; the SVG scales to the container
const COL = { caller: 110, router: 380, node: 640, provider: 1040 };
const NODE_W = 300,
  PROV_W = 260;

let root = null,
  ctx = null,
  state = null,
  topo = null,
  data = null;
let svg = null,
  staticG = null,
  dynG = null,
  jobsEl = null,
  noteEl = null;
let positions = {}; // key → {x, y}
let dots = new Map(); // request_id → dot animation
let raf = 0,
  stopSSE = null;
const filters = { role: "", node: "", principal: "", errors: false, inflight: false };
let selected = null;

const verdictColor = (m) => {
  if (!m) return "var(--text-dim)";
  switch (m.availability) {
    case "warming":
      return "var(--yellow)";
    case "absent":
      return "var(--red)";
    case "unavailable":
      return "var(--red)";
    default:
      return m.agent_state === "stopped" ? "var(--text-dim)" : "var(--green)";
  }
};

function readHash() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  const q = new URLSearchParams(qs || "");
  filters.role = q.get("role") || "";
  filters.node = q.get("node") || "";
  filters.principal = q.get("principal") || "";
  filters.errors = q.get("errors") === "1";
  filters.inflight = q.get("inflight") === "1";
}
function writeHash() {
  const q = new URLSearchParams();
  for (const k of ["role", "node", "principal"]) if (filters[k]) q.set(k, filters[k]);
  if (filters.errors) q.set("errors", "1");
  if (filters.inflight) q.set("inflight", "1");
  const s = q.toString();
  history.replaceState(null, "", `#activity${s ? "?" + s : ""}`);
}

const esc = (s) => ctx.fmt.escHtml(s);

// layoutStatic draws cards/nodes and records anchor positions.
function layoutStatic() {
  if (!topo) return;
  const callers = activeCallers(state);
  const rowH = 34;
  // Column heights decide the SVG height.
  let nodeY = 40;
  const nodeBoxes = topo.nodes.map((n) => {
    const h = 58 + Math.max(1, n.seats.length) * 16 + 8;
    const box = { ...n, y: nodeY, h };
    nodeY += h + 14;
    return box;
  });
  let provY = 40;
  const provBoxes = topo.providers.map((p) => {
    const h = 54 + Math.min(p.models.length, 8) * 14 + (p.models.length > 8 ? 14 : 0) + 8;
    const box = { ...p, y: provY, h };
    provY += h + 14;
    return box;
  });
  const routers = [...topo.roles, "direct"];
  const height = Math.max(nodeY, provY, 40 + Math.max(callers.length, routers.length) * rowH + 40, 320);
  svg.setAttribute("viewBox", `0 0 ${W} ${height}`);
  svg.style.height = `${Math.round((height * svg.clientWidth) / W)}px`;

  positions = {};
  let g = "";
  // Callers
  g += `<text x="${COL.caller}" y="22" class="act-col" text-anchor="middle">callers (last 60 s)</text>`;
  callers.forEach((c, i) => {
    const y = 50 + i * rowH;
    positions[`caller:${c.principal}`] = { x: COL.caller, y };
    g += `<g class="act-caller" data-principal="${esc(c.principal)}"><circle cx="${COL.caller}" cy="${y}" r="9"/><text x="${COL.caller}" y="${y + 22}" text-anchor="middle">${esc(c.principal.length > 22 ? c.principal.slice(0, 21) + "…" : c.principal)}</text></g>`;
  });
  if (!callers.length) g += `<text x="${COL.caller}" y="60" class="act-dim" text-anchor="middle">no requests yet</text>`;
  // Router column
  g += `<text x="${COL.router}" y="22" class="act-col" text-anchor="middle">router</text>`;
  routers.forEach((r, i) => {
    const y = 50 + i * rowH;
    positions[`router:${r}`] = { x: COL.router, y };
    g += `<g class="act-role ${r === "direct" ? "act-direct" : ""}" data-role="${esc(r)}"><rect x="${COL.router - 56}" y="${y - 12}" width="112" height="24" rx="6"/><text x="${COL.router}" y="${y + 4}" text-anchor="middle">${esc(r)}</text></g>`;
  });
  // Fleet / provider boundary
  const bx = COL.provider - 60;
  g += `<line x1="${bx}" y1="30" x2="${bx}" y2="${height - 10}" class="act-boundary"/><text x="${bx + 6}" y="${height - 14}" class="act-dim">leaves the building →</text>`;
  // Node cards
  g += `<text x="${COL.node + NODE_W / 2}" y="22" class="act-col" text-anchor="middle">fleet</text>`;
  for (const n of nodeBoxes) {
    const x = COL.node,
      m = n.metrics;
    const reachable = m.reachable === true;
    positions[`node:${n.name}`] = { x: x + NODE_W / 2, y: n.y + 20 };
    g += `<g class="act-node ${reachable ? "" : "act-node-down"}" data-node="${esc(n.name)}"><rect x="${x}" y="${n.y}" width="${NODE_W}" height="${n.h}" rx="8"/>`;
    g += `<text x="${x + 12}" y="${n.y + 20}" class="act-node-name">${esc(n.name)}${reachable ? "" : " · unreachable"}</text>`;
    const memPct = m.vram_pct ?? m.ram_pct ?? null,
      busy = m.gpu_busy_pct ?? null;
    const bar = (label, pct, yy, color) =>
      pct == null
        ? ""
        : `<text x="${x + 12}" y="${yy + 8}" class="act-bar-label">${label}</text><rect x="${x + 44}" y="${yy}" width="${NODE_W - 100}" height="8" rx="4" class="act-bar-track"/><rect x="${x + 44}" y="${yy}" width="${Math.max(0, ((NODE_W - 100) * Math.min(100, pct)) / 100)}" height="8" rx="4" style="fill:${color}"/><text x="${x + NODE_W - 12}" y="${yy + 8}" text-anchor="end" class="act-bar-label">${Math.round(pct)}%</text>`;
    g += bar("mem", memPct, n.y + 30, memPct >= 90 ? "var(--red)" : memPct >= 70 ? "var(--yellow)" : "var(--accent)");
    g += bar("gpu", busy, n.y + 44, busy >= 90 ? "var(--red)" : busy >= 50 ? "var(--yellow)" : "var(--green)");
    n.seats.forEach((s, i) => {
      const yy = n.y + 66 + i * 16;
      positions[`seat:${s.id}`] = { x: x + 20, y: yy - 4 };
      const dead = s.availability === "absent";
      g += `<g class="act-seat" data-seat="${esc(s.id)}"><circle cx="${x + 20}" cy="${yy - 4}" r="4" style="fill:${verdictColor(s)}"/><text x="${x + 30}" y="${yy}" class="act-seat-name" ${dead ? 'text-decoration="line-through"' : ""}>${esc(s.id)}</text>${s.availability && s.availability !== "available" ? `<text x="${x + NODE_W - 12}" y="${yy}" text-anchor="end" class="act-dim">${esc(s.availability)}</text>` : ""}</g>`;
    });
    if (!n.seats.length) g += `<text x="${x + 20}" y="${n.y + 66}" class="act-dim">no seats</text>`;
    g += `</g>`;
  }
  // Provider cards
  g += `<text x="${COL.provider + PROV_W / 2}" y="22" class="act-col" text-anchor="middle">providers</text>`;
  for (const p of provBoxes) {
    const x = COL.provider;
    positions[`provider:${p.key}`] = { x: x + PROV_W / 2, y: p.y + 20 };
    g += `<g class="act-provider" data-provider="${esc(p.key)}"><rect x="${x}" y="${p.y}" width="${PROV_W}" height="${p.h}" rx="8"/><text x="${x + 12}" y="${p.y + 20}" class="act-node-name">${esc(p.key)}</text><text x="${x + 12}" y="${p.y + 38}" class="act-dim">${p.models.length} models${p.discovered ? ` · ${p.discovered} discovered` : ""}</text>`;
    p.models.slice(0, 8).forEach((m, i) => {
      const yy = p.y + 58 + i * 14;
      positions[`seat:${m.id}`] = { x: x + 20, y: yy - 4 };
      g += `<g class="act-seat" data-seat="${esc(m.id)}"><circle cx="${x + 20}" cy="${yy - 4}" r="3" style="fill:${verdictColor(m)}"/><text x="${x + 30}" y="${yy}" class="act-seat-name">${esc(m.id)}</text></g>`;
    });
    if (p.models.length > 8) g += `<text x="${x + 30}" y="${p.y + 58 + 8 * 14}" class="act-dim">+${p.models.length - 8} more</text>`;
    g += `</g>`;
  }
  staticG.innerHTML = g;
}

// targetPos resolves where a started event's dot ends up.
function targetPos(e) {
  const pl = placeEvent(topo, e);
  const seat = pl.seat && positions[`seat:${pl.seat}`];
  if (seat) return { pos: seat, kind: "seat", crosses: false };
  if (pl.node && positions[`node:${pl.node}`]) return { pos: positions[`node:${pl.node}`], kind: "node", crosses: false };
  if (pl.provider && positions[`provider:${pl.provider}`]) return { pos: positions[`provider:${pl.provider}`], kind: "provider", crosses: true };
  return { pos: { x: COL.node - 40, y: 40 }, kind: "unknown", crosses: false };
}
function routerPos(e) {
  const r = e.role || "direct";
  return positions[`router:${r}`] || positions["router:direct"] || { x: COL.router, y: 50 };
}
function callerPos(e) {
  return positions[`caller:${principalOf(e)}`] || { x: COL.caller, y: 50 };
}

const HOP_MS = 600;

// Dot lifecycle: travel along path segments, then orbit the target until
// finished, then a fading label.
function startDot(id, e) {
  const from = callerPos(e),
    via = routerPos(e),
    tgt = targetPos(e);
  dots.set(id, {
    id,
    e,
    path: [from, via, tgt.pos],
    crosses: tgt.crosses,
    t0: performance.now(),
    hops: [],
    phase: "travel",
    orbit: 0,
    label: null,
    labelAt: 0,
    stream: !!e.stream,
    error: false,
    overflow: !!e.overflowed,
  });
}
function hopDot(id, e, fromEvent) {
  let d = dots.get(id);
  const tgt = targetPos(e);
  if (!d) {
    // Late join: no first hop drawn; start from the router.
    d = {
      id,
      e,
      path: [routerPos(e), tgt.pos],
      crosses: tgt.crosses,
      t0: performance.now(),
      hops: [],
      phase: "travel",
      orbit: 0,
      label: null,
      labelAt: 0,
      stream: !!e.stream,
      error: false,
      overflow: false,
    };
    dots.set(id, d);
    return;
  }
  const prev = d.path[d.path.length - 1];
  d.hops.push({ from: prev, to: tgt.pos, failed: true });
  d.e = e;
  d.path = [prev, tgt.pos];
  d.crosses = d.crosses || tgt.crosses;
  d.t0 = performance.now();
  d.phase = "travel";
}
function finishDot(id, e) {
  const d = dots.get(id);
  const failed = e.status >= 500 || (!!e.error_class && !/refused/.test(e.error_class));
  if (!d) {
    if (!e.resolved_via) return; // a rejected request never had a target; the Jobs table shows it
    // Finished without a seen start (page joined mid-request): flash the target.
    const tgt = targetPos(e);
    dots.set(id, {
      id,
      e,
      path: [tgt.pos, tgt.pos],
      crosses: false,
      t0: performance.now() - HOP_MS,
      hops: [],
      phase: "done",
      orbit: 0,
      label: labelFor(e),
      labelAt: performance.now(),
      stream: false,
      error: failed,
      overflow: !!e.overflowed,
    });
    return;
  }
  d.phase = "done";
  d.error = failed;
  d.overflow = d.overflow || !!e.overflowed;
  d.label = labelFor(e);
  d.labelAt = performance.now();
}
function labelFor(e) {
  const parts = [];
  if (e.completion_tokens != null)
    parts.push(`${e.completion_tokens >= 1000 ? (e.completion_tokens / 1000).toFixed(1) + "k" : e.completion_tokens} tok`);
  if (e.latency_ms != null) parts.push(e.latency_ms >= 1000 ? `${(e.latency_ms / 1000).toFixed(1)} s` : `${e.latency_ms} ms`);
  if (e.status >= 400) parts.push(`${e.status}${e.error_class ? " " + e.error_class : ""}`);
  return parts.join(" · ") || (e.status ? String(e.status) : "");
}

function lerp(a, b, t) {
  return { x: a.x + (b.x - a.x) * t, y: a.y + (b.y - a.y) * t };
}

// frame draws the dynamic layer. Cheap: tens of dots, not thousands.
function frame(now) {
  raf = requestAnimationFrame(frame);
  if (!dynG) return;
  let out = "";
  for (const [id, d] of dots) {
    const age = now - d.t0;
    const segs = d.path.length - 1;
    const total = segs * HOP_MS;
    const color = d.error ? "var(--red)" : d.overflow || d.crosses ? "var(--yellow)" : "var(--accent)";
    // Failed hops, drawn red and dashed.
    for (const h of d.hops) out += `<line x1="${h.from.x}" y1="${h.from.y}" x2="${h.to.x}" y2="${h.to.y}" class="act-hop-failed"/>`;
    // The lit path so far.
    if (segs >= 1) {
      const p = Math.min(1, age / total);
      let pathD = `M ${d.path[0].x} ${d.path[0].y}`;
      for (let i = 1; i <= segs; i++) {
        const segT = Math.min(1, Math.max(0, p * segs - (i - 1)));
        const to = lerp(d.path[i - 1], d.path[i], segT);
        pathD += ` L ${to.x} ${to.y}`;
        if (segT < 1) break;
      }
      out += `<path d="${pathD}" class="act-trail ${d.phase === "done" ? "act-trail-done" : ""}" style="stroke:${color}"/>`;
    }
    let pos;
    if (age < total) {
      const p = age / total,
        i = Math.min(segs - 1, Math.floor(p * segs));
      pos = lerp(d.path[i], d.path[i + 1], p * segs - i);
    } else {
      const end = d.path[d.path.length - 1];
      if (d.phase === "travel") d.phase = "orbit";
      if (d.phase === "orbit") {
        const a = ((now - d.t0 - total) / 900) * Math.PI * 2;
        pos = { x: end.x + Math.cos(a) * 10, y: end.y + Math.sin(a) * 10 };
        if (d.stream) out += `<circle cx="${end.x}" cy="${end.y}" r="${8 + 4 * Math.sin(now / 150)}" class="act-pulse"/>`;
      } else pos = end;
    }
    const sel = selected === id ? " act-dot-selected" : "";
    if (d.phase !== "done" || now - d.labelAt < 2500) {
      out += `<circle cx="${pos.x}" cy="${pos.y}" r="${d.phase === "done" ? 4 : 6}" class="act-dot${sel}" style="fill:${color}" data-id="${esc(id)}"/>`;
      if (d.label)
        out += `<text x="${pos.x + 10}" y="${pos.y - 8}" class="act-label" style="fill:${color};opacity:${Math.max(0, 1 - (now - d.labelAt) / 2500)}">${esc(d.label)}</text>`;
    } else {
      dots.delete(id);
    }
  }
  dynG.innerHTML = out;
}

// ---------------------------------------------------------------------------
// Jobs table
// ---------------------------------------------------------------------------

function jobRows() {
  return state.jobs.filter((j) => {
    if (filters.role && (j.role || "") !== filters.role) return false;
    if (filters.node && (j.node || j.provider || "") !== filters.node) return false;
    if (filters.principal && principalOf(j) !== filters.principal) return false;
    if (filters.errors && !(j.status >= 400 || j.error_class)) return false;
    if (filters.inflight && !j.inflight) return false;
    return true;
  });
}

function renderJobs() {
  if (!jobsEl) return;
  const rows = jobRows();
  const chip = (k, v, label) =>
    `<span class="filter-chip${filters[k] === v || (typeof v === "boolean" && filters[k] === v) ? " active" : ""}" data-filter="${k}" data-value="${esc(String(v))}">${esc(label)}</span>`;
  const roles = [...new Set(state.jobs.map((j) => j.role).filter(Boolean))].sort();
  const places = [...new Set(state.jobs.map((j) => j.node || j.provider).filter(Boolean))].sort();
  const principals = [...new Set(state.jobs.map(principalOf))].sort();
  let h = `<div class="section-title">Jobs <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(last ${state.jobs.length} requests on this replica)</span></div><div class="filter-bar">`;
  for (const r of roles) h += chip("role", r, r);
  for (const p of places) h += chip("node", p, p);
  for (const p of principals) h += chip("principal", p, p);
  h += chip("errors", true, "errors only") + chip("inflight", true, "in flight");
  if (filters.role || filters.node || filters.principal || filters.errors || filters.inflight)
    h += `<button class="clear-btn" data-action="clear">clear filters</button>`;
  h += `</div><div class="node-card" style="padding:0.4rem 0.8rem;overflow-x:auto"><table class="usage-table act-jobs"><thead><tr><th>time</th><th>principal</th><th>asked</th><th>resolved via</th><th>where</th><th>failover</th><th class="num">tokens</th><th class="num">latency</th><th>status</th><th>privacy</th></tr></thead><tbody>`;
  for (const j of rows) {
    const t = j.ts ? new Date(j.ts) : null;
    const time = t ? t.toLocaleTimeString([], { hour12: false }) : "";
    const status = j.inflight
      ? `<span style="color:var(--yellow)">in flight${j.stream ? " ▶" : ""}</span>`
      : j.error_class && /refused/.test(j.error_class)
        ? `<span style="color:var(--orange)">${esc(j.error_class)}</span>`
        : j.status >= 400
          ? `<span style="color:var(--red)">${j.status}${j.error_class ? " " + esc(j.error_class) : ""}</span>`
          : `${j.status}${j.stream ? " ▶" : ""}`;
    const tokens = j.prompt_tokens != null || j.completion_tokens != null ? `${j.prompt_tokens ?? "–"}/${j.completion_tokens ?? "–"}` : "";
    const lat = j.latency_ms != null ? (j.latency_ms >= 1000 ? `${(j.latency_ms / 1000).toFixed(1)} s` : `${j.latency_ms} ms`) : "";
    h += `<tr data-id="${esc(j.request_id)}" class="${selected === j.request_id ? "act-row-selected" : ""}"><td>${time}</td><td>${esc(principalOf(j))}</td><td>${esc(j.model || "")}</td><td>${esc(j.resolved_via || "")}${j.discovered ? ' <span class="badge badge-tag">discovered</span>' : ""}</td><td>${esc(j.node || j.provider || "")}${j.upstream_provider ? ` <span class="act-dim">${esc(j.upstream_provider)}</span>` : ""}</td><td>${esc(j.failover_from || "")}${j.overflowed ? ' <span class="badge badge-tool">overflow</span>' : ""}</td><td class="num">${tokens}</td><td class="num">${lat}</td><td>${status}</td><td>${esc(j.privacy_tolerance || "")}</td></tr>`;
  }
  if (!rows.length) h += `<tr><td colspan="10" class="act-dim">no requests${state.jobs.length ? " match the filters" : " yet"}</td></tr>`;
  h += `</tbody></table></div>`;
  jobsEl.innerHTML = h;
}

function onClick(ev) {
  const chip = ev.target.closest(".filter-chip[data-filter]");
  if (chip) {
    const k = chip.dataset.filter,
      v = chip.dataset.value;
    if (k === "errors" || k === "inflight") filters[k] = !filters[k];
    else filters[k] = filters[k] === v ? "" : v;
    writeHash();
    renderJobs();
    return;
  }
  if (ev.target.closest('[data-action="clear"]')) {
    Object.assign(filters, { role: "", node: "", principal: "", errors: false, inflight: false });
    writeHash();
    renderJobs();
    return;
  }
  const row = ev.target.closest("tr[data-id]");
  if (row) {
    selected = selected === row.dataset.id ? null : row.dataset.id;
    root.dispatchEvent(new CustomEvent("activity:select", { detail: selected }));
    renderJobs();
    return;
  }
  const caller = ev.target.closest(".act-caller");
  if (caller) {
    filters.principal = filters.principal === caller.dataset.principal ? "" : caller.dataset.principal;
    writeHash();
    renderJobs();
    return;
  }
  const role = ev.target.closest(".act-role");
  if (role && role.dataset.role !== "direct") {
    filters.role = filters.role === role.dataset.role ? "" : role.dataset.role;
    writeHash();
    renderJobs();
    return;
  }
  const node = ev.target.closest(".act-node, .act-provider");
  if (node) {
    const v = node.dataset.node || node.dataset.provider;
    filters.node = filters.node === v ? "" : v;
    writeHash();
    renderJobs();
  }
}

// ---------------------------------------------------------------------------
// Tab
// ---------------------------------------------------------------------------

export default {
  id: "activity",
  label: "Activity",
  mount(r, c) {
    root = r;
    ctx = c;
    state = newState();
    topo = null;
    data = null;
    dots = new Map();
    selected = null;
    readHash();
    root.innerHTML = `
      <div class="section-title">Live activity <span id="act-note" class="act-dim" style="font-weight:400;font-size:0.8rem"></span></div>
      <div class="act-wrap"><svg id="act-svg" class="act-svg" viewBox="0 0 ${W} 320" preserveAspectRatio="xMinYMin meet"><g id="act-static"></g><g id="act-dyn"></g></svg></div>
      <div id="act-jobs"></div>`;
    svg = root.querySelector("#act-svg");
    staticG = root.querySelector("#act-static");
    dynG = root.querySelector("#act-dyn");
    jobsEl = root.querySelector("#act-jobs");
    noteEl = root.querySelector("#act-note");
    root.addEventListener("click", onClick);
    ctx.api
      .get("/api/overview")
      .then((d) => {
        if (noteEl) noteEl.textContent = `showing this replica's traffic (${d.replica || "?"})`;
      })
      .catch(() => {});
    // data is the merge of the two registry-shaped payloads; whichever
    // arrives rebuilds the topology.
    ctx.poll(async () => {
      const f = await ctx.api.get("/api/fleet");
      data = { ...(data || {}), nodes: f.nodes, node_metrics: f.node_metrics, roles: f.roles };
      if (data.models) {
        topo = buildTopology(data);
        layoutStatic();
      }
    }, 10000);
    ctx.poll(async () => {
      const c = await ctx.api.get("/api/catalog");
      data = { ...(data || {}), models: c.models };
      if (data.nodes) {
        topo = buildTopology(data);
        layoutStatic();
      }
    }, 30000);
    ctx.poll(async () => {
      if (!data || !data.nodes || !data.models) return;
      data.node_metrics = await ctx.api.get("/api/node-metrics");
      const state2 = {};
      for (const m of Object.values(data.node_metrics)) for (const mdl of m.models || []) state2[mdl.model_id] = mdl.state;
      for (const m of data.models || []) m.agent_state = state2[m.id] || null;
      topo = buildTopology(data);
      layoutStatic();
    }, 5000);
    // Callers age out; refresh the static layer every few seconds for that.
    ctx.poll(async () => {
      if (topo) layoutStatic();
    }, 5000);
    stopSSE = ctx.api.sse("/api/events", {
      snapshot: (snap) => {
        const now = Date.now();
        for (const e of snap.recent || []) applyEvent(state, e, e.ts ? Date.parse(e.ts) : now);
        for (const e of snap.inflight || []) {
          for (const m of applyEvent(state, e, now)) if (m.kind === "start" && topo) startDot(m.id, m.event);
        }
        // Replay the last few so the view is not empty on arrival.
        if (topo) for (const j of state.jobs.slice(0, 30).reverse()) if (!j.inflight && j.resolved_via) finishDot(j.request_id, j);
        if (topo) layoutStatic();
        renderJobs();
      },
      request: (e) => {
        for (const m of applyEvent(state, e)) {
          if (!topo) continue;
          if (m.kind === "start") startDot(m.id, m.event);
          else if (m.kind === "hop") hopDot(m.id, m.event, m.from);
          else if (m.kind === "finish") finishDot(m.id, m.event);
        }
        if (topo && !positions[`caller:${principalOf(e)}`]) layoutStatic();
        renderJobs();
      },
      error: () => {
        if (noteEl) noteEl.textContent = "event stream disconnected — reconnecting…";
      },
      open: () => {
        if (noteEl && /disconnected/.test(noteEl.textContent)) noteEl.textContent = "reconnected";
      },
    });
    raf = requestAnimationFrame(frame);
    renderJobs();
  },
  unmount() {
    if (stopSSE) stopSSE();
    stopSSE = null;
    cancelAnimationFrame(raf);
    if (root) root.removeEventListener("click", onClick);
    root = svg = staticG = dynG = jobsEl = noteEl = null;
    topo = null;
    data = null;
  },
};
