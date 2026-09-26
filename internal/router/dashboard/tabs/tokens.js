// Tokens tab — tokenator (the token profiler) drawn natively by the shell
// over its JSON API through the router's same-origin /tokens/ proxy
// (ctx.config.tokensUrl). Phase 2 of the unified web UI: no iframe, one
// theme, one hash grammar.
//
//   #tokens[?q=…&project=…&since=…]           session list / content search
//   #tokens?session=<id>                      profile (usage, cache, attribution)
//   #tokens?session=<id>&view=transcript[&q=…][&e=<idx>]   transcript, highlighted, scrolled to entry
//   #tokens?model=<alias>                     sessions that used one model
//
// Endpoints: /api/sessions, /api/session/{key}, /api/session/{key}/transcript
// (paged), /api/model/{name} — see tokenator docs/deploy-serve.md. The tab
// keeps its state in the hash (history.replaceState on its own changes, like
// Requests and Catalog) so a link is a view and a reload keeps it.
let root = null;
let ctx = null;
let base = "";
let transcriptState = null; // {key, q, entries, total, offset, loading}

// tokenator's fixed palette per entry kind (report.kindSlot), mapped onto
// the dashboard's colours so a transcript reads the same in both places.
const KIND_COLOR = {
  tool_result: "var(--accent)",
  assistant_text: "var(--green)",
  thinking: "#d55181",
  user_text: "var(--yellow)",
  meta_text: "#199e70",
  tool_use: "var(--orange)",
  image: "#9085e9",
  compaction: "var(--text-dim)",
};
const KIND_ORDER = ["tool_result", "assistant_text", "thinking", "user_text", "meta_text", "tool_use", "image"];
const COLLAPSE_OVER = 1200; // chars; longer bodies start folded (as tokenator's page does)

function query() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  return new URLSearchParams(qs || "");
}

export function hashFor(params) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== "") p.set(k, v);
  const s = p.toString();
  return `#tokens${s ? "?" + s : ""}`;
}

function setHash(params) {
  history.replaceState(null, "", hashFor(params));
}

function n(v) {
  return v === null || v === undefined ? "–" : Number(v).toLocaleString();
}
// abbrev renders token counts compactly (12.3M, 456k, 789), as tokenator does.
export function abbrev(v) {
  v = Number(v) || 0;
  if (v >= 10_000_000) return `${Math.round(v / 1e6)}M`;
  if (v >= 1_000_000) return `${(v / 1e6).toFixed(1)}M`;
  if (v >= 10_000) return `${Math.round(v / 1e3)}k`;
  if (v >= 1_000) return `${(v / 1e3).toFixed(1)}k`;
  return String(v);
}
function day(ts) {
  return ts && ts.length >= 10 ? ts.slice(0, 10) : ts || "";
}
function clock(ts) {
  return ts && ts.length >= 19 ? ts.slice(11, 19) : ts || "";
}
function chip(kind) {
  const { escHtml } = ctx.fmt;
  return `<span class="tk-chip" style="background:${KIND_COLOR[kind] || "var(--text-dim)"}">${escHtml(kind)}</span>`;
}
// highlight escapes text and wraps case-insensitive matches of q in <mark>.
export function highlight(text, q, escHtml) {
  if (!q) return escHtml(text);
  const lt = text.toLowerCase(),
    lq = q.toLowerCase();
  let out = "",
    pos = 0;
  for (;;) {
    const i = lt.indexOf(lq, pos);
    if (i < 0) break;
    out += escHtml(text.slice(pos, i)) + "<mark>" + escHtml(text.slice(i, i + q.length)) + "</mark>";
    pos = i + q.length;
  }
  return out + escHtml(text.slice(pos));
}

function crumbs(parts) {
  const { escHtml } = ctx.fmt;
  return `<div class="tk-crumbs"><a href="#tokens">sessions</a>${parts
    .map((p) => (p.href ? ` › <a href="${escHtml(p.href)}">${escHtml(p.label)}</a>` : ` › <span>${escHtml(p.label)}</span>`))
    .join("")}</div>`;
}

function errorCard(e) {
  const { escHtml } = ctx.fmt;
  return `<div class="node-card"><p class="error">tokenator via <span class="api-base">${escHtml(base)}/</span>: ${escHtml(String(e.message || e))}</p>
    <p class="chat-placeholder">start it with <span class="api-base">pitf tokens serve</span>, or check --dashboard-tokens-url</p></div>`;
}

// ---------- list / search ----------

async function renderList(q) {
  const { escHtml } = ctx.fmt;
  const text = q.get("q") || "",
    project = q.get("project") || "",
    since = q.get("since") || "";
  root.innerHTML = `
    <div class="section-title">Tokens
      <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(tokenator, via <span class="api-base">${escHtml(base)}/</span> · <a href="${escHtml(base)}/" target="_blank" rel="noopener" style="color:var(--accent)">open standalone ↗</a>)</span>
    </div>
    <form id="tkForm" class="tok-form">
      <input id="tkQ" type="text" value="${escHtml(text)}" placeholder="search session content…" autocomplete="off" spellcheck="false">
      <select id="tkProject"><option value="">all projects</option></select>
      <select id="tkSince">${["", "24h", "7d", "30d"].map((s) => `<option value="${s}"${s === since ? " selected" : ""}>${s ? "last " + s : "all time"}</option>`).join("")}</select>
      <button type="submit">${text ? "search" : "filter"}</button>
    </form>
    <div id="tkBody"><p class="tab-placeholder">loading…</p></div>`;
  root.querySelector("#tkForm").addEventListener("submit", (ev) => {
    ev.preventDefault();
    const params = {
      q: root.querySelector("#tkQ").value.trim(),
      project: root.querySelector("#tkProject").value,
      since: root.querySelector("#tkSince").value,
    };
    setHash(params);
    renderList(new URLSearchParams(params));
  });
  const body = root.querySelector("#tkBody");
  try {
    const p = new URLSearchParams({ q: text, project, since });
    const d = await ctx.api.get(`${base}/api/sessions?${p}`);
    const sel = root.querySelector("#tkProject");
    if (sel) {
      sel.innerHTML = `<option value="">all projects</option>` + (d.projects || []).map((pr) => `<option${pr === project ? " selected" : ""}>${escHtml(pr)}</option>`).join("");
    }
    body.innerHTML = d.searched ? searchResults(d) : listTable(d);
  } catch (e) {
    if (body) body.innerHTML = errorCard(e);
  }
}

function sessionLink(row, view) {
  return hashFor({ session: row.key, view });
}

function listTable(d) {
  const { escHtml } = ctx.fmt;
  if (!d.rows.length) return `<div class="node-card"><p class="chat-placeholder">no sessions — run <span class="api-base">pitf tokens ingest</span> first</p></div>`;
  let h = `<div class="node-card" style="padding:0.5rem 0.9rem;overflow-x:auto"><table class="usage-table"><thead><tr>
    <th>when</th><th>project</th><th>session</th><th class="num">req</th><th class="num">tokens</th><th class="num">out</th><th></th></tr></thead><tbody>`;
  for (const r of d.rows) {
    const total = (r.input || 0) + (r.cache_read || 0) + (r.cache_write || 0) + (r.output || 0);
    h += `<tr>
      <td style="white-space:nowrap">${escHtml(day(r.ended_at || r.started_at))}</td>
      <td>${escHtml(r.project)}</td>
      <td><a href="${sessionLink(r, "transcript")}" style="color:var(--text);font-weight:550">${escHtml(r.title || r.key)}</a>${r.agent ? ` <span style="color:var(--text-dim);font-size:0.75rem">· ${escHtml(r.agent)}</span>` : ""}</td>
      <td class="num">${n(r.requests)}</td><td class="num">${abbrev(total)}</td><td class="num">${abbrev(r.output)}</td>
      <td style="white-space:nowrap"><a href="${sessionLink(r)}" style="color:var(--accent)">profile</a> · <a href="#requests?session=${encodeURIComponent(r.key)}" style="color:var(--accent)">requests</a></td>
    </tr>`;
  }
  return h + `</tbody></table></div>`;
}

function searchResults(d) {
  const { escHtml } = ctx.fmt;
  let h = `<p style="margin:0 0 0.5rem;color:var(--text-dim);font-size:0.85rem">${d.rows.length} session${d.rows.length === 1 ? "" : "s"} matched · scanned ${d.scanned} newest candidate sessions in ${d.scan_ms} ms${
    d.truncated ? ` · hit the ${d.limit}-session scan cap — narrow with project/since to reach older sessions` : ""
  }</p>`;
  if (!d.rows.length) return h + `<div class="node-card"><p class="chat-placeholder">no matches</p></div>`;
  for (const r of d.rows) {
    const total = (r.input || 0) + (r.cache_read || 0) + (r.cache_write || 0) + (r.output || 0);
    h += `<div class="node-card" style="cursor:default;margin-bottom:0.6rem">
      <div><a href="${hashFor({ session: r.key, view: "transcript", q: d.query })}" style="color:var(--text);font-weight:550">${escHtml(r.title || r.key)}</a>${r.agent ? ` <span style="color:var(--text-dim);font-size:0.75rem">agent: ${escHtml(r.agent)}</span>` : ""}</div>
      <div class="node-detail" style="margin-top:0.2rem">${escHtml(day(r.ended_at || r.started_at))} · ${escHtml(r.project)} · ${n(r.requests)} req · ${abbrev(total)} toks · ${
        r.matches ? `${r.matches} match${r.matches === 1 ? "" : "es"}` : "title match"
      } · <a href="${sessionLink(r)}" style="color:var(--accent)">profile</a>${r.scan_err ? ` · <span style="color:var(--text-dim)">transcript unavailable: ${escHtml(r.scan_err)}</span>` : ""}</div>`;
    if (r.hits && r.hits.length) {
      h += `<ul class="tk-snips">`;
      for (const hit of r.hits) {
        h += `<li><span class="tk-t">${escHtml(hit.ts)}</span>${chip(hit.kind)}${hit.tool ? `<span class="tk-t">${escHtml(hit.tool)}</span>` : ""}
          <a href="${hashFor({ session: r.key, view: "transcript", q: d.query, e: hit.anchor })}" title="open at this entry">¶</a> ${highlight(hit.snippet || "", d.query, escHtml)}</li>`;
      }
      h += `</ul>`;
    }
    h += `</div>`;
  }
  return h;
}

// ---------- profile ----------

function statTile(label, value, sub) {
  const { escHtml } = ctx.fmt;
  return `<div class="stat"><div class="stat-value" style="font-size:1.1rem">${value}</div><div class="stat-label">${escHtml(label)}${sub ? ` <span style="color:var(--text-dim)">${escHtml(sub)}</span>` : ""}</div></div>`;
}

function attrTable(title, rows, groupLabel) {
  const { escHtml } = ctx.fmt;
  if (!rows || !rows.length) return "";
  let h = `<div class="node-card" style="cursor:default;flex:1 1 20rem"><div class="node-name">${escHtml(title)}</div>
    <table class="usage-table" style="margin-top:0.4rem"><thead><tr><th>${escHtml(groupLabel)}</th><th class="num">blocks</th><th class="num">est tokens</th><th class="num">errors</th></tr></thead><tbody>`;
  for (const a of rows.slice(0, 12)) {
    h += `<tr><td style="max-width:24rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${escHtml(a.group)}">${escHtml(a.group)}</td>
      <td class="num">${n(a.blocks)}</td><td class="num">${abbrev(a.est_tokens)}</td><td class="num">${a.errors ? `<span style="color:var(--red)">${a.errors}</span>` : "–"}</td></tr>`;
  }
  return h + `</tbody></table></div>`;
}

async function renderProfile(key) {
  const { escHtml, barChartSvg } = ctx.fmt;
  root.innerHTML = `<p class="tab-placeholder">loading session ${escHtml(key)}…</p>`;
  let d;
  try {
    d = await ctx.api.get(`${base}/api/session/${encodeURIComponent(key)}`);
  } catch (e) {
    root.innerHTML = crumbs([{ label: key }]) + errorCard(e);
    return;
  }
  const m = d.meta;
  const t = d.totals || {};
  const total = (t.input || 0) + (t.cache_read || 0) + (t.cache_write || 0) + (t.output || 0);
  const reuse = d.reuse >= 0 ? `${Math.round(d.reuse * 100)}%` : "–";
  const pts = d.points || [];
  const usage = barChartSvg(
    [
      { key: "cache read", values: pts.map((p) => p.cache_read || 0), color: "var(--accent)" },
      { key: "cache write", values: pts.map((p) => p.cache_write || 0), color: "#9085e9" },
      { key: "input", values: pts.map((p) => p.input || 0), color: "var(--yellow)" },
      { key: "output", values: pts.map((p) => p.output || 0), color: "var(--green)" },
    ],
    { height: 150, valueFmt: abbrev, labels: pts.map((p) => clock(p.ts)) },
  );
  const kinds = KIND_ORDER.filter((k) => (d.buckets || []).some((b) => (b.by_kind || {})[k]));
  const buckets = barChartSvg(
    kinds.map((k) => ({ key: k, values: (d.buckets || []).map((b) => (b.by_kind || {})[k] || 0), color: KIND_COLOR[k] })),
    { height: 120, valueFmt: abbrev, labels: (d.buckets || []).map((b) => `${b.start_idx + 1}–${b.end_idx + 1}`) },
  );
  const legend = (items) => `<div class="tk-legend">${items.map(([k, c]) => `<span><i style="background:${c}"></i>${escHtml(k)}</span>`).join("")}</div>`;
  let events = "";
  if (d.events && d.events.length) {
    events = `<div class="node-card" style="cursor:default"><div class="node-name">Cache invalidations <span style="color:var(--text-dim);font-weight:400">(${d.events.length})</span></div>
      <table class="usage-table" style="margin-top:0.4rem"><thead><tr><th>when</th><th>model</th><th>cause</th><th class="num">expected</th><th class="num">read</th><th class="num">shortfall</th></tr></thead><tbody>${d.events
        .slice(0, 20)
        .map(
          (e) => `<tr><td>${escHtml(clock(e.ts))}</td><td>${escHtml(e.model)}</td><td>${escHtml(e.cause)}</td><td class="num">${abbrev(e.expected)}</td><td class="num">${abbrev(e.cache_read)}</td><td class="num" style="color:var(--red)">${abbrev(e.shortfall)}</td></tr>`,
        )
        .join("")}</tbody></table></div>`;
  }
  root.innerHTML = `
    ${crumbs([{ label: m.title || m.key }])}
    <div class="section-title">${escHtml(m.project)}${m.title ? ` — ${escHtml(m.title)}` : ""}
      <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">session <span class="api-base">${escHtml(m.key)}</span>${m.agent ? ` · agent ${escHtml(m.agent)}` : ""} · ${escHtml(d.source_kind || "")}</span>
    </div>
    <div class="tk-links">
      <a href="${hashFor({ session: m.key, view: "transcript" })}">transcript</a>
      <a href="#requests?session=${encodeURIComponent(m.key)}">router requests</a>
      ${ctx.config.monitorUrl ? `<a href="#agents">agents</a>` : ""}
      <a href="${escHtml(base)}/session/${encodeURIComponent(m.key)}" target="_blank" rel="noopener">tokenator page ↗</a>
    </div>
    <div class="stats" style="margin-bottom:1rem">
      ${statTile("requests", n(pts.length))}
      ${statTile("tokens", abbrev(total), "in+cache+out")}
      ${statTile("input", abbrev(t.input))}
      ${statTile("cache read", abbrev(t.cache_read))}
      ${statTile("cache write", abbrev(t.cache_write))}
      ${statTile("output", abbrev(t.output))}
      ${statTile("cache reuse", reuse, d.shortfall ? `shortfall ${abbrev(d.shortfall)}` : "")}
      ${statTile("new content", abbrev(d.new_est), "est tokens")}
    </div>
    <div class="node-card" style="cursor:default;margin-bottom:1rem"><div class="node-name">Context per request <span style="color:var(--text-dim);font-weight:400">(${(d.models || []).map(escHtml).join(", ")} · ${escHtml(day(d.start))} ${escHtml(clock(d.start))} → ${escHtml(clock(d.end))})</span></div>
      ${usage}${legend([["cache read", "var(--accent)"], ["cache write", "#9085e9"], ["input", "var(--yellow)"], ["output", "var(--green)"]])}</div>
    ${
      kinds.length
        ? `<div class="node-card" style="cursor:default;margin-bottom:1rem"><div class="node-name">New content by kind <span style="color:var(--text-dim);font-weight:400">(est tokens per request span)</span></div>${buckets}${legend(kinds.map((k) => [k, KIND_COLOR[k]]))}</div>`
        : ""
    }
    ${events}
    <div class="nodes" style="margin-top:1rem">${attrTable("Top tools", d.top_tools, "tool")}${attrTable("Top files", d.top_files, "file")}${attrTable("Composition", d.kinds, "kind")}</div>`;
}

// ---------- transcript ----------

async function renderTranscript(key, q, anchor) {
  const { escHtml } = ctx.fmt;
  transcriptState = { key, q, entries: [], total: 0, offset: 0, loading: false, meta: null, err: "" };
  root.innerHTML = `<p class="tab-placeholder">loading transcript ${escHtml(key)}…</p>`;
  try {
    await loadPage(anchor);
  } catch (e) {
    root.innerHTML = crumbs([{ label: key }]) + errorCard(e);
    return;
  }
  drawTranscript();
  if (anchor !== null && anchor !== undefined) {
    const el = root.querySelector(`[data-idx="${Number(anchor)}"]`);
    if (el) {
      el.scrollIntoView({ block: "center" });
      el.classList.add("tk-target");
    }
  }
}

// loadPage fetches the next page; with an anchor past the first page it
// keeps fetching until the anchor is loaded.
async function loadPage(anchor) {
  const st = transcriptState;
  const limit = 500;
  for (;;) {
    const p = new URLSearchParams({ q: st.q || "", offset: String(st.offset), limit: String(limit) });
    const d = await ctx.api.get(`${base}/api/session/${encodeURIComponent(st.key)}/transcript?${p}`);
    st.meta = d.meta;
    st.total = d.total;
    st.err = d.err || "";
    st.entries.push(...(d.entries || []));
    st.offset = st.entries.length;
    if (anchor === null || anchor === undefined || Number(anchor) < st.entries.length || st.entries.length >= st.total || !(d.entries || []).length) return;
  }
}

function drawTranscript() {
  const { escHtml } = ctx.fmt;
  const st = transcriptState;
  const m = st.meta || { key: st.key };
  let body = "";
  let prevDay = "";
  for (const e of st.entries) {
    const d = day(e.ts);
    const ts = d && d !== prevDay ? `${d} ${clock(e.ts)}` : clock(e.ts);
    prevDay = d || prevDay;
    const long = e.text.length > COLLAPSE_OVER || ((e.kind === "tool_result" || e.kind === "meta_text") && e.text.length > 200);
    const text = highlight(e.text, st.q, escHtml);
    body += `<div class="tk-entry${e.is_error ? " err" : ""}" data-idx="${e.idx}" id="tke${e.idx}">
      <div class="tk-ehead"><a href="${hashFor({ session: st.key, view: "transcript", q: st.q, e: e.idx })}" class="tk-t">¶${e.idx}</a>${chip(e.kind)}${e.tool ? `<span class="tk-tool">${escHtml(e.tool)}</span>` : ""}${e.is_error ? `<span class="tk-chip" style="background:var(--red)">error</span>` : ""}<span class="tk-t">${escHtml(ts)}</span></div>
      ${long ? `<details${e.matched ? " open" : ""}><summary>${e.text.length.toLocaleString()} chars</summary><pre>${text}</pre></details>` : `<pre>${text}</pre>`}
    </div>`;
  }
  const more = st.entries.length < st.total ? `<button class="copy-btn" id="tkMore" style="margin-top:0.6rem">load ${Math.min(500, st.total - st.entries.length)} more (${st.entries.length} of ${st.total})</button>` : "";
  root.innerHTML = `
    ${crumbs([{ label: m.title || m.key, href: hashFor({ session: st.key }) }, { label: "transcript" }])}
    <div class="section-title">${escHtml(m.project || "")}${m.title ? ` — ${escHtml(m.title)}` : ""}
      <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">session <span class="api-base">${escHtml(m.key || st.key)}</span>${m.agent ? ` · agent ${escHtml(m.agent)}` : ""} · ${st.total} entries</span>
    </div>
    <div class="tk-links">
      <a href="${hashFor({ session: st.key })}">profile</a>
      <a href="#requests?session=${encodeURIComponent(st.key)}">router requests</a>
      <a href="${escHtml(base)}/session/${encodeURIComponent(st.key)}/transcript${st.q ? "?q=" + encodeURIComponent(st.q) : ""}" target="_blank" rel="noopener">tokenator page ↗</a>
    </div>
    <form id="tkHl" class="tok-form" style="grid-template-columns:minmax(12rem,1fr) auto">
      <input id="tkHlQ" type="text" value="${escHtml(st.q || "")}" placeholder="highlight in this session…" autocomplete="off" spellcheck="false">
      <button type="submit">highlight</button>
    </form>
    ${st.err ? `<div class="node-card" style="cursor:default"><p class="chat-placeholder">${escHtml(st.err)}</p></div>` : ""}
    <div class="node-card" style="cursor:default">${body || `<p class="chat-placeholder">no entries</p>`}${more}</div>`;
  root.querySelector("#tkHl").addEventListener("submit", (ev) => {
    ev.preventDefault();
    const q = root.querySelector("#tkHlQ").value.trim();
    setHash({ session: st.key, view: "transcript", q });
    renderTranscript(st.key, q, null);
  });
  const btn = root.querySelector("#tkMore");
  if (btn) {
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      btn.textContent = "loading…";
      try {
        await loadPage(null);
        const y = window.scrollY;
        drawTranscript();
        window.scrollTo(0, y);
      } catch (e) {
        btn.textContent = String(e.message || e);
      }
    });
  }
}

// ---------- model ----------

async function renderModel(name) {
  const { escHtml } = ctx.fmt;
  root.innerHTML = `<p class="tab-placeholder">loading model ${escHtml(name)}…</p>`;
  let d;
  try {
    d = await ctx.api.get(`${base}/api/model/${encodeURIComponent(name)}`);
  } catch (e) {
    root.innerHTML = crumbs([{ label: name }]) + errorCard(e);
    return;
  }
  const t = d.totals || {};
  let rows = "";
  for (const r of d.rows || []) {
    rows += `<tr><td style="white-space:nowrap">${escHtml(day(r.last_ts))}</td><td>${escHtml(r.project)}</td>
      <td><a href="${hashFor({ session: r.key, view: "transcript" })}" style="color:var(--text);font-weight:550">${escHtml(r.title || r.key)}</a>${r.agent ? ` <span style="color:var(--text-dim);font-size:0.75rem">· ${escHtml(r.agent)}</span>` : ""}</td>
      <td class="num">${n(r.requests)}</td><td class="num">${abbrev((r.input || 0) + (r.output || 0))}</td><td class="num">${abbrev(r.output)}</td>
      <td><span class="badge ${r.via === "router" ? "badge-router" : "badge-alias"}">${escHtml(r.via)}</span></td>
      <td><a href="${hashFor({ session: r.key })}" style="color:var(--accent)">profile</a></td></tr>`;
  }
  root.innerHTML = `
    ${crumbs([{ label: `model ${name}` }])}
    <div class="section-title">model ${escHtml(name)}
      <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">${(d.rows || []).length} session${(d.rows || []).length === 1 ? "" : "s"}${d.trimmed ? ` (newest ${d.limit})` : ""} · router: ${n(t.requests)} req · ${abbrev((t.input || 0) + (t.output || 0))} toks · ${abbrev(t.output)} out${
        t.unpaired ? ` · ${n(t.unpaired)} not attributed to a session` : ""
      }</span>
    </div>
    <div class="tk-links"><a href="#catalog?model=${encodeURIComponent(name)}">catalog</a><a href="${escHtml(base)}/model/${encodeURIComponent(name)}" target="_blank" rel="noopener">tokenator page ↗</a></div>
    <div class="node-card" style="cursor:default;padding:0.5rem 0.9rem;overflow-x:auto">${
      rows
        ? `<table class="usage-table"><thead><tr><th>last</th><th>project</th><th>session</th><th class="num">req</th><th class="num">tokens</th><th class="num">out</th><th>from</th><th></th></tr></thead><tbody>${rows}</tbody></table>`
        : `<p class="chat-placeholder">no session used ${escHtml(name)} — neither a transcript nor the router log names it</p>`
    }</div>`;
}

// ---------- tab ----------

function route() {
  const q = query();
  const session = q.get("session"),
    model = q.get("model");
  if (session) {
    if (q.get("view") === "transcript") return renderTranscript(session, q.get("q") || "", q.get("e"));
    return renderProfile(session);
  }
  if (model) return renderModel(model);
  return renderList(q);
}

export default {
  id: "tokens",
  label: "Tokens",
  mount(r, c) {
    root = r;
    ctx = c;
    base = ctx.config.tokensUrl;
    route();
  },
  unmount() {
    transcriptState = null;
    root = null;
  },
};
