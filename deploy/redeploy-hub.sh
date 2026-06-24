#!/usr/bin/env bash
# redeploy-hub.sh — SAFE redeploy of the devbox hub to the live NAS.
#
# The hub DB is the canonical index to every share's history, so this never YOLOs a
# binary onto it. The pipeline, in order:
#   1. build linux/amd64 (hub + client)
#   2. migration DRY-RUN on a COPY of live data (scripts/verify-hub-migration.sh) — live untouched
#   3. auth SMOKE the new binary in a throwaway instance on the NAS (invite-revoke + +s)
#   4. BACK UP the live binary + DB (stopped, so WAL is quiescent)
#   5. swap the binary + sync the systemd unit, restart
#   6. VERIFY the hub answers + the new code is live (/v1/invite/revoke → 401, not 404)
#   7. AUTO-ROLLBACK to the previous binary on any post-deploy failure
#
# Counts before/after are printed for eyeball (not a hard gate — concurrent client
# activity can legitimately move them; the migration safety is proven in step 2).
set -euo pipefail
cd "$(dirname "$0")/.."

HUB="${HUB:-nas}"                         # ssh alias for the NAS (192.168.1.10)
DEST="${DEST:-/mnt/tank/apps/devbox}"
LANIP="${LANIP:-192.168.1.10}"
URL="http://$LANIP:8088"
say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
counts() { curl -sf "$URL/metrics" 2>/dev/null | grep -E '^devbox_(devices|shares|snapshots|chunks) ' | sort; }

say "🔨 build linux/amd64 (hub + client, trimmed static)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o /tmp/dh-hub ./cmd/devbox-hub
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o /tmp/dh-client ./cmd/devbox

say "🧪 migration dry-run on a COPY of live data (live hub untouched)"
scripts/verify-hub-migration.sh

say "🧪 auth smoke the NEW binary on the NAS (throwaway instance, prod untouched)"
# /tmp is noexec on TrueNAS — stage the binaries on the app dataset so they can run.
ssh "$HUB" "mkdir -p '$DEST/.staging'"
scp -q /tmp/dh-hub /tmp/dh-client scripts/hub-auth-smoke.sh "$HUB:$DEST/.staging/"
ssh "$HUB" "chmod +x '$DEST/.staging/dh-hub' '$DEST/.staging/dh-client' && \
  bash '$DEST/.staging/hub-auth-smoke.sh' '$DEST/.staging/dh-hub' '$DEST/.staging/dh-client' 8388; \
  rc=\$?; rm -rf '$DEST/.staging'; exit \$rc"

say "🔢 pre-deploy counts"
PRE="$(counts)"; echo "${PRE:-<hub not answering>}"

say "💾 back up live binary + DB, swap, sync unit, restart"
scp -q deploy/devbox-hub.service "$HUB:/tmp/devbox-hub.service"
scp -q /tmp/dh-hub "$HUB:$DEST/devbox-hub.new"
ssh "$HUB" 'bash -s' "$DEST" <<'DEPLOY'
set -e
DEST="$1"; cd "$DEST"
STAMP=$(date +%Y%m%d-%H%M%S); mkdir -p backups
cp -a devbox-hub "backups/devbox-hub.$STAMP"; ln -sf "devbox-hub.$STAMP" backups/devbox-hub.rollback
sudo systemctl stop devbox-hub
for f in data/devbox-hub.db data/devbox-hub.db-wal data/devbox-hub.db-shm; do
  [ -f "$f" ] && cp -a "$f" "backups/$(basename "$f").$STAMP"
done
mv devbox-hub.new devbox-hub && chmod +x devbox-hub
sudo cp /tmp/devbox-hub.service /etc/systemd/system/devbox-hub.service && sudo systemctl daemon-reload
sudo systemctl start devbox-hub
sleep 2; systemctl is-active devbox-hub >/dev/null && echo "  service active (backup: backups/devbox-hub.$STAMP)"
DEPLOY

say "🔎 post-deploy verify (auto-rollback on failure)"
rollback() {
  echo "↩️  ROLLING BACK to the previous binary"
  ssh "$HUB" "cd '$DEST' && sudo systemctl stop devbox-hub && cp -a backups/devbox-hub.rollback devbox-hub && sudo systemctl start devbox-hub && sleep 2 && systemctl is-active devbox-hub" || true
  echo "🛑 redeploy FAILED — rolled back to the previous binary"; exit 1
}
curl --retry 20 --retry-connrefused --retry-delay 1 --max-time 25 -sf "$URL/metrics" >/dev/null || rollback
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL/v1/invite/revoke")
[ "$code" = 401 ] || { echo "  ✗ /v1/invite/revoke = $code (want 401 — new code not live)"; rollback; }
echo "  ✓ /v1/invite/revoke present (401 unauth) — P2 code is LIVE"

say "🔢 post-deploy counts (compare to pre — device/share/chunk should match)"
echo "$(counts)"

say "✅ hub redeployed safely — P2 (+s attenuation · invite revocation) + P3 (gc-every) are live."
