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
#   1. Reads the secret from `ho secret get llm-router/api-key`
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
