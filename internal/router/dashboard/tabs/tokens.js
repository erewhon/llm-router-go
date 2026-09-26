// Tokens tab — tokenator (the token profiler: session browser, transcripts,
// per-model pages) shown inside the dashboard. Phase 1 of the unified web
// UI: tokenator is server-rendered, so this tab frames it through the
// router's same-origin /tokens/ proxy (ctx.config.tokensUrl) in tokenator's
// embedded mode (?embed=1: no chrome, the dashboard palette, jump links).
// Same origin means the SSO cookie applies and the frame's location is
// readable, so the hash follows navigation inside the frame and a reload
// lands on the same page.
//
// Hash grammar: #tokens → session list; #tokens?session=<id> → profile;
// #tokens?session=<id>&view=transcript[&q=…] → transcript;
// #tokens?model=<alias> → model page; #tokens?q=… → content search.
//
// The frame's pages post {type:"tokenator-jump", tab, session?, model?} for
// links that leave tokenator (router requests, catalog); the tab turns them
// into the shell's hash grammar. Phase 2 replaces the frame with a native
// tab over tokenator's JSON API.
let root = null;
let ctx = null;
let frame = null;
let onMessage = null;

function query() {
  const [, qs] = (location.hash || "").replace(/^#/, "").split("?", 2);
  return new URLSearchParams(qs || "");
}

// frameURL maps the hash query to the page tokenator should show.
export function frameURL(base, q) {
  const embed = new URLSearchParams({ embed: "1" });
  const session = q.get("session");
  const model = q.get("model");
  const search = q.get("q");
  if (session) {
    const path = `${base}/session/${encodeURIComponent(session)}${q.get("view") === "transcript" ? "/transcript" : ""}`;
    if (search) embed.set("q", search);
    return `${path}?${embed}`;
  }
  if (model) return `${base}/model/${encodeURIComponent(model)}?${embed}`;
  if (search) embed.set("q", search);
  return `${base}/?${embed}`;
}

// hashFor is the inverse: the frame's current location as a hash, so the
// address bar follows the user around inside tokenator.
export function hashFor(base, pathname, search) {
  const rel = pathname.startsWith(base) ? pathname.slice(base.length) : pathname;
  const params = new URLSearchParams(search || "");
  const out = new URLSearchParams();
  let m;
  if ((m = rel.match(/^\/session\/([^/]+)(\/transcript)?$/))) {
    out.set("session", decodeURIComponent(m[1]));
    if (m[2]) out.set("view", "transcript");
  } else if ((m = rel.match(/^\/model\/([^/]+)$/))) {
    out.set("model", decodeURIComponent(m[1]));
  }
  if (params.get("q")) out.set("q", params.get("q"));
  const qs = out.toString();
  return `#tokens${qs ? "?" + qs : ""}`;
}

// jumpHash turns a tokenator-jump message into the shell's hash, or null.
export function jumpHash(msg) {
  if (!msg || msg.type !== "tokenator-jump" || !/^[a-z]+$/.test(msg.tab || "")) return null;
  if (msg.session) return `#${msg.tab}?session=${encodeURIComponent(msg.session)}`;
  if (msg.model) return `#${msg.tab}?model=${encodeURIComponent(msg.model)}`;
  return `#${msg.tab}`;
}

export default {
  id: "tokens",
  label: "Tokens",
  mount(r, c) {
    root = r;
    ctx = c;
    const { escHtml } = ctx.fmt;
    const base = ctx.config.tokensUrl;
    root.innerHTML = `
      <div class="section-title">Tokens
        <span style="color:var(--text-dim);font-weight:400;font-size:0.8rem">(tokenator, via <span class="api-base">${escHtml(base)}/</span> ·
          <a href="${escHtml(base)}/?embed=0" target="_blank" rel="noopener" style="color:var(--accent)">open standalone ↗</a>)</span>
      </div>
      <iframe class="tokens-frame" title="tokenator" src="${escHtml(frameURL(base, query()))}"></iframe>`;
    frame = root.querySelector("iframe");
    // Follow the frame: same origin, so its location is readable on load.
    frame.addEventListener("load", () => {
      try {
        const loc = frame.contentWindow.location;
        const h = hashFor(base, loc.pathname, loc.search);
        if (h !== location.hash) history.replaceState(null, "", h);
      } catch (e) {
        /* cross-origin (should not happen behind the proxy): leave the hash */
      }
    });
    onMessage = (ev) => {
      if (!frame || ev.source !== frame.contentWindow) return;
      const h = jumpHash(ev.data);
      if (h) location.hash = h;
    };
    window.addEventListener("message", onMessage);
  },
  unmount() {
    if (onMessage) window.removeEventListener("message", onMessage);
    onMessage = null;
    frame = null;
    root = null;
  },
};
