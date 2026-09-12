// Dashboard v2 shell: hash-routed tabs over a shared header.
//
// One tab is mounted at a time. A tab is a module exporting
// {id, label, mount(root, ctx), unmount()}; it owns everything under `root`
// and every poll it starts. The shell owns the header strip, the tab nav and
// the hash. Design: Forge page "LLM Router Dashboard: split into sections +
// a live Activity view".
import { api } from "/static/lib/api.js";
import * as fmt from "/static/lib/fmt.js";
import * as dialogs from "/static/lib/dialogs.js"; // installs window.dashDialogs + binds the dialogs
import activity from "/static/tabs/activity.js";
import fleet from "/static/tabs/fleet.js";
import catalog from "/static/tabs/catalog.js";
import traffic from "/static/tabs/traffic.js";
import connect from "/static/tabs/connect.js";

const TABS = [activity, fleet, catalog, traffic, connect];
// Fleet until the Activity view lands (its leaf makes #activity the default):
// an empty placeholder is the wrong first thing to see behind the front door.
const DEFAULT_TAB = "fleet";

// poll(fn, ms): run fn now and every ms while the owning tab is mounted and
// the page is visible. Returns a stop function. Every tab's unmount must
// stop what its mount started; the shell also stops everything it knows
// about when switching tabs, so a forgotten stop cannot leak past a switch.
let livePolls = new Set();
function makePoll() {
  return function poll(fn, ms) {
    let timer = null;
    let stopped = false;
    const tick = async () => {
      if (stopped) return;
      if (document.visibilityState === "visible") {
        try {
          await fn();
        } catch (e) {
          console.warn("poll:", e);
        }
      }
      if (!stopped) timer = setTimeout(tick, ms);
    };
    const stop = () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      livePolls.delete(stop);
    };
    livePolls.add(stop);
    tick();
    return stop;
  };
}
// A hidden page skips its polls; when it comes back, fire them at once.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && current) {
    // Re-mount is the simplest "refresh everything" — tabs are cheap to mount.
    switchTo(current.id, true);
  }
});

const ctx = { config: window.DASH_CONFIG || {}, api, fmt, poll: makePoll() };

let current = null;

// Hash grammar: "#<tab>" or "#<tab>?k=v&k2=v2". Tabs keep their own state in
// the query so a link is a view and a reload keeps the filters.
export function parseHash() {
  const raw = (location.hash || "").replace(/^#/, "");
  const [id, qs] = raw.split("?", 2);
  return { id: id || DEFAULT_TAB, query: new URLSearchParams(qs || "") };
}

function renderNav(activeId) {
  const nav = document.getElementById("tabs");
  nav.innerHTML = TABS.map(
    (t) =>
      `<a href="#${t.id}" class="${t.id === activeId ? "active" : ""}" data-tab="${t.id}">${fmt.escHtml(t.label)}</a>`,
  ).join("");
}

function switchTo(id, force = false) {
  const tab = TABS.find((t) => t.id === id) || TABS.find((t) => t.id === DEFAULT_TAB);
  if (current && current.id === tab.id && !force) return;
  if (current) {
    try {
      current.unmount();
    } catch (e) {
      console.warn("unmount:", e);
    }
  }
  for (const stop of [...livePolls]) stop();
  const root = document.getElementById("tab-root");
  root.innerHTML = "";
  current = tab;
  renderNav(tab.id);
  document.title = `LLM Router · ${tab.label}`;
  try {
    tab.mount(root, ctx);
  } catch (e) {
    root.innerHTML = `<p class="error">${fmt.escHtml(String(e))}</p>`;
    console.error(e);
  }
}

window.addEventListener("hashchange", () => {
  // Same tab, different query (#catalog?node=x while on Catalog): re-mount so
  // the tab re-reads its state from the hash. Tabs write their own state
  // with history.replaceState, which fires no hashchange, so no loop.
  const { id } = parseHash();
  switchTo(id, current !== null && current.id === id);
});

// Header buttons open the dialogs from every tab.
document.getElementById("btnTokens").addEventListener("click", () => dialogs.openTokens());
document.getElementById("btnChat").addEventListener("click", () => dialogs.openChat());

// Header strip: until /api/overview lands (its own leaf) derive the strip
// from /api/models so the shell is useful on day one.
async function refreshStrip() {
  const el = document.getElementById("strip");
  try {
    const d = await api.get("/api/models");
    const models = d.models || [];
    const enabled = models.filter((m) => m.enabled !== false);
    const up = enabled.filter((m) => (m.availability ? m.availability === "available" : true)).length;
    const warming = enabled.filter((m) => m.availability === "warming").length;
    const absent = enabled.filter((m) => m.availability === "absent").length;
    const discovered = models.filter((m) => m.discovered).length;
    const roles = d.roles || [];
    const bound = roles.filter((r) => r.available).length;
    const tile = (v, label, cls = "") =>
      `<div class="stat"><div class="stat-value ${cls}">${v}</div><div class="stat-label">${label}</div></div>`;
    let modelsV = `${up}<span style="color:var(--text-dim);font-size:0.8rem">/${enabled.length}</span>`;
    if (warming) modelsV += ` <span style="color:var(--yellow);font-size:0.8rem">${warming} warming</span>`;
    if (absent) modelsV += ` <span style="color:var(--red);font-size:0.8rem">${absent} absent</span>`;
    el.innerHTML =
      tile(modelsV, "models up") +
      tile(
        `${bound}<span style="color:var(--text-dim);font-size:0.8rem">/${roles.length}</span>`,
        "roles bound",
        bound < roles.length ? "" : "",
      ) +
      (discovered ? tile(discovered, "discovered") : "") +
      tile(d.node_count ?? "?", "nodes");
  } catch (e) {
    el.innerHTML = `<div class="stat"><div class="stat-value" style="color:var(--red)">—</div><div class="stat-label">${fmt.escHtml(String(e))}</div></div>`;
  }
}
refreshStrip();
setInterval(() => {
  if (document.visibilityState === "visible") refreshStrip();
}, 30000);

switchTo(parseHash().id);
