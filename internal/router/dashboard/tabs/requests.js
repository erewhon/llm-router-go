// Requests tab — every request one harness session sent, newest first, from
// the request log. The session is the caller's own id (Claude Code's
// X-Claude-Code-Session-Id, opencode's x-session-id); a prefix of four or
// more characters is enough. State lives in the hash (#requests?session=…),
// which is the link `pitf session <id>` prints.
let root = null;
let ctx = null;

function sessionFromHash() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  return (new URLSearchParams(qs || "").get("session") || "").trim();
}

function form(session) {
  const { escHtml } = ctx.fmt;
  return `
    <div class="section-title">Requests by session
      <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(from the request log; paste a session id or its first 4+ characters)</span>
    </div>
    <form id="reqForm" class="tok-form" style="margin:0 0 0.8rem">
      <input id="reqSession" type="text" value="${escHtml(session)}" placeholder="session id or prefix"
             autocomplete="off" spellcheck="false" style="min-width:22rem">
      <button type="submit">show</button>
    </form>
    <div id="reqBody"></div>`;
}

function tokens(n) {
  return n === null || n === undefined ? "–" : Number(n).toLocaleString();
}

function table(d) {
  const { escHtml, fmtAgoIso } = ctx.fmt;
  if (!d.available) {
    return `<div class="node-card"><p class="chat-placeholder">${escHtml(d.reason || "the request log is not queryable on this router")}</p></div>`;
  }
  if (!d.rows.length) {
    return `<div class="node-card"><p class="chat-placeholder">no requests carry a session id starting with <span class="api-base">${escHtml(d.session)}</span></p></div>`;
  }
  const sessions = new Set(d.rows.map((r) => r.session_id));
  let h = `<div style="margin:0 0 0.5rem;color:var(--text-dim);font-size:0.85rem">${d.rows.length}${d.rows.length === d.limit ? "+" : ""} requests`;
  if (sessions.size > 1) h += ` across ${sessions.size} sessions (the prefix is ambiguous)`;
  h += `</div><div class="node-card" style="padding:0.5rem 0.9rem;overflow-x:auto"><table class="usage-table"><thead><tr>
    <th>when</th><th>model</th><th>served by</th><th class="num">status</th><th class="num">latency</th>
    <th class="num">in</th><th class="num">out</th><th class="num">cache read</th><th>provider</th><th>principal</th>${sessions.size > 1 ? "<th>session</th>" : ""}
  </tr></thead><tbody>`;
  for (const r of d.rows) {
    const bad = r.status >= 400 || r.error_class;
    h += `<tr title="${escHtml(r.request_id || "")}">
      <td>${fmtAgoIso(r.ts)}</td>
      <td>${escHtml(r.model)}</td>
      <td>${escHtml(r.resolved_via || "")}</td>
      <td class="num"${bad ? ' style="color:var(--red)"' : ""}>${r.status}${r.error_class ? ` ${escHtml(r.error_class)}` : ""}</td>
      <td class="num">${r.latency_ms} ms</td>
      <td class="num">${tokens(r.prompt_tokens)}</td>
      <td class="num">${tokens(r.completion_tokens)}</td>
      <td class="num">${tokens(r.cache_read_tokens)}</td>
      <td>${escHtml(r.upstream_provider || "")}</td>
      <td>${escHtml(r.principal || "")}</td>
      ${sessions.size > 1 ? `<td><span class="api-base">${escHtml(r.session_id)}</span></td>` : ""}
    </tr>`;
  }
  return h + "</tbody></table></div>";
}

async function load(session) {
  const body = root && root.querySelector("#reqBody");
  if (!body) return;
  if (session.length < 4) {
    body.innerHTML = session
      ? `<p class="tab-placeholder">type at least 4 characters</p>`
      : `<p class="tab-placeholder">no session selected</p>`;
    return;
  }
  try {
    const d = await ctx.api.get(`/api/requests?session=${encodeURIComponent(session)}`);
    if (root) body.innerHTML = table(d);
  } catch (e) {
    if (root) body.innerHTML = `<p class="error">${ctx.fmt.escHtml(String(e.message || e))}</p>`;
  }
}

function onSubmit(ev) {
  ev.preventDefault();
  const session = root.querySelector("#reqSession").value.trim();
  // replaceState: the tab owns its hash state and a submit is not a new view.
  history.replaceState(null, "", `#requests?session=${encodeURIComponent(session)}`);
  load(session);
}

let stopPoll = null;

export default {
  id: "requests",
  label: "Requests",
  mount(r, c) {
    root = r;
    ctx = c;
    const session = sessionFromHash();
    root.innerHTML = form(session);
    root.querySelector("#reqForm").addEventListener("submit", onSubmit);
    // A live session keeps adding rows; refresh while the tab is open.
    stopPoll = ctx.poll(() => root && load(root.querySelector("#reqSession").value.trim()), 15000);
  },
  unmount() {
    if (stopPoll) stopPoll();
    stopPoll = null;
    root = null;
  },
};
