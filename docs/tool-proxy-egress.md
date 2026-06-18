# Tool-proxy per-request VPN egress selection (`X-Egress`)

**Status:** design sketch (2026-06-17). Not implemented yet. Premise validated on live infra (see below).

## Goal

Let a client pick the VPN exit **per request** — US, EU (GDPR), a specific
country/city, or random — for the tool proxy's outbound web tools
(`web_search`, `fetch_url`, `tavily`). Deterministic and client-controlled
(e.g. "this research script always exits Sweden"), **not** model-chosen.
Inference and the auto-router embedder stay on the LAN (never through the VPN),
as today.

## Validated premise — one tunnel, many exits (no new tunnels)

Mullvad runs a SOCKS5 proxy on **every** relay, reachable from **inside any one
active WireGuard tunnel**; traffic sent through relay X's SOCKS5 exits from X's
IP. So the **existing** `svc-sys-research-vpn` tunnel (default exit Zürich) can
reach any country per request just by changing the SOCKS5 *target* — no new
handshakes, no pool of tunnels, no netns work.

Tested 2026-06-17 from inside the existing container's netns (`nsenter`):

| SOCKS target | Exit seen at am.i.mullvad.net |
|---|---|
| _(none — default tunnel)_ | Switzerland, Zürich |
| `us-atl-wg-socks5-001.relays.mullvad.net:1080` | **USA, Atlanta** (45.134.140.131) |
| `se-got-wg-socks5-001.relays.mullvad.net:1080` | **Sweden, Gothenburg** (185.213.154.217) |
| `ch-zrh-wg-socks5-001.relays.mullvad.net:1080` | Switzerland, Zürich |

- `socks_name`s resolve via **public DNS** to in-tunnel `10.124.x` IPs (so the
  tool proxy can resolve them host-side and hand the IP to microsocks).
- `AllowedIPs` already covers `10.124.0.0/16` — **no VPN-container change needed.**
- No auth on the relay SOCKS proxies (gated by being in the tunnel).
- Caveat: the intra-Mullvad relay hop isn't re-encrypted with our keys (fine for
  research traffic). The single base tunnel is still a SPOF (out of scope here;
  a 2nd tunnel later).

## Relay catalogue

`GET https://api.mullvad.net/www/relays/wireguard/` → 556 relays, each with
`socks_name`, `socks_port` (1080), `country_code`, `city_code`, `hostname`,
`active`. Filter `active`, index by country/city. Cache with a TTL (~1h) + keep
the last good copy as fallback.

## Plumbing — how the client selects

Two mechanisms; the header is primary.

### 1. `X-Egress` header (primary)
Client sends `X-Egress: se`. **Already passes through end-to-end with no router
change** — the router's `ReverseProxy.Rewrite` clones the inbound request and
only overrides `Authorization` (it passes `""` to the tool-proxy hop), so custom
headers survive client → router → tool proxy. The tool proxy reads it.

Spec grammar (mirrors `~/.local/bin/vpn`):
| Spec | Meaning |
|---|---|
| `se` | random active relay in country `se` |
| `se-got` | random active relay in city `se-got` |
| `se-got-wg-001` / `<socks_name>` | that exact relay |
| `any` / `random` | random active relay anywhere |
| `default` / _(absent)_ | current behaviour — microsocks default exit, no relay hop |

Trivial in a script (`-H 'X-Egress: se'`), works with the OpenAI SDK
(`default_headers`), model-agnostic.

### 2. Model aliases (convenience, for model-only clients e.g. OpenCode)
`research-eu`, `research-us`, `research-random` → same backend model + a fixed
egress. OpenCode picks a *model*, not a header. **Needs a small router change**:
the router resolves aliases to a model_id before forwarding, which loses the
suffix — so the router should recognise a configured set of egress-suffixed
aliases, map them to `base-model + X-Egress`, and inject the header. Defer to a
follow-up; the header covers scripts day one.

**Precedence:** explicit `X-Egress` header > alias-derived egress > default.

## Go implementation seams

New package `internal/toolproxy/egress`:
- `Catalogue`: fetches the relay list (configurable URL), caches (TTL) + last-good
  fallback, indexes active relays by `country_code` / `city_code` / `socks_name`.
- `Resolve(spec) (relay, error)`: applies the grammar; random pick within a match;
  returns a sentinel for `default`/empty.
- `DialerFor(spec) (proxy.Dialer, error)`:
  - `default`/empty → the **base microsocks dialer** (current global behaviour).
  - relay spec → **nested SOCKS**:
    `proxy.SOCKS5("tcp", relay.socks_name+":1080", nil, microsocksDialer)`
    (outer = microsocks enters the tunnel; inner = relay SOCKS picks the exit).
  - Cache dialers per resolved relay.

Wiring:
- `tools/http_client.go` today builds one client with one SOCKS dialer. Change to
  resolve a per-request client from the egress `Catalogue` (cache `http.Client`s
  keyed by relay so connection pools are reused).
- Thread the spec request-scoped: `handleChat` reads `X-Egress` → `context` →
  tool execution selects the client. The web tools take the client from context
  rather than a single injected `toolClient`.
- New flags: `--mullvad-relays-url` (default the API), `--egress-cache-ttl`
  (default 1h), `--egress-default` (default empty = current exit), and an
  `--egress-enabled` kill-switch.

## Resilience / observability
- Spec matches no active relay → log + fall back to default exit; set a response
  header `X-Egress-Exit: <relay|default>` so callers see what actually happened.
- Chosen relay connection fails → retry another relay matching the spec (a few
  attempts) before falling back. (Relay-level failover; base tunnel still SPOF.)
- Log the resolved exit per request (country/city + relay) for the dashboard.

## Phasing
- **E1 (core):** `egress.Catalogue` + nested `DialerFor` + `X-Egress` header in
  the tool proxy (country/city/exact/any/default). Default behaviour unchanged.
  No router change. Unit tests for spec→relay matching + fallback.
- **E2:** model-alias support (router suffix→header translation), retry-on-dead-
  relay, `X-Egress-Exit` response header, metrics.
- **E3 (later):** 2nd base tunnel to kill the SPOF; per-spec dialer keep-alive.

## Open items
- Confirm the tool proxy can resolve `*.relays.mullvad.net` → `10.124.x` from the
  euclid **host** namespace (it ran inside the container's netns in the test; the
  proxy runs in the host ns but hands the IP to microsocks which is in-tunnel —
  resolve host-side, dial via microsocks). Quick to verify in E1.
- Decide which models honour egress (all tool_proxy models, or a tagged subset).
- Relay-list refresh cadence + handling of `owned`/DAITA filters if we ever want
  to restrict to owned infra.
