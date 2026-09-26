// Agents tab, task board half: agent-monitor's Kanban cards (GET/POST/PATCH/
// DELETE /api/tasks, POST /api/launch) through the router's same-origin
// /monitor/ proxy — the writes the CORS-only MVP could not do. Rendered as a
// compact grouped table rather than a drag board; the columns are
// agent-monitor's (backlog, active, needs_input, done; archived hidden).
//
// Optimistic: an action changes the local copy and re-renders at once, then
// the server's answer (or the next poll) replaces it; a failure shows the
// message inline and forces a refetch. Deleting takes two clicks.
let root = null;
let ctx = null;
let base = "";
let tasks = [];
let projects = [];
let pendingDelete = null; // task id awaiting the second click
let error = "";
let editing = null; // task id whose title is an input

export const COLUMNS = ["backlog", "active", "needs_input", "done"];
const COLUMN_LABEL = { backlog: "backlog", active: "active", needs_input: "needs input", done: "done" };
const COLUMN_COLOR = { backlog: "var(--text-dim)", active: "var(--green, #3fb950)", needs_input: "var(--yellow)", done: "var(--accent)" };

async function refresh() {
  const [t, p] = await Promise.all([
    ctx.api.get(`${base}/api/tasks`),
    ctx.api.get(`${base}/api/projects`).catch(() => []),
  ]);
  tasks = Array.isArray(t) ? t : [];
  projects = Array.isArray(p) ? p : [];
  render();
}

// act runs a write optimistically: apply mutates the local copy, the
// request goes out, and either way the list is refetched.
async function act(apply, request) {
  error = "";
  apply();
  render();
  try {
    await request();
  } catch (e) {
    error = String(e.message || e);
  }
  try {
    await refresh();
  } catch (e) {
    error = error || String(e.message || e);
    render();
  }
}

function create(title, group) {
  return act(
    () => tasks.push({ id: -Date.now(), title, group, column: "backlog", pending: true }),
    () => ctx.api.send("POST", `${base}/api/tasks`, { title, group }),
  );
}

function patch(id, fields) {
  return act(
    () => {
      const t = tasks.find((x) => x.id === id);
      if (t) Object.assign(t, fields);
    },
    () => ctx.api.send("PATCH", `${base}/api/tasks/${id}`, fields),
  );
}

function remove(id) {
  return act(
    () => {
      tasks = tasks.filter((x) => x.id !== id);
    },
    () => ctx.api.send("DELETE", `${base}/api/tasks/${id}`),
  );
}

function launch(id, project) {
  const t = tasks.find((x) => x.id === id);
  return act(
    () => {
      if (t) t.session_name = project;
    },
    () => ctx.api.send("POST", `${base}/api/launch`, { project, task_id: id, task: t ? t.title : "" }),
  );
}

function projectName(p) {
  // agent-monitor's ProjectConfig has yaml tags only, so JSON keys are
  // the Go field names.
  return p.name || p.Name || "";
}

function row(t) {
  const { escHtml } = ctx.fmt;
  const id = t.id;
  const title =
    editing === id
      ? `<input class="task-title-input" data-edit="${id}" value="${escHtml(t.title)}" autofocus>`
      : `<span class="task-title" data-action="edit" data-id="${id}" title="click to rename">${escHtml(t.title)}</span>`;
  const link = t.url ? ` <a href="${escHtml(t.url)}" target="_blank" rel="noopener" style="color:var(--accent)" title="${escHtml(t.source || "link")}">↗</a>` : "";
  const src = t.source ? `<span class="badge badge-tag">${escHtml(t.source)}</span>` : "";
  const session = t.session_name ? `<span class="api-base" title="tmux session">${escHtml(t.session_name)}</span>` : `<span style="color:var(--text-dim)">–</span>`;
  const move = `<select class="task-move" data-action="move" data-id="${id}" title="move to column">${COLUMNS.map(
    (c) => `<option value="${c}"${c === t.column ? " selected" : ""}>${COLUMN_LABEL[c]}</option>`,
  ).join("")}</select>`;
  const launchCtl = projects.length
    ? `<select class="task-move" data-action="launch" data-id="${id}" title="launch an agent on this task"><option value="">launch…</option>${projects
        .map((p) => `<option value="${escHtml(projectName(p))}">${escHtml(projectName(p))}</option>`)
        .join("")}</select>`
    : "";
  const del =
    pendingDelete === id
      ? `<button class="copy-btn task-del-confirm" data-action="delete-confirm" data-id="${id}">delete?</button>`
      : `<button class="copy-btn" data-action="delete" data-id="${id}" title="delete">✕</button>`;
  return `<tr class="task-row${t.pending ? " task-pending" : ""}" data-task="${id}">
    <td><span style="color:${COLUMN_COLOR[t.column] || "var(--text-dim)"}">&#9679;</span> ${COLUMN_LABEL[t.column] || escHtml(t.column || "")}</td>
    <td>${title}${link}</td>
    <td>${t.group ? escHtml(t.group) : ""}</td>
    <td>${src}</td>
    <td>${session}</td>
    <td>${updatedAgo(t.updated_at)}</td>
    <td class="task-actions">${move}${launchCtl}${del}</td>
  </tr>`;
}

function updatedAgo(iso) {
  const ms = iso ? Date.parse(iso) : NaN;
  return Number.isNaN(ms) || ms < Date.UTC(2000, 0, 1) ? "–" : ctx.fmt.fmtAgoIso(iso);
}

export function render() {
  if (!root) return;
  const { escHtml } = ctx.fmt;
  const byCol = COLUMNS.map((c) => tasks.filter((t) => t.column === c));
  const counts = COLUMNS.map((c, i) => `<span style="color:${COLUMN_COLOR[c]}">${byCol[i].length} ${COLUMN_LABEL[c]}</span>`).join(" · ");
  let h = `<div class="section-title" style="margin-top:1.25rem">Tasks
      <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(agent-monitor's board · ${counts})</span></div>
    <form class="task-new" data-action="create">
      <input class="chat-input task-new-title" name="title" placeholder="new task title…" autocomplete="off" required>
      <input class="chat-input task-new-group" name="group" placeholder="group (optional)" autocomplete="off">
      <button type="submit" class="chat-send">add</button>
    </form>`;
  if (error) h += `<p class="error" style="margin:0.25rem 0">${escHtml(error)}</p>`;
  if (!tasks.length) {
    h += `<div class="node-card"><p class="chat-placeholder">no tasks on the board</p></div>`;
  } else {
    const order = new Map(COLUMNS.map((c, i) => [c, i]));
    const sorted = [...tasks].sort((a, b) => (order.get(a.column) ?? 9) - (order.get(b.column) ?? 9) || Date.parse(b.updated_at || 0) - Date.parse(a.updated_at || 0));
    h += `<div class="node-card" style="padding:0.5rem 0.9rem;overflow-x:auto"><table class="usage-table"><thead><tr>
      <th>column</th><th>task</th><th>group</th><th>source</th><th>session</th><th>updated</th><th></th>
    </tr></thead><tbody>${sorted.map(row).join("")}</tbody></table></div>`;
  }
  root.innerHTML = h;
  const inp = root.querySelector("input[data-edit]");
  if (inp) {
    inp.focus();
    inp.select();
  }
}

function onClick(ev) {
  const el = ev.target.closest("[data-action]");
  if (!el || el.tagName === "SELECT" || el.tagName === "FORM") return;
  const id = Number(el.dataset.id);
  switch (el.dataset.action) {
    case "edit":
      editing = id;
      pendingDelete = null;
      render();
      break;
    case "delete":
      pendingDelete = id;
      render();
      break;
    case "delete-confirm":
      pendingDelete = null;
      remove(id);
      break;
  }
}

function onChange(ev) {
  const el = ev.target.closest("select[data-action]");
  if (!el) return;
  const id = Number(el.dataset.id);
  if (el.dataset.action === "move") patch(id, { column: el.value });
  if (el.dataset.action === "launch" && el.value) launch(id, el.value);
}

function onSubmit(ev) {
  const form = ev.target.closest('form[data-action="create"]');
  if (!form) return;
  ev.preventDefault();
  const title = form.title.value.trim();
  if (!title) return;
  const group = form.group.value.trim();
  form.reset();
  create(title, group);
}

function onKey(ev) {
  const inp = ev.target.closest("input[data-edit]");
  if (!inp) return;
  if (ev.key === "Enter") {
    ev.preventDefault();
    commitEdit(inp);
  } else if (ev.key === "Escape") {
    editing = null;
    render();
  }
}

function onBlur(ev) {
  const inp = ev.target.closest && ev.target.closest("input[data-edit]");
  if (inp && editing !== null) commitEdit(inp);
}

function commitEdit(inp) {
  const id = Number(inp.dataset.edit);
  const title = inp.value.trim();
  const t = tasks.find((x) => x.id === id);
  editing = null;
  if (!title || !t || t.title === title) {
    render();
    return;
  }
  patch(id, { title });
}

export function mountTasks(r, c, monitorBase) {
  root = r;
  ctx = c;
  base = monitorBase;
  tasks = [];
  projects = [];
  error = "";
  pendingDelete = null;
  editing = null;
  root.addEventListener("click", onClick);
  root.addEventListener("change", onChange);
  root.addEventListener("submit", onSubmit);
  root.addEventListener("keydown", onKey);
  root.addEventListener("focusout", onBlur);
  root.innerHTML = `<p class="tab-placeholder">loading tasks…</p>`;
  return refresh;
}

export function unmountTasks() {
  if (root) {
    root.removeEventListener("click", onClick);
    root.removeEventListener("change", onChange);
    root.removeEventListener("submit", onSubmit);
    root.removeEventListener("keydown", onKey);
    root.removeEventListener("focusout", onBlur);
  }
  root = null;
}
