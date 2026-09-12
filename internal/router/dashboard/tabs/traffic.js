// Traffic tab — "what happened?" The Router counters card, Usage by user
// (from the request log) and Upstream failures (rolling 24 h ring since
// restart). Ported behaviour-for-behaviour from the legacy render() on
// 2026-09-12. Each card polls and renders on its own, so one slow endpoint
// never blanks the others.
//
// TODO(traffic-from-reqlog): the Phase 3 leaf adds a 1h/24h/7d window
// selector here and replaces the since-restart sources with /api/traffic.

let root = null;
let ctx = null;
let routerPrev = null; // previous /api/router-metrics sample, for rates
const cards = { router: "", usage: "", upstream: "" };

function paint() {
  if (!root) return;
  root.innerHTML =
    (cards.router || `<div class="section-title">Router</div><p class="tab-placeholder">waiting for /api/router-metrics…</p>`) +
    cards.usage +
    cards.upstream;
}

function renderRouter(rt) {
  const { fmtUptime, escHtml } = ctx.fmt;
  if (!rt || !rt.reachable) return `<div class="section-title">Router</div><p class="tab-placeholder">router metrics unavailable</p>`;
  const errPct = rt.total_requests > 0 ? ((rt.errors / rt.total_requests) * 100).toFixed(1) : "0.0";
  const reqps = rt.reqPerSec != null ? (rt.reqPerSec >= 10 ? rt.reqPerSec.toFixed(0) : rt.reqPerSec.toFixed(1)) : "-";
  const tokps = rt.tokPerSec != null ? rt.tokPerSec.toFixed(0) : "-";
  const avgLat = rt.avg_latency_ms != null ? rt.avg_latency_ms.toFixed(0) : "-";
  const totalTok = (rt.tokens_prompt || 0) + (rt.tokens_completion || 0);
  const topModels =
    (rt.top_models || [])
      .slice(0, 5)
      .map(([m, n]) => `${escHtml(m)} <span style="color:var(--text-dim)">${n}</span>`)
      .join(" · ") || '<span style="color:var(--text-dim)">no traffic since restart</span>';
  return `
      <div class="section-title">Router</div>
      <div class="nodes">
        <div class="node-card">
          <div class="node-name">llm-router-go <span class="badge badge-on">${escHtml(rt.version || "?")}</span></div>
          <div class="node-detail" style="margin-top:0.4rem">mode <strong>${escHtml(rt.mode || "?")}</strong> · up ${fmtUptime(rt.uptime_seconds)}</div>
          <div class="node-detail">${rt.models || 0} models active</div>
        </div>
        <div class="node-card">
          <div class="node-name">Requests</div>
          <div class="metric-row"><span class="rstat-label">total</span><span class="vram-text"><strong>${(rt.total_requests || 0).toLocaleString()}</strong></span></div>
          <div class="metric-row"><span class="rstat-label">errors</span><span class="vram-text">${rt.errors || 0} <span style="color:var(--text-dim)">(${errPct}%)</span></span></div>
          <div class="metric-row"><span class="rstat-label">rate</span><span class="vram-text"><strong>${reqps}</strong> req/s</span></div>
        </div>
        <div class="node-card">
          <div class="node-name">Throughput</div>
          <div class="metric-row"><span class="rstat-label">tokens</span><span class="vram-text"><strong>${totalTok.toLocaleString()}</strong></span></div>
          <div class="metric-row"><span class="rstat-label">rate</span><span class="vram-text"><strong>${tokps}</strong> tok/s</span></div>
          <div class="metric-row"><span class="rstat-label">avg latency</span><span class="vram-text">${avgLat} ms</span></div>
        </div>
        <div class="node-card" style="flex:2">
          <div class="node-name">Top models <span style="color:var(--text-dim);font-weight:400">by requests</span></div>
          <div class="node-detail" style="margin-top:0.4rem;line-height:1.7">${topModels}</div>
        </div>
      </div>`;
}

// Usage by user — who is spending what, from the durable request log.
// Hidden when the sink cannot answer (no reqlog) or the listener wanted an
// identity we did not present (the server answers 401/403; api.get throws).
function renderUsage(us) {
  const { escHtml, fmtAgoIso } = ctx.fmt;
  if (!us || !us.available || !(us.rows || []).length) return "";
  const rows = us.rows
    .map((r) => {
      const who = r.principal ? escHtml(r.principal) : '<span style="color:var(--text-dim)">(unattributed)</span>';
      const tok = (r.prompt_tokens || 0) + (r.completion_tokens || 0);
      const cost = r.cost_usd ? "$" + r.cost_usd.toFixed(r.cost_usd < 0.1 ? 4 : 2) : '<span style="color:var(--text-dim)">local</span>';
      const fail = r.failures ? `<span style="color:var(--red)">${r.failures}</span>` : "0";
      return `<tr><td>${who}</td><td class="num">${(r.requests || 0).toLocaleString()}</td><td class="num">${fail}</td><td class="num">${tok.toLocaleString()}</td><td class="num">${cost}</td><td>${fmtAgoIso(r.last_seen)}</td></tr>`;
    })
    .join("");
  return `
      <div class="section-title">Usage by user <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(last ${us.hours}h, from the request log)</span></div>
      <div class="node-card" style="padding:0.5rem 0.9rem;overflow-x:auto">
        <table class="usage-table">
          <thead><tr><th>principal</th><th class="num">requests</th><th class="num">failures</th><th class="num">tokens</th><th class="num">cost</th><th>last seen</th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
}

// Upstream failures — rendered only when something failed in the rolling
// 24h window, so the section doubles as the alarm. Grouped by endpoint:
// many models failing under one api_base = the endpoint is down; one model
// alone = that model's upstream is broken (the 2026-08-27 Zen triage).
function renderUpstream(list) {
  const { escHtml, fmtAgoS } = ctx.fmt;
  const upRows = (list || []).filter((u) => u.failed_24h > 0);
  if (!upRows.length) return "";
  const ago = (iso) => fmtAgoS(Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000));
  const groups = {};
  for (const u of upRows) (groups[u.api_base || "(local)"] ||= []).push(u);
  let cardsHtml = "";
  for (const [base, rows] of Object.entries(groups)) {
    const host = base.replace(/^https?:\/\//, "").replace(/\/+$/, "");
    const lines = rows
      .map((u) => {
        // Hard-down: every attempt in the last hour failed, on enough
        // attempts (>=3) that it isn't one flaky request.
        const hardDown = u.total_1h >= 3 && u.failed_1h === u.total_1h;
        const pct1 = u.total_1h ? Math.round((100 * u.failed_1h) / u.total_1h) : null;
        const pct24 = Math.round((100 * u.failed_24h) / u.total_24h);
        const badge = hardDown ? '<span class="badge" style="background:rgba(255,80,80,0.15);color:var(--red)">DOWN</span> ' : "";
        const since = u.failing_since ? ` · failing since ${ago(u.failing_since)}` : u.last_failure_at ? ` · last ${ago(u.last_failure_at)}` : "";
        const oneH =
          pct1 === null ? '<span style="color:var(--text-dim)">no traffic 1h</span>' : `1h <strong>${u.failed_1h}/${u.total_1h}</strong> (${pct1}%)`;
        return `<div class="metric-row"${hardDown ? ' style="color:var(--red)"' : ""}>
          <span class="rstat-label">${badge}${escHtml(u.model)}</span>
          <span class="vram-text">${oneH} · 24h ${u.failed_24h}/${u.total_24h} (${pct24}%)
            <span style="color:var(--text-dim)">${escHtml(u.last_error_class || "")}${since}</span></span>
        </div>`;
      })
      .join("");
    cardsHtml += `<div class="node-card" style="flex:1;min-width:20rem"><div class="node-name">${escHtml(host)}</div>${lines}</div>`;
  }
  return `
      <div class="section-title">Upstream failures <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(rolling 24h, since router restart — durable history: just reqlog-failures)</span></div>
      <div class="nodes">${cardsHtml}</div>`;
}

export default {
  id: "traffic",
  label: "Traffic",
  mount(r, c) {
    root = r;
    ctx = c;
    routerPrev = null;
    cards.router = cards.usage = cards.upstream = "";
    paint();
    ctx.poll(async () => {
      const rd = await ctx.api.get("/api/router-metrics");
      if (rd && rd.reachable) {
        const now = Date.now() / 1000;
        const tokTotal = (rd.tokens_prompt || 0) + (rd.tokens_completion || 0);
        if (routerPrev && now > routerPrev.ts) {
          const dt = now - routerPrev.ts;
          rd.reqPerSec = Math.max(0, (rd.total_requests - routerPrev.total) / dt);
          rd.tokPerSec = Math.max(0, (tokTotal - routerPrev.tok) / dt);
        }
        routerPrev = { total: rd.total_requests, tok: tokTotal, ts: now };
      }
      cards.router = renderRouter(rd);
      paint();
    }, 2000);
    ctx.poll(async () => {
      let us = null;
      try {
        us = await ctx.api.get("/api/usage?hours=24");
      } catch (_) {
        us = null; // 401/403: this listener wants the proxy's identity
      }
      cards.usage = renderUsage(us);
      paint();
    }, 30000);
    ctx.poll(async () => {
      const d = await ctx.api.get("/api/upstream");
      cards.upstream = renderUpstream(d.upstream || []);
      paint();
    }, 30000);
  },
  unmount() {
    root = null;
    routerPrev = null;
  },
};
