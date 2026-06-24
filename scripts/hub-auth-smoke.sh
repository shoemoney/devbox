#!/usr/bin/env bash
# hub-auth-smoke.sh — smoke-test a hub binary's M8a auth on a THROWAWAY instance
# (never touches a real hub): proves invite revocation works and +s attenuation
# holds. Reusable by redeploy-hub.sh and by hand / CI.
#
#   scripts/hub-auth-smoke.sh <devbox-hub-bin> <devbox-bin> [port]
#
# Needs: curl, sed (no python/jq). The two binaries must match the host's OS/arch.
set -euo pipefail

HUB_BIN="${1:?usage: hub-auth-smoke.sh <devbox-hub-bin> <devbox-bin> [port]}"
DEVBOX_BIN="${2:?need the devbox client binary too}"
PORT="${3:-8388}"
BASE="http://127.0.0.1:$PORT"
T="$(mktemp -d)"
cleanup() { pkill -9 -f "$HUB_BIN serve --data $T/data" 2>/dev/null || true; rm -rf "$T"; }
trap cleanup EXIT
mkdir -p "$T/data" "$T/work"; echo smoke > "$T/work/f.txt"
fail() { echo "🛑 SMOKE FAIL: $*"; exit 1; }
tok() { sed -n 's/.*"token":"\([^"]*\)".*/\1/p'; }                    # pluck a JSON token field

"$HUB_BIN" serve --data "$T/data" --listen "127.0.0.1:$PORT" >"$T/hub.log" 2>&1 &
for _ in $(seq 1 25); do curl -sf "$BASE/metrics" >/dev/null 2>&1 && break; sleep 0.3; done
curl -sf "$BASE/metrics" >/dev/null 2>&1 || fail "hub never came up (see $T/hub.log)"

join() { # $1=cfgdir $2=token  -> stdout: device bearer
  XDG_CONFIG_HOME="$1" "$DEVBOX_BIN" join "$BASE" "$2" >/dev/null
  sed -n 's/^bearer *= *"\([^"]*\)".*/\1/p' "$1/devbox/daemon.toml"
}
api() { curl -s -H "Authorization: Bearer $1" -H 'Content-Type: application/json' "${@:2}"; }

# Owner enrolls and publishes legacy share 'team' (implicit owner).
OWNER=$(join "$T/owner" "$("$HUB_BIN" token --data "$T/data")")
[ -n "$OWNER" ] || fail "owner join produced no bearer"
XDG_CONFIG_HOME="$T/owner" "$DEVBOX_BIN" publish "$T/work" team >/dev/null

# 1) Invite revocation: mint → revoke(200) → redeem must be refused.
INV=$(api "$OWNER" -d '{"share":"team","principal":"bob","role":"editor"}' "$BASE/v1/invite" | tok)
[ -n "$INV" ] || fail "invite mint returned no token"
code=$(api "$OWNER" -o /dev/null -w '%{http_code}' -d "{\"token\":\"$INV\"}" "$BASE/v1/invite/revoke")
[ "$code" = 200 ] || fail "revoke expected 200, got $code (old binary? /v1/invite/revoke missing)"
if XDG_CONFIG_HOME="$T/bob" "$DEVBOX_BIN" join "$BASE" "$INV" >/dev/null 2>&1; then fail "a REVOKED invite still redeemed"; fi
echo "  ✅ invite revocation — minted, revoked (200), redeem refused"

# 2) +s attenuation: an admin WITHOUT +s must not confer +s; but may grant normally.
ADMINV=$(api "$OWNER" -d '{"share":"team","principal":"adm","role":"admin","reshare":false}' "$BASE/v1/invite" | tok)
ADM=$(join "$T/adm" "$ADMINV")
[ -n "$ADM" ] || fail "admin join produced no bearer"
code=$(api "$ADM" -o /dev/null -w '%{http_code}' -d '{"share":"team","principal":"carol","role":"editor","reshare":true}' "$BASE/v1/invite")
[ "$code" = 403 ] || fail "admin-without-+s conferring +s expected 403, got $code (ESCALATION reopened)"
code=$(api "$ADM" -o /dev/null -w '%{http_code}' -d '{"share":"team","principal":"dave","role":"editor","reshare":false}' "$BASE/v1/invite")
[ "$code" = 200 ] || fail "admin granting a non-resharing editor expected 200, got $code"
echo "  ✅ +s attenuation — admin denied conferring +s (403); normal grant ok (200)"

echo "🎉 hub auth smoke passed"
