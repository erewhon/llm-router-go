#!/usr/bin/env bash
# Materialize /etc/llm-router/tool-proxy.env on the tool-proxy host (euclid by
# default) from `ho secret`. The Go tool-proxy unit picks it up via
# EnvironmentFile; if it's missing the unit still starts (the auto-router's
# redirect 401s, but normal tool_proxy models work).
#
# Usage:
#   deploy/scripts/deploy-tool-proxy-secrets.sh
#   HOST=foo.local deploy/scripts/deploy-tool-proxy-secrets.sh
#
# Writes:
#   LITELLM_KEY=<router api key>   # the auto-router redirects to the Go router
#                                  # on :4010, which now requires a bearer.
# TAVILY_API_KEY is intentionally NOT written — tavily_search stays
# unregistered, matching the Python proxy. Set TAVILY_SECRET to enable it.
#
# The secret never appears in argv or shell history — only env vars (ho stdin)
# and a tmpfs file.

set -euo pipefail

HOST="${HOST:-euclid.local}"
# Same key the router accepts (a member of ROUTER_API_KEYS).
SECRET_NAME="${SECRET_NAME:-llm-router/api-key}"
# Optional: set TAVILY_SECRET=llm-router/tavily-key to register tavily_search.
TAVILY_SECRET="${TAVILY_SECRET:-}"
HO="${HO:-/home/erewhon/.local/bin/ho}"

if [[ ! -x "$HO" ]]; then
    echo "deploy-tool-proxy-secrets: ho not found at $HO" >&2
    exit 1
fi

WORK=$(mktemp -d /dev/shm/llmr-tp-secrets.XXXXXX)
chmod 700 "$WORK"
trap '[[ -f "$WORK/tool-proxy.env" ]] && shred -u "$WORK/tool-proxy.env" 2>/dev/null || true; rm -rf "$WORK"' EXIT

echo "==> Fetching $SECRET_NAME from ho..."
{
    printf 'LITELLM_KEY='
    "$HO" secret get "$SECRET_NAME"
    echo
    if [[ -n "$TAVILY_SECRET" ]]; then
        if tv=$("$HO" secret get "$TAVILY_SECRET" 2>/dev/null) && [[ -n "$tv" ]]; then
            printf 'TAVILY_API_KEY=%s\n' "$tv"
        else
            echo "deploy-tool-proxy-secrets: WARN $TAVILY_SECRET did not resolve; tavily stays off" >&2
        fi
    fi
} > "$WORK/tool-proxy.env"
chmod 600 "$WORK/tool-proxy.env"

echo "==> Staging on $HOST:/tmp/tool-proxy.env.new..."
scp -q "$WORK/tool-proxy.env" "$HOST:/tmp/tool-proxy.env.new"

echo "==> Installing on $HOST..."
ssh -t "$HOST" '
    set -e
    sudo mkdir -p /etc/llm-router
    sudo install -m 0640 -o root -g llm-router /tmp/tool-proxy.env.new /etc/llm-router/tool-proxy.env
    rm -f /tmp/tool-proxy.env.new
    ls -la /etc/llm-router/tool-proxy.env
'

echo
echo "Done. Restart the tool proxy to pick up new keys:"
echo "  ssh $HOST sudo systemctl restart llm-router-go-tool-proxy"
