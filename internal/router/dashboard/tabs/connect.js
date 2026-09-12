// Connect tab — "how do I use it, as me?" The Connection quick start and
// example, and the Apps links. Ported from the legacy render() on
// 2026-09-12. The Tokens and Chat dialogs stay dialogs (lib/dialogs.js,
// opened from the header buttons on every tab); Phase 4 makes Tokens a
// section here.
let root = null;
let ctx = null;

function render(models) {
  const { escHtml } = ctx.fmt;
  const publicUrl = ctx.config.apiBase || "";
  const providerId = ctx.config.providerId || "";
  const keyHint = ctx.config.apiKey || "pat_…";
  const example = models[0]?.aliases?.[0] || models[0]?.id || "MODEL";
  return `
    <div class="section-title">Connection</div>
    <div class="nodes">
      <div class="node-card" style="flex:2">
        <div class="node-name">Quick Start</div>
        <div class="node-detail" style="margin-top:0.5rem">
          <strong>Host:</strong> <span class="api-base">${escHtml(publicUrl)}</span>
          <button class="copy-btn" data-copy="${escHtml(publicUrl)}">copy</button>
        </div>
        <div class="node-detail" style="margin-top:0.5rem">
          <strong>API Key:</strong> <span>your personal access token — click <a href="#" data-action="tokens" style="color:var(--accent)">&#128273; Tokens</a> to mint one</span>
        </div>
        ${
          providerId
            ? `<div class="node-detail" style="margin-top:0.5rem">
          <strong>OpenCode:</strong> <span>run <span class="api-base">/connect</span> (or <span class="api-base">opencode auth login</span>), choose <em>Other</em>, provider id <span class="api-base">${escHtml(providerId)}</span>, paste the token</span>
        </div>`
            : ""
        }
        <div class="node-detail" style="margin-top:0.5rem">
          <strong>Model:</strong> <span>use the model ID or any alias from the <a href="#catalog" style="color:var(--accent)">Catalog</a></span>
        </div>
      </div>
      <div class="node-card" style="flex:3">
        <div class="node-name">Example</div>
        <div style="margin-top:0.5rem">
          <span class="api-base">curl ${escHtml(publicUrl)}/v1/chat/completions \\<br>
          &nbsp;&nbsp;-H "Authorization: Bearer ${escHtml(keyHint)}" \\<br>
          &nbsp;&nbsp;-H "Content-Type: application/json" \\<br>
          &nbsp;&nbsp;-d '{"model":"${escHtml(example)}","messages":[{"role":"user","content":"Hello"}]}'</span>
        </div>
      </div>
    </div>
    <div class="section-title">Apps</div>
    <div class="nodes">
      <div class="node-card" style="cursor:default; display:flex; gap:1rem; flex-wrap:wrap; align-items:center; padding:0.75rem 1.25rem">
        <a href="https://grafana.bcc.sh" target="_blank" style="color:var(--accent);text-decoration:none;font-size:0.85rem;font-weight:500">Grafana</a>
        <a href="https://llm-dashboard.bcc.sh" target="_blank" style="color:var(--accent);text-decoration:none;font-size:0.85rem;font-weight:500">Dashboard</a>
        <a href="http://192.168.42.159:5403" target="_blank" style="color:var(--accent);text-decoration:none;font-size:0.85rem;font-weight:500">ACE-Step Music</a>
      </div>
    </div>`;
}

function onClick(ev) {
  const copy = ev.target.closest("[data-copy]");
  if (copy) return ctx.fmt.copyText(copy.dataset.copy, copy);
  const tok = ev.target.closest('[data-action="tokens"]');
  if (tok) {
    ev.preventDefault();
    window.dashDialogs?.openTokens?.();
  }
}

export default {
  id: "connect",
  label: "Connect",
  mount(r, c) {
    root = r;
    ctx = c;
    root.innerHTML = render([]);
    root.addEventListener("click", onClick);
    // Only the example's model name depends on the catalog; one fetch.
    ctx.api
      .get("/api/models")
      .then((d) => {
        if (root) root.innerHTML = render(d.models || []);
      })
      .catch(() => {});
  },
  unmount() {
    if (root) root.removeEventListener("click", onClick);
    root = null;
  },
};
