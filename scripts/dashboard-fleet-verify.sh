#!/usr/bin/env bash
# dashboard-fleet-verify.sh — prove the hub's live dashboard flow stream works on
# real hardware: a device exercising the hub produces live join · push · pull ·
# conflict · gc events on /api/events (SSE), and /api/state carries the server-side
# history window. Uses an ISOLATED hub (never the live .10).
#
#   scripts/dashboard-fleet-verify.sh            # remote: build arm64, run on a Pi
#   LOCAL=1 scripts/dashboard-fleet-verify.sh    # local: native build on 127.0.0.1 (no Pi)
#
# Knobs: HUB_PI (default 192.168.1.13) · PI_KEY (~/.ssh/pi) · PORT/DASH ports.
set -euo pipefail

LOCAL="${LOCAL:-0}"
HUB_PI="${1:-192.168.1.13}"
KEY="${PI_KEY:-$HOME/.ssh/pi}"
PORT="${PORT:-8288}"
DASH="${DASH:-8299}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# The test body runs ON the hub host (localhost or the Pi). It starts an isolated
# hub with the dashboard + a fast in-process GC, captures the SSE flow stream, then
# drives every event type: join + push (CLI), pull + conflict (raw bearer), gc (timer).
gen_body() {  # args: HOST BIN
  cat <<BODY
set -e
HUB=$1; BIN=$2
pkill -9 -f devbox-hub 2>/dev/null || true
rm -rf /tmp/dhub /tmp/dA /tmp/dwork; mkdir -p /tmp/dA /tmp/dwork
echo "live flow test" > /tmp/dwork/file.txt
nohup \$BIN/devbox-hub serve --data /tmp/dhub --listen \$HUB:$PORT --dashboard --dashboard-addr \$HUB:$DASH --gc-every 2s --gc-keep 1 >/tmp/dhub.log 2>&1 &
sleep 2
( curl -sN http://\$HUB:$DASH/api/events > /tmp/sse.log 2>/dev/null & echo \$! > /tmp/sse.pid )
sleep 1
export XDG_CONFIG_HOME=/tmp/dA
TOK=\$(\$BIN/devbox-hub token --data /tmp/dhub)
\$BIN/devbox join http://\$HUB:$PORT "\$TOK" >/dev/null     # -> join
\$BIN/devbox publish /tmp/dwork team >/dev/null            # -> push
BEARER=\$(grep '^bearer' /tmp/dA/devbox/daemon.toml | cut -d'"' -f2)
curl -s -H "Authorization: Bearer \$BEARER" "http://\$HUB:$PORT/v1/head?share=team" >/dev/null   # -> pull
curl -s -H "Authorization: Bearer \$BEARER" -H 'Content-Type: application/json' \
  -d '{"share":"team","parent":"'\$(printf '0%.0s' {1..64})'","manifest_hash":"'\$(printf '1%.0s' {1..64})'"}' \
  "http://\$HUB:$PORT/v1/push" >/dev/null                  # -> conflict (stale parent)
sleep 3                                                     # -> gc (2s timer fires)
kill \$(cat /tmp/sse.pid) 2>/dev/null || true
echo "--- SSE flow events ---"; cat /tmp/sse.log
echo "--- /api/state history ---"; curl -s http://\$HUB:$DASH/api/state | grep -o '"history":\[[^]]*\]' | head -c 200; echo
pkill -9 -f devbox-hub 2>/dev/null || true
rm -rf /tmp/devbox /tmp/devbox-hub /tmp/dhub /tmp/dA /tmp/dwork /tmp/sse.* /tmp/dhub.log
BODY
}

if [ "$LOCAL" = 1 ]; then
  echo "🔧 build native binaries (local mode)"
  go build -o "$TMP/devbox" ./cmd/devbox
  go build -o "$TMP/devbox-hub" ./cmd/devbox-hub
  echo "📡 localhost: hub --dashboard + drive join·push·pull·conflict·gc"
  RESULT=$(gen_body 127.0.0.1 "$TMP" | bash 2>/dev/null)
else
  echo "🔧 build linux/arm64 binaries"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$TMP/devbox" ./cmd/devbox
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$TMP/devbox-hub" ./cmd/devbox-hub
  scp -q -i "$KEY" "$TMP/devbox" "$TMP/devbox-hub" "shoemoney@$HUB_PI:/tmp/"
  echo "📡 $HUB_PI: hub --dashboard + drive join·push·pull·conflict·gc"
  RESULT=$(gen_body "$HUB_PI" /tmp | ssh -o ConnectTimeout=8 -i "$KEY" "shoemoney@$HUB_PI" bash 2>/dev/null)
fi
echo "$RESULT"

echo "🔎 verdict"
for ev in join push pull conflict gc; do
  echo "$RESULT" | grep -q "\"type\":\"$ev\"" || { echo "🛑 no '$ev' flow event on the SSE stream"; exit 1; }
done
echo "$RESULT" | grep -q '"history":\[' || { echo "🛑 /api/state missing the server-side history window"; exit 1; }
echo "🎉 dashboard verified: join·push·pull·conflict·gc all stream over SSE + history window present."
