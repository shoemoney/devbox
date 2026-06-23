#!/usr/bin/env bash
# dashboard-fleet-verify.sh — prove the hub's live dashboard flow stream works on
# real hardware: a device joining + pushing produces live "join" and "push" events
# on the dashboard's /api/events SSE stream. Uses an ISOLATED hub (never the live
# .10). One host is enough (the SSE stream is the hub's, the device is local to it).
#
# Usage: scripts/dashboard-fleet-verify.sh [hubPi]   (default 192.168.1.13, arm64)
set -euo pipefail

HUB_PI="${1:-192.168.1.13}"
KEY="${PI_KEY:-$HOME/.ssh/pi}"
PORT="${PORT:-8288}"
DASH="${DASH:-8299}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

echo "🔧 build linux/arm64 binaries"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$TMP/devbox" ./cmd/devbox
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$TMP/devbox-hub" ./cmd/devbox-hub
scp -q -i "$KEY" "$TMP/devbox" "$TMP/devbox-hub" "shoemoney@$HUB_PI:/tmp/"

echo "📡 $HUB_PI: start hub --dashboard, capture SSE, then join+publish (= join+push flows)"
RESULT=$(ssh -o ConnectTimeout=8 -i "$KEY" "shoemoney@$HUB_PI" bash <<EOF 2>/dev/null
set -e
pkill -9 -f devbox-hub 2>/dev/null || true
rm -rf /tmp/dhub /tmp/dA /tmp/dwork; mkdir -p /tmp/dA /tmp/dwork
echo "live flow test" > /tmp/dwork/file.txt
nohup /tmp/devbox-hub serve --data /tmp/dhub --listen $HUB_PI:$PORT --dashboard --dashboard-addr $HUB_PI:$DASH >/tmp/dhub.log 2>&1 &
sleep 2
# start capturing the live flow stream BEFORE any activity
( curl -sN http://$HUB_PI:$DASH/api/events > /tmp/sse.log 2>/dev/null & echo \$! > /tmp/sse.pid )
sleep 1
export XDG_CONFIG_HOME=/tmp/dA
TOK=\$(/tmp/devbox-hub token --data /tmp/dhub)
/tmp/devbox join http://$HUB_PI:$PORT "\$TOK" >/dev/null   # -> "join" flow event
/tmp/devbox publish /tmp/dwork team >/dev/null              # -> "push" flow event
sleep 2
kill \$(cat /tmp/sse.pid) 2>/dev/null || true
echo "--- captured flow events ---"
cat /tmp/sse.log
echo "--- /api/state totals ---"
curl -s http://$HUB_PI:$DASH/api/state | tr ',' '\n' | grep -E 'devices|shares|version' | head
pkill -9 -f devbox-hub 2>/dev/null || true
rm -rf /tmp/devbox /tmp/devbox-hub /tmp/dhub /tmp/dA /tmp/dwork /tmp/sse.* /tmp/dhub.log
EOF
)
echo "$RESULT"

echo "🔎 verdict"
echo "$RESULT" | grep -q '"type":"join"' || { echo "🛑 no join flow event on the SSE stream"; exit 1; }
echo "$RESULT" | grep -q '"type":"push"' || { echo "🛑 no push flow event on the SSE stream"; exit 1; }
echo "🎉 dashboard fleet-verified: live join + push flow events stream over SSE on real hardware."
