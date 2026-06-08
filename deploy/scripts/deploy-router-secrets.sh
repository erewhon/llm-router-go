#!/usr/bin/env bash
# Materialize /etc/llm-router/proxy.env on the router host (euclid by
# default) from `ho secret`. The router's systemd unit picks the file
# up via EnvironmentFile; if it's missing the unit still starts (auth
# disabled, with a WARN log).
#
# Usage:
#   deploy/scripts/deploy-router-secrets.sh                # to euclid.local
#   HOST=foo.local deploy/scripts/deploy-router-secrets.sh
#
# Re-run whenever the key rotates. The script:
#   1. Reads secrets via `ho secret get` (router API key, OpenCode Zen key,
#      and the reqlog Postgres password → assembled into ROUTER_PG_DSN)
#   2. Writes a tmpfs file (chmod 600)
#   3. scp's it to the host and `sudo install`s at /etc/llm-router/proxy.env
#      (mode 0640, owner root:llm-router)
#   4. Shreds the local copy
#
# The secret never appears in argv or shell history — only in env vars
# (ho stdin) and tmpfs.

set -euo pipefail

HOST="${HOST:-euclid.local}"
SECRET_NAME="${SECRET_NAME:-llm-router/api-key}"
# Upstream OpenCode Zen API key — the router resolves the external Zen models'
# `api_key: OPENCODE_ZEN_API_KEY` (env-var name) against this. Optional: if the
# ref doesn't resolve, the line is omitted and Zen models 401.
ZEN_SECRET_NAME="${ZEN_SECRET_NAME:-llm-router/opencode-zen-api-key}"
# Request-log Postgres. The router reads a full DSN from ROUTER_PG_DSN (env),
# but only the password is a secret (in `ho`); host/port/db are connection
# config and live here. Optional: if the password ref doesn't resolve, the
# ROUTER_PG_DSN line is omitted and the router falls back to NopSink.
REQLOG_PG_SECRET="${REQLOG_PG_SECRET:-llm-router/reqlog-pg-password}"
REQLOG_PG_USER="${REQLOG_PG_USER:-router}"
REQLOG_PG_HOST="${REQLOG_PG_HOST:-127.0.0.1}"
REQLOG_PG_PORT="${REQLOG_PG_PORT:-5433}"
REQLOG_PG_DB="${REQLOG_PG_DB:-router}"
HO="${HO:-/home/erewhon/.local/bin/ho}"

if [[ ! -x "$HO" ]]; then
    echo "deploy-router-secrets: ho not found at $HO" >&2
    exit 1
fi

# tmpfs scratch (RAM-only) so the secret never hits disk locally.
WORK=$(mktemp -d /dev/shm/llmr-secrets.XXXXXX)
chmod 700 "$WORK"
trap '[[ -f "$WORK/proxy.env" ]] && shred -u "$WORK/proxy.env" 2>/dev/null || true; rm -rf "$WORK"' EXIT

echo "==> Fetching $SECRET_NAME from ho..."
{
    printf 'ROUTER_API_KEYS='
    "$HO" secret get "$SECRET_NAME"
    echo
    printf 'WELLKNOWN_API_KEY='
    "$HO" secret get "$SECRET_NAME"
    echo
    if zen=$("$HO" secret get "$ZEN_SECRET_NAME" 2>/dev/null) && [[ -n "$zen" ]]; then
        printf 'OPENCODE_ZEN_API_KEY=%s\n' "$zen"
    else
        echo "deploy-router-secrets: WARN $ZEN_SECRET_NAME did not resolve; Zen models will 401" >&2
    fi
    # ROUTER_PG_DSN — only the password comes from `ho`; the rest is config.
    # printf is a shell builtin, so the password never appears in argv/ps.
    if pgpw=$("$HO" secret get "$REQLOG_PG_SECRET" 2>/dev/null) && [[ -n "$pgpw" ]]; then
        printf 'ROUTER_PG_DSN=postgres://%s:%s@%s:%s/%s?sslmode=disable\n' \
            "$REQLOG_PG_USER" "$pgpw" "$REQLOG_PG_HOST" "$REQLOG_PG_PORT" "$REQLOG_PG_DB"
    else
        echo "deploy-router-secrets: WARN $REQLOG_PG_SECRET did not resolve; reqlog disabled (NopSink)" >&2
    fi
} > "$WORK/proxy.env"
chmod 600 "$WORK/proxy.env"

echo "==> Staging on $HOST:/tmp/proxy.env.new..."
scp -q "$WORK/proxy.env" "$HOST:/tmp/proxy.env.new"

echo "==> Installing on $HOST..."
ssh -t "$HOST" '
    set -e
    sudo mkdir -p /etc/llm-router
    sudo install -m 0640 -o root -g llm-router /tmp/proxy.env.new /etc/llm-router/proxy.env
    rm -f /tmp/proxy.env.new
    ls -la /etc/llm-router/proxy.env
'

echo
echo "Done. Restart the router to pick up new keys:"
echo "  ssh $HOST sudo systemctl restart llm-router-go"
