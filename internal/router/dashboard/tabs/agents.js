// Agents tab — the coding agents agent-monitor is watching, live, with a
// jump from each agent's harness session to the Requests tab. Shown only
// when the router has a --dashboard-monitor-url (a router on the same
// machine as its agents, e.g. a work laptop): the browser reads the monitor
// directly, which answers GETs with CORS *. Read-only; the task board stays
// in agent-monitor's own page, linked from here.
let root = null;
let ctx = null;
let stopPoll = null;

// Attention first: an agent waiting on the operator outranks one working.
const ORDER = { waiting: 0, error: 1, running: 2, planning: 3, idle: 4, unknown: 5 };
const COLOR = {
  waiting: "var(--yellow)",
  error: "var(--red)",
  running: "var(--green)",
  planning: "var(--accent)",
  idle: "var(--text-dim)",
  unknown: "var(--text-dim)",
};

function statusCell(a) {
  const { escHtml } = ctx.fmt;
  const color = COLOR[a.status] || "var(--text-dim)";
  const reason = a.status === "waiting" && a.wait_reason && a.wait_reason !== "none" ? ` · ${escHtml(a.wait_reason)}` : "";
  return `<span style="color:${color}">&#9679; ${escHtml(a.status)}${reason}</span>`;
}

function sessionCell(a) {
  const { escHtml } = ctx.fmt;
  if (!a.session_id) {
    return `<span style="color:var(--text-dim)" title="no hook has reported a session id; see agent-monitor hooks install">–</span>`;
  }
  const short = a.session_id.slice(0, 8);
  return `<a href="#requests?session=${encodeURIComponent(a.session_id)}" style="color:var(--accent)" title="router requests for ${escHtml(a.session_id)}">requests</a>
    <span class="api-base" title="${escHtml(a.session_id)}">${escHtml(short)}</span>`;
}

// agent-monitor sends Go's zero time for an agent it has not seen update.
function updated(iso) {
  const t = iso ? Date.parse(iso) : NaN;
  return Number.isNaN(t) || t < Date.UTC(2000, 0, 1) ? "–" : ctx.fmt.fmtAgoIso(iso);
}

function table(agents) {
  const { escHtml } = ctx.fmt;
  if (!agents.length) {
    return `<div class="node-card"><p class="chat-placeholder">agent-monitor sees no agents</p></div>`;
  }
  agents.sort((x, y) => (ORDER[x.status] ?? 9) - (ORDER[y.status] ?? 9) || x.name.localeCompare(y.name));
  let h = `<div class="node-card" style="padding:0.5rem 0.9rem;overflow-x:auto"><table class="usage-table"><thead><tr>
    <th>agent</th><th>status</th><th>tmux</th><th>last line</th><th>updated</th><th>session</th>
  </tr></thead><tbody>`;
  for (const a of agents) {
    const badge = a.badge ? `<span class="api-base">${escHtml(a.badge)}</span> ` : "";
    h += `<tr>
      <td>${badge}${escHtml(a.name)}</td>
      <td>${statusCell(a)}</td>
      <td><span class="api-base">${escHtml(a.target || a.session || "")}</span></td>
      <td style="max-width:28rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text-dim)" title="${escHtml(a.last_line || "")}">${escHtml(a.last_line || "")}</td>
      <td>${updated(a.updated_at)}</td>
      <td>${sessionCell(a)}</td>
    </tr>`;
  }
  return h + "</tbody></table></div>";
}

async function load() {
  const body = root && root.querySelector("#agentsBody");
  if (!body) return;
  const base = ctx.config.monitorUrl;
  try {
    const res = await fetch(`${base}/api/agents`, { cache: "no-store" });
    if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
    const agents = await res.json();
    if (root) body.innerHTML = table(Array.isArray(agents) ? agents : []);
  } catch (e) {
    if (!root) return;
    const { escHtml } = ctx.fmt;
    body.innerHTML = `<div class="node-card"><p class="error">agent-monitor at <span class="api-base">${escHtml(base)}</span> is unreachable: ${escHtml(String(e.message || e))}</p>
      <p class="chat-placeholder">start it with <span class="api-base">pitf monitor</span> (or <span class="api-base">agent-monitor</span>)</p></div>`;
  }
}

export default {
  id: "agents",
  label: "Agents",
  mount(r, c) {
    root = r;
    ctx = c;
    const { escHtml } = ctx.fmt;
    const base = ctx.config.monitorUrl;
    root.innerHTML = `
      <div class="section-title">Agents
        <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(from agent-monitor at <a href="${escHtml(base)}/" target="_blank" rel="noopener" style="color:var(--accent)">${escHtml(base)}</a> ↗, which also has the task board)</span>
      </div>
      <div id="agentsBody"><p class="tab-placeholder">loading…</p></div>`;
    stopPoll = ctx.poll(load, 3000);
  },
  unmount() {
    if (stopPoll) stopPoll();
    stopPoll = null;
    root = null;
  },
};
