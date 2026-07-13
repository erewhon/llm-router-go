#!/usr/bin/env bash
# Deploy the Go router binary + systemd unit to euclid as the LiteLLM
# replacement. Hard cutover: stops litellm-proxy, starts llm-router-go on
# the same port (:4010). To roll back, see ROLLBACK below.
#
# Usage:
#   deploy/scripts/deploy-router.sh                # install (no service flip)
#   deploy/scripts/deploy-router.sh --cutover      # install + flip services
#
# ROLLBACK on euclid:
#   sudo systemctl stop llm-router-go
#   sudo systemctl disable llm-router-go        # so it stays off through reboot
#   sudo systemctl enable --now litellm-proxy   # restore old behavior
#
# Prereqs on euclid:
#   - `llm-router` system user exists (already created for the node agent).
#   - UFW allows inbound :4010 from the LAN (already in place for LiteLLM).
#   - models.yaml in sync (run `just sync` from the Python repo first).

set -euo pipefail

HOST="${HOST:-euclid.local}"
CUTOVER=false
for arg in "$@"; do
    case "$arg" in
        --cutover) CUTOVER=true ;;
        *) echo "unknown arg: $arg" >&2; exit 2 ;;
    esac
done

REPO_ROOT=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$REPO_ROOT"

ARCH=$(ssh "$HOST" 'uname -m')
case "$ARCH" in
    x86_64)  GOARCH=amd64 ;;
    aarch64) GOARCH=arm64 ;;
    *)
        echo "deploy: unsupported arch $ARCH on $HOST" >&2
        exit 1
        ;;
esac

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

echo "==> Building router ($GOARCH, version=$VERSION) for $HOST..."
# CGO_ENABLED=1 so the binary uses glibc → NSS → avahi for hostname
# resolution. Pure-Go DNS goes straight to 127.0.0.53 which isn't a
# working stub on euclid (systemd-resolved is active but doesn't bind
# the loopback stub there), so static builds can't resolve *.local.
CGO_ENABLED=1 GOOS=linux GOARCH=$GOARCH \
    go build -ldflags="-X main.version=$VERSION" \
    -o "bin/$GOARCH/router" \
    ./cmd/router

echo "==> Staging artifacts on $HOST:/tmp ..."
rsync -avz --info=name "bin/$GOARCH/router" \
    "$HOST:/tmp/llm-router-go"
rsync -avz --info=name "deploy/systemd/llm-router-go.service" \
    "$HOST:/tmp/llm-router-go.service"

echo "==> Installing on $HOST (will prompt for sudo)..."
ssh -t "$HOST" "
    set -e
    sudo install -m 755 /tmp/llm-router-go /usr/local/bin/llm-router-go
    sudo install -m 644 /tmp/llm-router-go.service /etc/systemd/system/llm-router-go.service
    sudo systemctl daemon-reload
    rm -f /tmp/llm-router-go /tmp/llm-router-go.service
    echo
    echo 'Installed:'
    /usr/local/bin/llm-router-go -version | sed 's/^/  version=/'
    echo
"

if [[ "$CUTOVER" == "true" ]]; then
    echo "==> Cutting over: retire litellm-proxy + litellm-dashboard, (re)start llm-router-go..."
    # The dashboard UI is now baked into the router binary (served on :4011 via
    # --dashboard), so the standalone Python litellm-dashboard is retired here.
    # `restart` (not just `enable --now`) guarantees an already-running router
    # picks up the freshly installed binary + unit.
    ssh -t "$HOST" "
        set -e
        sudo systemctl stop litellm-proxy || true
        sudo systemctl disable litellm-proxy || true
        sudo systemctl disable --now litellm-dashboard || true
        sudo systemctl enable llm-router-go
        sudo systemctl restart llm-router-go
        echo
        systemctl status llm-router-go --no-pager | head -12
    "
    echo
    echo 'Done. To roll back:'
    echo "  ssh $HOST sudo systemctl stop llm-router-go && \\"
    echo "  ssh $HOST sudo systemctl disable llm-router-go && \\"
    echo "  ssh $HOST sudo systemctl enable --now litellm-proxy"
else
    echo 'Installed but not started. To cut over:'
    echo "  $0 --cutover"
    echo 'Or manually:'
    echo "  ssh $HOST sudo systemctl stop litellm-proxy"
    echo "  ssh $HOST sudo systemctl disable litellm-proxy"
    echo "  ssh $HOST sudo systemctl enable --now llm-router-go"
fi
