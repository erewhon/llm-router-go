#!/usr/bin/env bash
# Deploy the Go tool-proxy binary + systemd unit to euclid. It runs on :5393,
# parallel to the legacy Python proxy on :5392; the routers point at it via
# $ROUTER_TOOL_PROXY_URL (see deploy-router-secrets.sh / the router unit).
#
# Usage:
#   deploy/scripts/deploy-tool-proxy.sh                 # install + (re)start
#   HOST=foo.local deploy/scripts/deploy-tool-proxy.sh
#
# CUTOVER (point the routers at the Go proxy) is separate — append to each
# router host's /etc/llm-router/proxy.env and restart it:
#   ROUTER_TOOL_PROXY_URL=http://192.168.42.240:5393/v1
# ROLLBACK: remove that line on the routers and restart them (-> default :5392).
#
# Secrets: LITELLM_KEY (a valid ROUTER_API_KEY, used by the auto-router's
# redirect to the Go router) is materialized into /etc/llm-router/tool-proxy.env
# by deploy-tool-proxy-secrets.sh — run that first/whenever the key rotates.
#
# Prereqs on euclid: the `llm-router` system user, models.yaml in sync (run
# `just sync` from the Python repo), and the SOCKS5 VPN reachable at
# 192.168.42.219:1080.

set -euo pipefail

HOST="${HOST:-euclid.local}"

REPO_ROOT=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$REPO_ROOT"

ARCH=$(ssh "$HOST" 'uname -m')
case "$ARCH" in
    x86_64)  GOARCH=amd64 ;;
    aarch64) GOARCH=arm64 ;;
    *) echo "deploy: unsupported arch $ARCH on $HOST" >&2; exit 1 ;;
esac

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

echo "==> Building tool-proxy ($GOARCH, version=$VERSION) for $HOST..."
# CGO_ENABLED=1 so the binary uses glibc -> NSS -> avahi for *.local
# resolution (same rationale as deploy-router.sh).
CGO_ENABLED=1 GOOS=linux GOARCH=$GOARCH \
    go build -ldflags="-X main.version=$VERSION" \
    -o "bin/$GOARCH/tool-proxy" \
    ./cmd/tool-proxy

echo "==> Staging artifacts on $HOST:/tmp ..."
rsync -avz --info=name "bin/$GOARCH/tool-proxy" \
    "$HOST:/tmp/llm-router-go-tool-proxy"
rsync -avz --info=name "deploy/systemd/llm-router-go-tool-proxy.service" \
    "$HOST:/tmp/llm-router-go-tool-proxy.service"

echo "==> Installing + (re)starting on $HOST (will prompt for sudo)..."
ssh -t "$HOST" "
    set -e
    sudo install -m 755 /tmp/llm-router-go-tool-proxy /usr/local/bin/llm-router-go-tool-proxy
    sudo install -m 644 /tmp/llm-router-go-tool-proxy.service /etc/systemd/system/llm-router-go-tool-proxy.service
    sudo systemctl daemon-reload
    sudo systemctl enable --now llm-router-go-tool-proxy
    sudo systemctl restart llm-router-go-tool-proxy
    rm -f /tmp/llm-router-go-tool-proxy /tmp/llm-router-go-tool-proxy.service
    echo
    echo 'Installed:'
    /usr/local/bin/llm-router-go-tool-proxy -version | sed 's/^/  version=/'
    sleep 1
    systemctl is-active llm-router-go-tool-proxy | sed 's/^/  active=/'
"

echo
echo 'Done. Health check:'
echo "  ssh $HOST curl -s http://127.0.0.1:5393/health"
echo 'To cut the routers over (if not already): on each router host append'
echo '  ROUTER_TOOL_PROXY_URL=http://192.168.42.240:5393/v1'
echo 'to /etc/llm-router/proxy.env and: sudo systemctl restart llm-router-go'
