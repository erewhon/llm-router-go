// The Tokens section and the Chat dialog. Chat's <dialog> markup lives in
// index.html so the header button works from every tab; Tokens render into
// the Connect tab's #tokBody (openTokens navigates there). Ported from the
// legacy single-file dashboard on 2026-09-12. Importing it installs
// window.dashDialogs = {openTokens, openChat, closeChat, sendChat, stopChat}
// and binds the controls by delegation.
import { escHtml, fmtAgoIso, copyText, chatCapable } from "/static/lib/fmt.js";

const cfg = () => window.DASH_CONFIG || {};
const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------------------
// Access tokens: list / mint / revoke the signed-in person's own PATs. The
// listener verifies identity via the front proxy's shared secret; reached any
// other way, /api/tokens answers 401/503 and the dialog says so.
// ---------------------------------------------------------------------------
let tokState = null;

const SCOPE_LABEL = {
  "models:local": "local only — fleet hardware, nothing billed",
  "models:local_or_zdr": "local or zero-retention cloud",
  "models:*": "unrestricted — any model, including paid providers",
};

// openTokens takes the reader to the Tokens section of the Connect tab.
export function openTokens() {
  location.hash = "#connect?tokens=1";
}

export async function loadTokens(reveal) {
  const body = $("tokBody");
  if (!body) return; // the Connect tab is not mounted
  try {
    const r = await fetch("/api/tokens", { cache: "no-store" });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      tokState = null;
      $("tokWho").textContent = "";
      body.innerHTML = `<p class="chat-err">${escHtml(d.error || "HTTP " + r.status)}</p>`;
      return;
    }
    tokState = d;
    renderTokens(reveal);
  } catch (e) {
    body.innerHTML = `<p class="chat-err">${escHtml(e.message)}</p>`;
  }
}

export function renderTokens(reveal, err) {
  const d = tokState;
  if (!$("tokBody")) return;
  $("tokWho").textContent = d.principal + (d.owner ? " · owner" : "");
  const allowed = d.allowed_scopes || ["models:local"];
  const defaultScope = allowed.includes("models:local") ? "models:local" : allowed[allowed.length - 1];
  let h = "";
  if (err) h += `<div class="tok-err">${escHtml(err)}</div>`;
  if (reveal) {
    h += `<div class="tok-reveal">
      <div><strong>New token “${escHtml(reveal.label)}” — copy it now.</strong> Only its hash is stored, so it cannot be shown again.</div>
      <div class="tok-secret"><code id="tokSecret">${escHtml(reveal.token)}</code><button class="copy-btn" data-action="copy-secret">copy</button></div>
      <div class="tok-hint" style="margin:0.5rem 0 0">Use it as the API key at <code>${escHtml(cfg().apiBase || "")}/v1</code>. OpenCode: <code>/connect</code> → Other → provider id <code>${escHtml(cfg().providerId || "llm")}</code> → paste.</div>
    </div>`;
  }
  h += `<form class="tok-form" id="tokForm">
      <input id="tokLabel" placeholder="label (laptop, opencode, night-agent)" maxlength="80" required>
      <select id="tokScope">${allowed.map((s) => `<option value="${escHtml(s)}"${s === defaultScope ? " selected" : ""}>${escHtml(SCOPE_LABEL[s] || s)}</option>`).join("")}</select>
      <select id="tokExp"><option value="0">never expires</option><option value="30">30 days</option><option value="90">90 days</option><option value="365" selected>1 year</option></select>
      <button id="tokMint" type="submit">Mint</button>
    </form>
    <div class="tok-hint">${
      d.owner
        ? "You are an owner: you may mint any scope. Blank defaults to local-only."
        : "Your tokens are local-only: they reach fleet hardware and can never bill a cloud provider."
    }</div>`;
  const toks = d.tokens || [];
  if (!toks.length) {
    h += '<p class="chat-placeholder">No tokens yet.</p>';
  } else {
    const fmtDay = (iso) => (iso ? new Date(iso).toISOString().slice(0, 10) : "—");
    h +=
      `<table class="tok-table"><thead><tr><th>label</th><th>id</th><th>scope</th><th>created</th><th>last used</th><th>expires</th><th>state</th><th></th></tr></thead><tbody>` +
      toks
        .map(
          (t) => `<tr class="${t.state === "active" ? "" : "dead"}">
        <td>${escHtml(t.label) || "—"}</td>
        <td class="tok-id">${escHtml(t.id)}</td>
        <td>${escHtml(t.scope)}</td>
        <td>${fmtDay(t.created_at)}</td>
        <td>${t.last_used_at ? fmtAgoIso(t.last_used_at) : "never"}</td>
        <td>${t.expires_at ? fmtDay(t.expires_at) : "never"}</td>
        <td>${escHtml(t.state)}</td>
        <td>${t.state === "active" ? `<button class="tok-revoke" data-action="revoke" data-id="${escHtml(t.id)}" data-label="${escHtml(t.label)}">revoke</button>` : ""}</td>
      </tr>`,
        )
        .join("") +
      "</tbody></table>";
  }
  $("tokBody").innerHTML = h;
}

export async function mintToken(ev) {
  ev.preventDefault();
  const btn = $("tokMint");
  btn.disabled = true;
  const payload = {
    label: $("tokLabel").value.trim(),
    scope: $("tokScope").value,
    expires_days: parseInt($("tokExp").value, 10) || 0,
  };
  try {
    const r = await fetch("/api/tokens", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      renderTokens(null, d.error || "HTTP " + r.status);
      return;
    }
    await loadTokens(d);
  } catch (e) {
    renderTokens(null, e.message);
  } finally {
    btn.disabled = false;
  }
}

export async function revokeToken(id, label) {
  if (!confirm(`Revoke token “${label || id}”? Anything using it stops working on its next request.`)) return;
  try {
    const r = await fetch("/api/tokens/" + encodeURIComponent(id), { method: "DELETE" });
    if (!r.ok && r.status !== 204) {
      const d = await r.json().catch(() => ({}));
      renderTokens(null, d.error || "HTTP " + r.status);
      return;
    }
    await loadTokens();
  } catch (e) {
    renderTokens(null, e.message);
  }
}

// ---------------------------------------------------------------------------
// Quick Chat: one user message -> one model, streamed, with TTFT/tok/s metrics.
// ---------------------------------------------------------------------------
let chatAbort = null;

// State glyph for a picker option: <option> cannot carry a coloured dot,
// so the verdict rides as a prefix.
function stateGlyph(m) {
  switch (m.availability) {
    case "warming":
      return "◐ ";
    case "absent":
    case "unavailable":
      return "✕ ";
    default:
      return m.agent_state === "stopped" ? "○ " : "● ";
  }
}

// openChat({model}) opens the dialog with a grouped picker: roles first,
// then local seats, then providers, then discovered. The picker reads the
// catalog and the fleet itself so it works from any tab.
export async function openChat(opts = {}) {
  const sel = $("chatModel");
  sel.innerHTML = '<option value="">(model list loading…)</option>';
  $("chatDialog").showModal();
  $("chatInput").focus();
  let models = [],
    roles = [];
  try {
    const [c, f] = await Promise.all([
      fetch("/api/catalog", { cache: "no-store" }).then((r) => r.json()),
      fetch("/api/fleet", { cache: "no-store" })
        .then((r) => r.json())
        .catch(() => ({})),
    ]);
    models = (c.models || []).filter(chatCapable);
    roles = f.roles || [];
  } catch (_) {
    /* leave the placeholder */
  }
  const group = (label, items) => (items.length ? `<optgroup label="${escHtml(label)}">${items.join("")}</optgroup>` : "");
  const opt = (value, label) => `<option value="${escHtml(value)}">${escHtml(label)}</option>`;
  const local = models.filter((m) => (m.nodes || []).length);
  const providers = models.filter((m) => !(m.nodes || []).length && m.backend === "external" && !m.discovered);
  const discovered = models.filter((m) => m.discovered);
  const html =
    group(
      "roles",
      roles.map((r) => opt(r.role, `${r.available ? "● " : "✕ "}${r.role}${r.target ? " → " + r.target : ""}`)),
    ) +
    group(
      "local seats",
      local.map((m) => opt(m.id, `${stateGlyph(m)}${m.id} · ${m.head_node || m.nodes[0]}`)),
    ) +
    group(
      "providers",
      providers.map((m) => opt(m.id, `${stateGlyph(m)}${m.id}`)),
    ) +
    group(
      "discovered",
      discovered.map((m) => opt(m.id, `${stateGlyph(m)}${m.id}`)),
    );
  sel.innerHTML = html || '<option value="">(no chat-capable models)</option>';
  const want = opts.model || localStorage.getItem("chatModel");
  if (want && sel.querySelector(`option[value="${CSS.escape(want)}"]`)) sel.value = want;
}

export function closeChat() {
  stopChat();
  $("chatDialog").close();
}

export function stopChat() {
  if (chatAbort) {
    chatAbort.abort();
    chatAbort = null;
  }
  const send = $("chatSend"),
    stop = $("chatStop");
  send.disabled = false;
  send.style.display = "";
  stop.style.display = "none";
}

export async function sendChat() {
  const model = $("chatModel").value;
  const input = $("chatInput");
  const msg = input.value.trim();
  if (!model || !msg) return;
  localStorage.setItem("chatModel", model);

  const out = $("chatOutput");
  const metricsEl = $("chatMetrics");
  out.innerHTML = "";
  const reasoningWrap = document.createElement("details");
  reasoningWrap.className = "chat-reasoning";
  reasoningWrap.innerHTML = '<summary>Reasoning</summary><div class="chat-reasoning-body"></div>';
  const reasoningBody = reasoningWrap.querySelector(".chat-reasoning-body");
  const answer = document.createElement("div");
  answer.className = "chat-answer";
  out.appendChild(answer);
  metricsEl.textContent = "";

  const send = $("chatSend"),
    stop = $("chatStop");
  send.disabled = true;
  send.style.display = "none";
  stop.style.display = "";

  const t0 = performance.now();
  let tFirst = null,
    deltaTokens = 0,
    usageTokens = null,
    promptTokens = null;
  let answerText = "",
    reasoningText = "",
    gotReasoning = false;
  chatAbort = new AbortController();

  const fmtMetrics = (done) => {
    const now = performance.now();
    const elapsed = (now - t0) / 1000;
    const ttft = tFirst !== null ? (tFirst - t0) / 1000 : null;
    const toks = usageTokens !== null ? usageTokens : deltaTokens;
    const genWindow = (tFirst !== null ? now - tFirst : 0) / 1000;
    const tps = toks > 0 && genWindow > 0.01 ? toks / genWindow : null;
    const parts = [
      "TTFT " + (ttft !== null ? ttft.toFixed(2) + "s" : "—"),
      (tps !== null ? tps.toFixed(1) : "—") + " tok/s",
      toks + " tok" + (usageTokens === null ? " (est)" : ""),
      elapsed.toFixed(1) + "s elapsed",
    ];
    if (promptTokens !== null) parts.push(promptTokens + " prompt");
    metricsEl.textContent = parts.join("  ·  ") + (done ? "" : "  …");
  };

  try {
    const resp = await fetch("/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: model, message: msg }),
      signal: chatAbort.signal,
    });
    if (!resp.ok || !resp.body) throw new Error("HTTP " + resp.status);
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    streamLoop: while (true) {
      const r = await reader.read();
      if (r.done) break;
      buf += dec.decode(r.value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n\n")) !== -1) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        for (const line of frame.split("\n")) {
          const s = line.trim();
          if (!s.startsWith("data:")) continue;
          const data = s.slice(5).trim();
          if (data === "[DONE]") continue;
          let obj;
          try {
            obj = JSON.parse(data);
          } catch (e) {
            continue;
          }
          if (obj.error) {
            answer.innerHTML = '<span class="chat-err">' + escHtml(obj.error) + "</span>";
            chatAbort.abort();
            break streamLoop;
          }
          const choice = (obj.choices && obj.choices[0]) || {};
          const delta = choice.delta || {};
          if (delta.reasoning_content) {
            if (tFirst === null) tFirst = performance.now();
            deltaTokens++;
            reasoningText += delta.reasoning_content;
            if (!gotReasoning) {
              gotReasoning = true;
              out.insertBefore(reasoningWrap, answer);
            }
            reasoningBody.textContent = reasoningText;
          }
          if (delta.content) {
            if (tFirst === null) tFirst = performance.now();
            deltaTokens++;
            answerText += delta.content;
            answer.textContent = answerText;
          }
          if (obj.usage) {
            if (obj.usage.completion_tokens != null) usageTokens = obj.usage.completion_tokens;
            if (obj.usage.prompt_tokens != null) promptTokens = obj.usage.prompt_tokens;
          }
          fmtMetrics(false);
          out.scrollTop = out.scrollHeight;
        }
      }
    }
    fmtMetrics(true);
  } catch (e) {
    if (e && e.name === "AbortError") {
      fmtMetrics(true);
      metricsEl.textContent += " (stopped)";
    } else {
      if (!answerText) answer.innerHTML = '<span class="chat-err">' + escHtml((e && e.message) || e) + "</span>";
      fmtMetrics(true);
    }
  } finally {
    stopChat();
  }
}

// Wiring: the dialogs' own controls, delegated so re-rendered token bodies
// need no inline handlers. Cmd/Ctrl+Enter sends; Esc closes natively.
function bind() {
  $("chatClose")?.addEventListener("click", closeChat);
  $("chatSend")?.addEventListener("click", sendChat);
  $("chatStop")?.addEventListener("click", stopChat);
  // Tokens controls are delegated on the document: the section is
  // re-rendered by the Connect tab and by every list refresh.
  document.addEventListener("submit", (ev) => {
    if (ev.target.id === "tokForm") mintToken(ev);
  });
  document.addEventListener("click", (ev) => {
    const b = ev.target.closest("#tokBody button[data-action]");
    if (!b) return;
    if (b.dataset.action === "revoke") revokeToken(b.dataset.id, b.dataset.label);
    if (b.dataset.action === "copy-secret") copyText($("tokSecret").textContent, b);
  });
  document.addEventListener("keydown", (e) => {
    const dlg = $("chatDialog");
    if (dlg && dlg.open && (e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      sendChat();
    }
  });
}
bind();
window.dashDialogs = { openTokens, loadTokens, openChat, closeChat, sendChat, stopChat };
