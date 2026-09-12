// Connect tab — "how do I use it, as me?" Connection quick start and
// example, Access tokens (mint / list / revoke, inline since Phase 4), and
// the Apps links. Chat stays a dialog (lib/dialogs.js) opened from the
// header or the "try it" button. Minting a token and making a first request
// is one screen, top to bottom.
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
          <strong>Model:</strong> <span>use a role (<span class="api-base">coder</span>), a model ID or any alias from the <a href="#catalog" style="color:var(--accent)">Catalog</a></span>
        </div>
        <div style="margin-top:0.7rem"><button class="chat-open-btn" data-action="try">&#128172; try it &mdash; chat with coder</button></div>
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
    <div class="section-title">Access tokens <span id="tokWho" class="tok-who" style="font-weight:400;font-size:0.8rem"></span></div>
    <div class="node-card" id="tokCard"><div id="tokBody" class="tok-body"><p class="chat-placeholder">Loading&#8230;</p></div></div>
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
  if (ev.target.closest('[data-action="try"]')) return window.dashDialogs?.openChat?.({ model: "coder" });
  const tok = ev.target.closest('[data-action="tokens"]');
  if (tok) {
    ev.preventDefault();
    root.querySelector("#tokCard")?.scrollIntoView({ behavior: "smooth", block: "start" });
  }
}

function wantsTokens() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  return new URLSearchParams(qs || "").get("tokens") === "1";
}

export default {
  id: "connect",
  label: "Connect",
  mount(r, c) {
    root = r;
    ctx = c;
    root.innerHTML = render([]);
    root.addEventListener("click", onClick);
    const scroll = wantsTokens();
    const tokens = () => {
      window.dashDialogs?.loadTokens?.();
      if (scroll) root.querySelector("#tokCard")?.scrollIntoView({ behavior: "smooth", block: "start" });
    };
    tokens();
    // Only the example's model name depends on the catalog; one fetch.
    ctx.api
      .get("/api/catalog")
      .then((d) => {
        if (!root) return;
        root.innerHTML = render(d.models || []);
        tokens();
      })
      .catch(() => {});
  },
  unmount() {
    if (root) root.removeEventListener("click", onClick);
    root = null;
  },
};
