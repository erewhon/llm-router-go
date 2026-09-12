// Formatting and classification helpers shared by every tab. Moved verbatim
// from the legacy single-file dashboard (2026-09-12); behaviour unchanged.

export function escHtml(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

export function fmtAgoIso(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  if (s < 172800) return `${(s / 3600).toFixed(1)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

// fmtAgo for a seconds count (the inventory's age_s and similar).
export function fmtAgoS(s) {
  if (s == null) return "never";
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  return `${(s / 3600).toFixed(1)}h ago`;
}

export function sparklineSvg(vals, color) {
  const w = 48,
    h = 16;
  // Always emit a fixed-size box: an empty placeholder until two samples
  // exist, so the row doesn't reflow (layout shift) when history arrives.
  if (!vals || vals.length < 2) {
    return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" style="vertical-align:middle"></svg>`;
  }
  const coords = vals
    .map((v, i) => {
      const x = (i / (vals.length - 1)) * w;
      const y = h - (v / 100) * h;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" style="vertical-align:middle">` +
    `<polyline points="${coords}" fill="none" stroke="${color}" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>`
  );
}

export function copyText(text, btn) {
  navigator.clipboard.writeText(text).then(() => {
    btn.textContent = "copied";
    btn.classList.add("copied");
    setTimeout(() => {
      btn.textContent = "copy";
      btn.classList.remove("copied");
    }, 1500);
  });
}

export function vramBarColor(pct) {
  if (pct >= 90) return "red";
  if (pct >= 70) return "yellow";
  return "green";
}

export function fmtUptime(s) {
  if (s == null) return "?";
  s = Math.floor(s);
  const d = Math.floor(s / 86400),
    h = Math.floor((s % 86400) / 3600),
    m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

// fmtCtx renders a token window the way it is spoken: 131072 -> "131k".
export function fmtCtx(n) {
  if (!n) return "";
  return n >= 1000 ? `${Math.round(n / 1024)}k` : String(n);
}

// Quantization badge. Evidence order: explicit tags (author-supplied, most
// reliable) -> gguf_file name -> hf_repo string. Returns '' when nothing
// matches: an unknown quant must render as nothing, never as a guess.
const QUANT_PATTERNS = [
  // Longest / most specific first: 'nvfp4' and 'mxfp4' both contain 'fp4',
  // and 'q4_k_xl' contains 'q4_k'.
  ["nvfp4", "NVFP4"],
  ["mxfp4", "MXFP4"],
  ["fp8", "FP8"],
  ["bf16", "BF16"],
  ["fp16", "FP16"],
  ["q8_0", "Q8"],
  ["-q8", "Q8"],
  ["q6_k_xl", "Q6_K_XL"],
  ["q6_k", "Q6"],
  ["-q6", "Q6"],
  ["q5_k_xl", "Q5_K_XL"],
  ["q5_k_m", "Q5_K_M"],
  ["q5_k", "Q5"],
  ["-q5", "Q5"],
  ["q4_k_xl", "Q4_K_XL"],
  ["q4_k_m", "Q4_K_M"],
  ["q4_k_s", "Q4_K_S"],
  ["gptq-int4", "GPTQ-INT4"],
  ["q4_0", "Q4"],
  ["-q4", "Q4"],
  ["q3_k", "Q3"],
  ["-q3", "Q3"],
  ["awq", "AWQ"],
  ["gptq", "GPTQ"],
];
export function quantLabel(m) {
  const gguf = (m.gguf_file || "").toLowerCase();
  const repo = (m.hf_repo || "").split("#")[0].toLowerCase();
  const hay = [(m.tags || []).join(" "), gguf, repo].join(" ").toLowerCase();
  for (const [needle, label] of QUANT_PATTERNS) {
    if (hay.includes(needle)) return label;
  }
  // Known to be a GGUF build but the specific quant isn't discoverable.
  if (gguf || repo.endsWith("-gguf")) return "GGUF";
  return "";
}

// Real serving engine. models.yaml `backend` selects which health-probe driver
// the node agent uses, NOT the engine: Atlas and llama.cpp both declare
// backend: vllm because they speak the same OpenAI /v1 + Prometheus /metrics
// shape. Prefer harder evidence before falling back to that field.
export function engineLabel(m) {
  const tags = m.tags || [];
  if (tags.includes("atlas")) return "atlas";
  if (m.gguf_file || (m.hf_repo || "").toLowerCase().endsWith("-gguf")) return "llama.cpp";
  return m.backend;
}

const ROUTER_IDS = ["auto", "auto-free", "auto-full", "coder-resilient"];

export function isRouterModel(m) {
  return ROUTER_IDS.includes(m.id);
}

export function modelType(m) {
  const tags = m.tags || [];
  if (isRouterModel(m)) return { label: "router", badge: "badge-router" };
  const nodes = m.nodes || [];
  if (tags.includes("embedding") || tags.includes("reranker"))
    return {
      label: m.backend === "external" && nodes.includes("euclid") ? "ovms" : m.backend,
      badge: "badge-embed",
    };
  if (tags.includes("stt"))
    return {
      label: m.backend === "external" && nodes.includes("euclid") ? "ovms" : m.backend,
      badge: "badge-stt",
    };
  if (tags.includes("tts")) return { label: "tts", badge: "badge-tts" };
  if (tags.includes("image_gen") || tags.includes("image_edit")) return { label: "image", badge: "badge-image" };
  if (tags.includes("music_gen")) return { label: "music", badge: "badge-music" };
  if (tags.includes("gui_agent")) return { label: "gui-agent", badge: "badge-router" };
  return { label: engineLabel(m), badge: "badge-backend" };
}

// A "cloud" model runs on somebody else's hardware: external backend AND no
// node. Node-pinned externals (flux, orpheus, ui-tars, the OpenArc embedder)
// are deliberately NOT cloud — they're external only in the sense that
// something other than the agent starts them, and they die with their host.
// The auto-* routers are excluded too: they're our own routing stubs.
export function isCloudModel(m) {
  return m.backend === "external" && (!m.nodes || m.nodes.length === 0) && !isRouterModel(m);
}

// Non-chat models (embeddings/rerank/stt/tts/image/music) can't take a chat
// message; the Chat dialog's picker filters with this.
export function chatCapable(m) {
  const tags = m.tags || [];
  if (m.enabled === false) return false;
  for (const t of ["embedding", "reranker", "stt", "tts", "image_gen", "image_edit", "music_gen"]) {
    if (tags.includes(t)) return false;
  }
  return true;
}
