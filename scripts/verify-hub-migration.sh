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

if have_sqlite; then
  echo "📊 BEFORE: user_version=$(sqlite3 "$DATA/devbox-hub.db" 'PRAGMA user_version;')  counts(dev/share/snap/chunk)=$(counts "$DATA/devbox-hub.db")"
fi

echo "🚀 running migration (devbox-hub gc --keep 100000 triggers Open→migrate, prunes nothing)…"
"$WORK/devbox-hub" gc --data "$DATA" --keep 100000
echo "   exit: $?"

echo
echo "🔎 checks:"
fail=0
check() { if eval "$2"; then echo "  ✅ $1"; else echo "  ❌ $1"; fail=1; fi; }

check "pre-migration backup created" "[[ -f \"$DATA/devbox-hub.db.pre-v2.bak\" ]]"

if have_sqlite; then
  ver=$(sqlite3 "$DATA/devbox-hub.db" 'PRAGMA user_version;')
  echo "  ℹ️  AFTER: user_version=$ver  counts=$(counts "$DATA/devbox-hub.db")"
  check "user_version advanced past 0" "[[ \"$ver\" -gt 0 ]]"

  # --- INVARIANTS (adjust per migration) -------------------------------------
  # M8 (v1→v2): devices/shares/chunks identical; snapshots may grow (head backfill).
  bak="$DATA/devbox-hub.db.pre-v2.bak"
  for tbl in devices shares chunks; do
    before=$(sqlite3 "$bak" "SELECT COUNT(*) FROM $tbl;")
    after=$(sqlite3 "$DATA/devbox-hub.db" "SELECT COUNT(*) FROM $tbl;")
    check "$tbl preserved ($before -> $after)" "[[ \"$before\" -eq \"$after\" ]]"
  done
  snap_b=$(sqlite3 "$bak" "SELECT COUNT(*) FROM snapshots;")
  snap_a=$(sqlite3 "$DATA/devbox-hub.db" "SELECT COUNT(*) FROM snapshots;")
  check "snapshots not lost ($snap_b -> $snap_a, growth ok for backfill)" "[[ \"$snap_a\" -ge \"$snap_b\" ]]"

  orphans=$(sqlite3 "$DATA/devbox-hub.db" "SELECT COUNT(*) FROM shares s WHERE s.head_snapshot IS NOT NULL AND s.head_snapshot!='' AND NOT EXISTS (SELECT 1 FROM snapshots o WHERE o.share=s.name AND o.id=s.head_snapshot);")
  check "every non-empty head has its own snapshot row (orphans=$orphans)" "[[ \"$orphans\" -eq 0 ]]"
else
  echo "  ⚠️  sqlite3 not found — skipped count/schema invariants (install for full checks)"
fi

echo
if [[ "$fail" -eq 0 ]]; then echo "🎉 migration verified safe on this data — OK to upgrade the live hub."; else echo "🛑 migration verification FAILED — do NOT upgrade the live hub."; exit 1; fi
