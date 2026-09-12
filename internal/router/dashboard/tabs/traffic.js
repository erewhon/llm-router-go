// Traffic tab — "what happened?" The Router counters card, Usage by user
// (from the request log) and Upstream failures (rolling 24 h ring since
// restart). Ported behaviour-for-behaviour from the legacy render() on
// 2026-09-12. Each card polls and renders on its own, so one slow endpoint
// never blanks the others.
//
// Since 2026-09-12 the durable half (requests/tokens/cost over 1h/24h/7d, and
// the failures list) comes from /api/traffic, i.e. the request log; the
// Router counters card stays as the "is it alive" view since restart.

let root = null;
let ctx = null;
let routerPrev = null; // previous /api/router-metrics sample, for rates
const cards = { router: "", series: "", usage: "" };
const view = { window: "24h", by: "model" };

function readHash() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  const q = new URLSearchParams(qs || "");
  if (["1h", "24h", "7d"].includes(q.get("window"))) view.window = q.get("window");
  if (["model", "principal"].includes(q.get("by"))) view.by = q.get("by");
}
function writeHash() {
  history.replaceState(null, "", `#traffic?window=${view.window}&by=${view.by}`);
}

function paint() {
  if (!root) return;
  root.innerHTML =
    (cards.router || `<div class="section-title">Router</div><p class="tab-placeholder">waiting for /api/router-metrics…</p>`) +
    (cards.series || `<div class="section-title">Requests over time</div><p class="tab-placeholder">waiting for /api/traffic…</p>`) +
    cards.usage;
}

// renderSeries draws the three charts and the failures table from one
// /api/traffic payload.
function renderSeries(d) {
  const { escHtml, barChartSvg, SERIES_COLORS, fmtAgoIso } = ctx.fmt;
  const sel = (name, options) =>
    `<span class="filter-bar" style="display:inline-flex;margin:0 0 0 0.5rem">${options
      .map(([v, label]) => `<span class="filter-chip${view[name] === v ? " active" : ""}" data-set="${name}" data-value="${v}">${label}</span>`)
      .join("")}</span>`;
  const head = `<div class="section-title">Requests over time <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(from the request log)</span>
      ${sel("window", [
        ["1h", "1 h"],
        ["24h", "24 h"],
        ["7d", "7 d"],
      ])}${sel("by", [
        ["model", "by model"],
        ["principal", "by user"],
      ])}</div>`;
  if (!d || !d.available) {
    return (
      head +
      `<div class="node-card"><p class="chat-placeholder">${escHtml((d && d.reason) || "the request log is not queryable on this router")}</p></div>`
    );
  }
  const rows = d.rows || [];
  const labels = rows.length
    ? rows[0].buckets.map((b) => {
        const t = new Date(b.start);
        return d.window === "7d"
          ? t.toLocaleDateString([], { month: "short", day: "numeric" })
          : t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
      })
    : [];
  const series = (pick) =>
    rows.map((r, i) => ({
      key: r.key || "(unattributed)",
      color: r.key === "other" ? "#8b8fa3" : SERIES_COLORS[i % SERIES_COLORS.length],
      values: r.buckets.map(pick),
    }));
  const fmtN = (v) => (v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e3 ? `${(v / 1e3).toFixed(1)}k` : String(Math.round(v)));
  const legend = rows
    .map(
      (r, i) =>
        `<span class="badge" style="background:${r.key === "other" ? "#8b8fa3" : SERIES_COLORS[i % SERIES_COLORS.length]}22;color:${r.key === "other" ? "#8b8fa3" : SERIES_COLORS[i % SERIES_COLORS.length]}">${escHtml(r.key || "(unattributed)")} ${r.requests}</span>`,
    )
    .join(" ");
  const chart = (title, s, fmt) =>
    `<div class="node-card" style="flex:1;min-width:24rem"><div class="node-name">${title}</div><div style="margin-top:0.4rem">${barChartSvg(s, { labels, valueFmt: fmt })}</div></div>`;
  let h =
    head +
    `<div style="margin:0 0 0.6rem;line-height:1.9">${legend || '<span class="chat-placeholder">no requests in this window</span>'}</div><div class="nodes">`;
  h += chart(
    "requests",
    series((b) => b.requests),
    fmtN,
  );
  h += chart(
    "errors",
    series((b) => b.errors),
    fmtN,
  );
  h += `</div><div class="nodes">`;
  h += chart(
    "tokens (prompt + completion)",
    series((b) => b.prompt_tokens + b.completion_tokens),
    fmtN,
  );
  h += chart(
    "cost, provider-billed (USD)",
    series((b) => b.cost_usd),
    (v) => `$${v >= 1 ? v.toFixed(2) : v.toFixed(3)}`,
  );
  h += `</div>`;
  const fails = d.failures || [];
  if (fails.length) {
    h += `<div class="section-title">Upstream failures <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(last ${d.window}, from the request log)</span></div>
      <div class="node-card" style="padding:0.5rem 0.9rem;overflow-x:auto"><table class="usage-table"><thead><tr><th>model</th><th>backend</th><th>error class</th><th class="num">count</th><th>last</th></tr></thead><tbody>`;
    for (const f of fails) {
      h += `<tr><td>${escHtml(f.model || "(unresolved)")}</td><td><span class="api-base">${escHtml(f.backend_url || "")}</span></td><td>${escHtml(f.error_class || "5xx")}</td><td class="num">${f.count}</td><td>${fmtAgoIso(f.last)}</td></tr>`;
    }
    h += `</tbody></table></div>`;
  }
  return h;
}

async function loadSeries() {
  let d = null;
  try {
    d = await ctx.api.get(`/api/traffic?window=${view.window}&by=${view.by}`);
  } catch (e) {
    d = { available: false, reason: String(e) };
  }
  cards.series = renderSeries(d);
  paint();
}

function onClick(ev) {
  const chip = ev.target.closest(".filter-chip[data-set]");
  if (!chip) return;
  view[chip.dataset.set] = chip.dataset.value;
  writeHash();
  loadSeries();
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

export default {
  id: "traffic",
  label: "Traffic",
  mount(r, c) {
    root = r;
    ctx = c;
    routerPrev = null;
    cards.router = cards.usage = cards.series = "";
    readHash();
    root.addEventListener("click", onClick);
    paint();
    ctx.poll(loadSeries, 60000);
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
  },
  unmount() {
    if (root) root.removeEventListener("click", onClick);
    root = null;
    routerPrev = null;
  },
};
