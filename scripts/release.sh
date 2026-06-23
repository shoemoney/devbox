#!/usr/bin/env bash
set -euo pipefail

# One-shot local release to Forgejo. No CI runner required.
#
#   ./scripts/release.sh            # (re)publish the moving 'latest' release from HEAD
#   ./scripts/release.sh v1.0.0     # cut a versioned release (creates the git tag at HEAD)
#
# Builds every target via build-release.sh, then creates/updates the Forgejo release
# and uploads assets under the *de-versioned* names install.sh downloads:
#   devbox_<os>_<arch>   devbox-hub_<os>_<arch>   (+ .exe on windows)   + SHA256SUMS
#
# Auth: ~/.config/forgejo/token (or $FORGEJO_TOKEN). Override host/repo with
# FORGEJO_API / REPO if you ever move the remote.

cd "$(dirname "$0")/.."

TAG="${1:-latest}"
API="${FORGEJO_API:-https://git.shoemoney.ai/api/v1}"
REPO="${REPO:-shoemoney/devbox}"
TOKEN="${FORGEJO_TOKEN:-$(cat ~/.config/forgejo/token 2>/dev/null || true)}"
[ -n "$TOKEN" ] || { echo "🛑 no Forgejo token (set FORGEJO_TOKEN or ~/.config/forgejo/token)" >&2; exit 1; }

auth=(-H "Authorization: token $TOKEN")
api() { curl -fsSL "${auth[@]}" "$@"; }

# 1. Build. A real tag stamps its name into the binary; 'latest' uses git-describe.
if [ "$TAG" = latest ]; then
  ./scripts/build-release.sh
else
  VERSION="$TAG" ./scripts/build-release.sh
fi

COMMIT=$(git rev-parse HEAD)

# 2. Drop any existing release+tag with this name so we recreate cleanly (latest is a
#    moving target; re-cutting a vX.Y.Z is rare but should be idempotent too).
old_id=$(api "$API/repos/$REPO/releases/tags/$TAG" 2>/dev/null \
           | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)
if [ -n "${old_id:-}" ]; then
  echo "↻ replacing existing release '$TAG' (id $old_id)"
  curl -fsSL -X DELETE "${auth[@]}" "$API/repos/$REPO/releases/$old_id" || true
fi
# Move the lightweight tag to HEAD for 'latest'; leave real version tags alone if present.
if [ "$TAG" = latest ]; then
  curl -fsSL -X DELETE "${auth[@]}" "$API/repos/$REPO/tags/$TAG" >/dev/null 2>&1 || true
fi

# 3. Create the release (Forgejo creates the tag at $COMMIT if it doesn't exist).
prerelease=$([ "$TAG" = latest ] && echo true || echo false)
body=$(printf 'devbox %s — built %s from `%s`.\n\nInstall: `curl -fsSL https://git.shoemoney.ai/shoemoney/devbox/raw/branch/main/install.sh | sh`' "$TAG" "$(date -u +%Y-%m-%dT%H:%MZ)" "${COMMIT:0:7}")
rel_id=$(api -X POST "$API/repos/$REPO/releases" \
  -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; t,c,b,p=sys.argv[1:5]; print(json.dumps({"tag_name":t,"target_commitish":c,"name":t,"body":b,"draft":False,"prerelease":p=="true"}))' "$TAG" "$COMMIT" "$body" "$prerelease")" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "✅ release '$TAG' created (id $rel_id)"

# 4. Upload de-versioned assets. dist names are <bin>_<version>_<os>_<arch>[.exe].
upload() {
  local f="$1" base ext arch rest os bin name
  base=$(basename "$f"); ext=""
  case "$base" in *.exe) ext=".exe"; base="${base%.exe}";; esac
  arch="${base##*_}"; rest="${base%_*}"
  os="${rest##*_}"; bin="${rest%_*}"; bin="${bin%_*}"   # strip version component
  name="${bin}_${os}_${arch}${ext}"
  echo "  ⬆ $name"
  api -X POST "$API/repos/$REPO/releases/$rel_id/assets?name=$name" \
    -F "attachment=@$f" >/dev/null
}
for f in dist/devbox_* dist/devbox-hub_*; do [ -f "$f" ] && upload "$f"; done
# checksums keep their plain name
echo "  ⬆ SHA256SUMS"
api -X POST "$API/repos/$REPO/releases/$rel_id/assets?name=SHA256SUMS" \
  -F "attachment=@dist/SHA256SUMS" >/dev/null

echo
echo "🎉 https://git.shoemoney.ai/$REPO/releases/tag/$TAG"
