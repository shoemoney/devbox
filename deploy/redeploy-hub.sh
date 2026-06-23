#!/usr/bin/env bash
# Cross-compile the devbox hub for the NAS (linux/amd64, no CGO — pure-Go sqlite),
# ship it, and restart the systemd service. Run from anywhere in the repo.
set -euo pipefail
cd "$(dirname "$0")/.."

HUB=nas                         # ssh alias for 192.168.1.10
DEST=/mnt/tank/apps/devbox
URL=http://192.168.1.10:8088

echo "🔨 building linux/amd64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/devbox-hub-linux ./cmd/devbox-hub

echo "🚚 shipping to ${HUB}:${DEST}..."
scp -q /tmp/devbox-hub-linux "${HUB}:${DEST}/devbox-hub.new"
ssh "$HUB" "mv ${DEST}/devbox-hub.new ${DEST}/devbox-hub && chmod +x ${DEST}/devbox-hub && sudo systemctl restart devbox-hub && systemctl is-active devbox-hub"

echo "✅ redeployed. hub metrics:"
curl --retry 10 --retry-connrefused --retry-delay 1 --max-time 15 -sf "${URL}/metrics" | grep '^devbox_' || echo "⚠️  hub not answering yet"
