#!/usr/bin/env bash
# m8a-invite-fleet-verify.sh — prove the M8a device-facing invite flow end to end
# across TWO real machines, against an ISOLATED hub (never the live .10 hub).
#
# What it asserts on real hardware:
#   1. an owner publishes a share and invites a principal as editor;
#   2. a device on a DIFFERENT host redeems that invite via the join PoP flow and
#      lands enrolled as the bound principal with the bound role (members shows it);
#   3. that invited editor can actually push (the role write-gate allows it).
#
# GOTCHA baked in: the invite token is 64 hex chars (randomBearer = 32 bytes). An
# earlier run grepped [0-9a-f]{32} and truncated it → 401 on redeem. We grep {64}.
#
# Usage: scripts/m8a-invite-fleet-verify.sh [hubPi] [peerPi]
#   defaults: hubPi=192.168.1.13  peerPi=192.168.1.15  (arm64 Pis; pi4/.14 offline)
set -euo pipefail

HUB_PI="${1:-192.168.1.13}"
PEER_PI="${2:-192.168.1.15}"
PORT="${PORT:-8099}"
KEY="${PI_KEY:-$HOME/.ssh/pi}"
SSH="ssh -o ConnectTimeout=8 -o StrictHostKeyChecking=no -i $KEY"
HUB_URL="http://$HUB_PI:$PORT"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say() { printf '\n=== %s ===\n' "$1"; }

say "build linux/arm64 binaries"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$TMP/devbox" ./cmd/devbox
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$TMP/devbox-hub" ./cmd/devbox-hub

say "deploy (hub+devA → $HUB_PI, devB → $PEER_PI)"
scp -q -i "$KEY" "$TMP/devbox-hub" "$TMP/devbox" "shoemoney@$HUB_PI:/tmp/"
scp -q -i "$KEY" "$TMP/devbox" "shoemoney@$PEER_PI:/tmp/"

cleanup_fleet() {
  $SSH "shoemoney@$HUB_PI" 'pkill -9 -f devbox-hub 2>/dev/null; pkill -9 -f "devbox start" 2>/dev/null; rm -rf /tmp/devbox /tmp/devbox-hub /tmp/ivhub /tmp/devA /tmp/teamdir' 2>/dev/null || true
  $SSH "shoemoney@$PEER_PI" 'pkill -9 -f "devbox start" 2>/dev/null; rm -rf /tmp/devbox /tmp/devB /tmp/devB-team' 2>/dev/null || true
}
trap 'cleanup_fleet; rm -rf "$TMP"' EXIT

say "$HUB_PI: start isolated hub, devA join + publish + invite bob editor"
INV=$($SSH "shoemoney@$HUB_PI" bash <<EOF 2>/dev/null
set -e
pkill -9 -f devbox-hub 2>/dev/null || true
rm -rf /tmp/ivhub /tmp/devA /tmp/teamdir; mkdir -p /tmp/devA /tmp/teamdir
echo hello > /tmp/teamdir/readme.txt
nohup /tmp/devbox-hub serve --data /tmp/ivhub --listen $HUB_PI:$PORT >/tmp/ivhub.log 2>&1 &
sleep 2
TOK=\$(/tmp/devbox-hub token --data /tmp/ivhub)
export XDG_CONFIG_HOME=/tmp/devA
/tmp/devbox join $HUB_URL "\$TOK" >/dev/null
/tmp/devbox publish /tmp/teamdir team >/dev/null
/tmp/devbox invite team bob editor | grep -oE '[0-9a-f]{64}' | head -1
EOF
)
[ "${#INV}" -eq 64 ] || { echo "🛑 invite token not 64 chars (got ${#INV}) — extraction bug"; exit 1; }
echo "invite token ok (64 chars)"

say "$PEER_PI: devB redeems the invite, then mounts + edits + syncs"
RESULT=$($SSH "shoemoney@$PEER_PI" bash <<EOF 2>/dev/null
set -e
rm -rf /tmp/devB /tmp/devB-team; mkdir -p /tmp/devB
export XDG_CONFIG_HOME=/tmp/devB
/tmp/devbox join $HUB_URL $INV >/dev/null
echo "MEMBERS:"; /tmp/devbox members team
/tmp/devbox mount team /tmp/devB-team >/dev/null
BEFORE=\$(/tmp/devbox log team | grep -c .)
echo "edit by the invited editor" >> /tmp/devB-team/readme.txt
timeout 12 /tmp/devbox start >/dev/null 2>&1 || true
AFTER=\$(/tmp/devbox log team | grep -c .)
echo "SNAPSHOTS:\$BEFORE->\$AFTER"
EOF
)
echo "$RESULT"

say "verdict"
echo "$RESULT" | grep -q 'bob.*editor' || { echo "🛑 invite binding did NOT apply cross-machine"; exit 1; }
echo "$RESULT" | grep -qE 'SNAPSHOTS:1->2' || { echo "🛑 invited editor could not push (write gate wrong?)"; exit 1; }
echo "🎉 M8a invite flow fleet-verified: cross-machine redeem + role-gated push both work."
