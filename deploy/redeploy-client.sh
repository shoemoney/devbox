#!/usr/bin/env bash
# Build the devbox client (host arch — fleet macs are darwin/arm64) and push it to
# the fleet macs at /tmp/devbox. Run from anywhere in the repo.
set -euo pipefail
cd "$(dirname "$0")/.."

FLEET=(hueb reek wick amber)    # 192.168.1.3/.4/.5/.7

echo "🔨 building devbox client..."
go build -o /tmp/devbox ./cmd/devbox

for h in "${FLEET[@]}"; do
  if scp -q /tmp/devbox "${h}:/tmp/devbox" 2>/dev/null && ssh "$h" 'chmod +x /tmp/devbox'; then
    echo "✅ ${h}"
  else
    echo "⚠️  ${h} unreachable, skipped"
  fi
done
