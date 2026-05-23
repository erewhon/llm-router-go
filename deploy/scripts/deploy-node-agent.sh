#!/usr/bin/env bash
# Cross-compile node-agent for the target host's architecture, rsync the
# binary and systemd unit, and install. Does not enable or start the
# service — the operator does that after verifying with `systemctl
# status llm-router-go-agent`.
#
# Usage: deploy/scripts/deploy-node-agent.sh <hostname> [--start]
#
# --start: after install, run `systemctl start llm-router-go-agent`
#          on the target. Does NOT enable on boot.
#
# Prerequisites on the target host:
#   - `llm-router` system user must exist (created by the Python
#     agent's setup).
#   - /home/erewhon/Projects/erewhon/llm-router/models.yaml must exist
#     (kept in sync by the Python repo's deploy-nodes.sh).
#   - UFW (if active) must allow inbound :8101 from the LAN. Example:
#       sudo ufw allow from 192.168.42.0/24 to any port 8101 \
#           comment "LLM Router Go node agent"
#     The Python agent's :8100 has a matching rule today.

set -euo pipefail

HOST=${1:-}
START=${2:-}

if [[ -z "$HOST" ]]; then
    echo "usage: $0 <hostname> [--start]" >&2
    exit 2
fi

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

echo "==> Building node-agent ($GOARCH, version=$VERSION) for $HOST..."
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH \
    go build -ldflags="-X main.version=$VERSION" \
    -o "bin/$GOARCH/node-agent" \
    ./cmd/node-agent

echo "==> Staging artifacts on $HOST:/tmp ..."
rsync -avz --info=name "bin/$GOARCH/node-agent" \
    "$HOST:/tmp/llm-router-go-agent"
rsync -avz --info=name "deploy/systemd/llm-router-go-agent.service" \
    "$HOST:/tmp/llm-router-go-agent.service"

echo "==> Installing on $HOST (will prompt for sudo)..."
ssh -t "$HOST" "
    set -e
    sudo install -m 755 /tmp/llm-router-go-agent /usr/local/bin/llm-router-go-agent
    sudo install -m 644 /tmp/llm-router-go-agent.service /etc/systemd/system/llm-router-go-agent.service
    sudo systemctl daemon-reload
    rm -f /tmp/llm-router-go-agent /tmp/llm-router-go-agent.service
    echo
    echo 'Installed:'
    /usr/local/bin/llm-router-go-agent -version | sed 's/^/  version=/'
    echo
"

if [[ "$START" == "--start" ]]; then
    echo "==> Starting llm-router-go-agent on $HOST..."
    ssh -t "$HOST" 'sudo systemctl restart llm-router-go-agent && systemctl status llm-router-go-agent --no-pager | head -15'
else
    echo "Not started. To start manually:"
    echo "    ssh $HOST sudo systemctl start llm-router-go-agent"
    echo "To enable on boot:"
    echo "    ssh $HOST sudo systemctl enable --now llm-router-go-agent"
fi
