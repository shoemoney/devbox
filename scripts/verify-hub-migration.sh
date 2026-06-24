#!/usr/bin/env bash
# verify-hub-migration.sh — prove a hub schema migration is safe BEFORE upgrading
# the live hub, by running the new binary against a COPY of real hub data.
#
# Why this exists: a hub DB migration that loses a byte is catastrophic (it's the
# index to every share's history). Unit tests use synthetic data; this runs the
# real thing in isolation. The live hub is NEVER touched — we copy and verify.
#
# GOTCHA baked in: the hub DB runs in WAL mode, so most data lives in
# devbox-hub.db-wal, NOT the 4 KB devbox-hub.db. Copy ONLY the .db and you migrate
# an empty database and get a false pass. We copy all three (db, -wal, -shm).
#
# Usage:
#   scripts/verify-hub-migration.sh                 # copy from the live .10 hub
#   scripts/verify-hub-migration.sh /path/to/data   # verify a local data dir copy
#
# Re-run after each new migration (M8a principals, M8-3 sidecar, M9 edge table…).
# Adjust the INVARIANTS block per migration: which counts must stay equal vs grow.
set -euo pipefail

HUB_HOST="${HUB_HOST:-shoemoney@192.168.1.10}"
HUB_DATA="${HUB_DATA:-/mnt/tank/apps/devbox/data}"
SRC="${1:-}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
DATA="$WORK/data"
mkdir -p "$DATA"

echo "🔧 building hub binary…"
go build -o "$WORK/devbox-hub" ./cmd/devbox-hub

if [[ -n "$SRC" ]]; then
  echo "📂 copying local hub data from $SRC (all WAL files)…"
  cp "$SRC"/devbox-hub.db* "$DATA"/
else
  echo "📡 copying live hub data from $HUB_HOST:$HUB_DATA (all WAL files, live untouched)…"
  scp -o ConnectTimeout=10 "$HUB_HOST:$HUB_DATA/devbox-hub.db*" "$DATA"/
fi

have_sqlite() { command -v sqlite3 >/dev/null 2>&1; }
counts() { # "dev/share/snap/chunk" for a db file
  local d s n c
  d=$(sqlite3 "$1" "SELECT COUNT(*) FROM devices;" 2>/dev/null || echo '?')
  s=$(sqlite3 "$1" "SELECT COUNT(*) FROM shares;" 2>/dev/null || echo '?')
  n=$(sqlite3 "$1" "SELECT COUNT(*) FROM snapshots;" 2>/dev/null || echo '?')
  c=$(sqlite3 "$1" "SELECT COUNT(*) FROM chunks;" 2>/dev/null || echo '?')
  echo "$d/$s/$n/$c"
}

# Capture BEFORE the binary touches the copy — robust whether or not a migration
# actually runs (a same-version redeploy is a legitimate no-op, not a failure).
snapcounts() { sqlite3 "$1" 'SELECT (SELECT COUNT(*) FROM devices),(SELECT COUNT(*) FROM shares),(SELECT COUNT(*) FROM snapshots),(SELECT COUNT(*) FROM chunks);' 2>/dev/null | tr '|' ' '; }
if have_sqlite; then
  vB=$(sqlite3 "$DATA/devbox-hub.db" 'PRAGMA user_version;')
  read -r dB sB nB cB <<<"$(snapcounts "$DATA/devbox-hub.db")"
  echo "📊 BEFORE: user_version=$vB  dev/share/snap/chunk=$dB/$sB/$nB/$cB"
fi

echo "🚀 opening DB with the new binary (gc --keep 100000 → Open runs any pending migration, prunes nothing)…"
"$WORK/devbox-hub" gc --data "$DATA" --keep 100000; rc=$?
echo "   exit: $rc"

echo
echo "🔎 checks:"
fail=0
check() { if eval "$2"; then echo "  ✅ $1"; else echo "  ❌ $1"; fail=1; fi; }
check "new binary opened the DB cleanly (exit 0)" "[[ $rc -eq 0 ]]"

if have_sqlite; then
  vA=$(sqlite3 "$DATA/devbox-hub.db" 'PRAGMA user_version;')
  read -r dA sA nA cA <<<"$(snapcounts "$DATA/devbox-hub.db")"
  echo "  ℹ️  AFTER: user_version=$vA  dev/share/snap/chunk=$dA/$sA/$nA/$cA"
  # --- INVARIANTS: devices/shares/chunks identical; snapshots may only grow; ----
  # user_version may only advance; a v1→v2 jump must leave its backup. -----------
  check "user_version did not regress ($vB → $vA)" "[[ $vA -ge $vB ]]"
  check "devices preserved ($dB → $dA)" "[[ $dB -eq $dA ]]"
  check "shares preserved ($sB → $sA)" "[[ $sB -eq $sA ]]"
  check "chunks preserved ($cB → $cA)" "[[ $cB -eq $cA ]]"
  check "snapshots not lost ($nB → $nA, growth ok for backfill)" "[[ $nA -ge $nB ]]"
  orphans=$(sqlite3 "$DATA/devbox-hub.db" "SELECT COUNT(*) FROM shares s WHERE s.head_snapshot IS NOT NULL AND s.head_snapshot!='' AND NOT EXISTS (SELECT 1 FROM snapshots o WHERE o.share=s.name AND o.id=s.head_snapshot);")
  check "every non-empty head has its own snapshot row (orphans=$orphans)" "[[ \"$orphans\" -eq 0 ]]"
  if [[ "$vB" -eq 0 && "$vA" -gt 0 ]]; then
    check "v1→v2 pre-migration backup created" "[[ -f \"$DATA/devbox-hub.db.pre-v2.bak\" ]]"
  else
    echo "  ℹ️  no v1→v2 migration this run (already at v$vA) — backup check N/A"
  fi
else
  echo "  ⚠️  sqlite3 not found — skipped count/schema invariants (install for full checks)"
fi

echo
if [[ "$fail" -eq 0 ]]; then echo "🎉 migration verified safe on this data — OK to upgrade the live hub."; else echo "🛑 migration verification FAILED — do NOT upgrade the live hub."; exit 1; fi
