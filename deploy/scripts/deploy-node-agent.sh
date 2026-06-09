#!/usr/bin/env bash
# Cross-compile node-agent for the target host's architecture, rsync the
# binary and systemd unit, and install. Does not enable or start the
# service — the operator does that after verifying with `systemctl
# status llm-router-go-agent`.
#
# Usage: deploy/scripts/deploy-node-agent.sh <hostname> [--cutover]
#
# (no flag): build + install the binary and :8100 unit, but do NOT start
#            it — the Python llm-router-agent keeps :8100. Stages a node
#            before cutting over.
# --cutover: after install, stop+disable the Python llm-router-agent and
#            enable+start the Go agent on :8100 (on boot too). Reversible —
#            the Python unit stays installed.
#
# Prerequisites on the target host:
#   - `llm-router` system user must exist (created by the Python
#     agent's setup).
#   - /home/erewhon/Projects/erewhon/llm-router/models.yaml must exist
#     (kept in sync by the Python repo's deploy-nodes.sh).
#   - The Go agent reuses the Python agent's existing :8100 UFW rule;
#     no new firewall rule is needed at cutover.

set -euo pipefail

HOST=${1:-}
MODE=${2:-}

if [[ -z "$HOST" ]]; then
    echo "usage: $0 <hostname> [--cutover]" >&2
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

if [[ "$MODE" == "--cutover" ]]; then
    echo "==> Cutting over $HOST to the Go node agent (:8100), retiring the Python agent..."
    ssh -t "$HOST" '
        set -e
        sudo systemctl stop llm-router-agent 2>/dev/null || true
        sudo systemctl disable llm-router-agent 2>/dev/null || true
        sudo systemctl daemon-reload
        sudo systemctl enable llm-router-go-agent
        sudo systemctl restart llm-router-go-agent
        sleep 2
        echo "  go-agent=$(systemctl is-active llm-router-go-agent) python-agent=$(systemctl is-active llm-router-agent 2>/dev/null || echo gone)"
        printf "  :8100 health -> "; curl -sS -m5 http://localhost:8100/health | head -c 160; echo
    '
    echo
    echo "Cut over. To roll back on $HOST:"
    echo "    ssh $HOST sudo systemctl stop llm-router-go-agent"
    echo "    ssh $HOST sudo systemctl disable llm-router-go-agent"
    echo "    ssh $HOST sudo systemctl enable --now llm-router-agent"
elif [[ -n "$MODE" ]]; then
    echo "unknown mode: $MODE (expected --cutover or nothing)" >&2
    exit 2
else
    echo "Installed but not started (Python llm-router-agent still owns :8100)."
    echo "To cut over (retire Python, Go owns :8100, enable on boot):"
    echo "    $0 $HOST --cutover"
fi
