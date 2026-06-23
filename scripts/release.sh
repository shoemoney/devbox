#!/usr/bin/env bash
set -euo pipefail

# One-shot local release. No CI runner required.
#
#   ./scripts/release.sh            # (re)publish the moving 'latest' release
#   ./scripts/release.sh v1.0.0     # cut a versioned release
#
# Builds every target here (private source repo), then publishes to the PUBLIC
# devbox-dist repo so `curl|sh` works without a token. Assets carry the
# de-versioned names install.sh downloads, plus the installers themselves:
#   install.sh  install.ps1  devbox_<os>_<arch>  devbox-hub_<os>_<arch>  SHA256SUMS
#
# Auth: ~/.config/forgejo/token (or $FORGEJO_TOKEN). Override with FORGEJO_API /
# REPO (publish target) / DIST_BRANCH if the dist repo ever moves.

cd "$(dirname "$0")/.."

TAG="${1:-latest}"
API="${FORGEJO_API:-https://git.shoemoney.ai/api/v1}"
REPO="${REPO:-shoemoney/devbox-dist}"   # public distribution repo (releases live here)
DIST_BRANCH="${DIST_BRANCH:-main}"      # tags hang off this branch in the dist repo
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

# 3. Create the release. Tag hangs off the dist repo's branch (the source $COMMIT
#    lives in the private repo, not here) — we keep $COMMIT only for provenance.
prerelease=$([ "$TAG" = latest ] && echo true || echo false)
body=$(printf 'devbox %s — built %s from source `%s`.\n\nInstall: `curl -fsSL https://git.shoemoney.ai/shoemoney/devbox-dist/releases/download/latest/install.sh | sh`' "$TAG" "$(date -u +%Y-%m-%dT%H:%MZ)" "${COMMIT:0:7}")
rel_id=$(api -X POST "$API/repos/$REPO/releases" \
  -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; t,c,b,p=sys.argv[1:5]; print(json.dumps({"tag_name":t,"target_commitish":c,"name":t,"body":b,"draft":False,"prerelease":p=="true"}))' "$TAG" "$DIST_BRANCH" "$body" "$prerelease")" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "✅ release '$TAG' created (id $rel_id)"

# 4. Upload de-versioned assets. dist names are <bin>_<version>_<os>_<arch>[.exe].
# deversion <basename> -> <bin>_<os>_<arch>[.exe] (the name install.sh fetches).
deversion() {
  local base="$1" ext="" arch rest os bin
  case "$base" in *.exe) ext=".exe"; base="${base%.exe}";; esac
  arch="${base##*_}"; rest="${base%_*}"
  os="${rest##*_}"; bin="${rest%_*}"; bin="${bin%_*}"   # strip version component
  printf '%s_%s_%s%s' "$bin" "$os" "$arch" "$ext"
}
upload() {
  local name; name=$(deversion "$(basename "$1")")
  echo "  ⬆ $name"
  api -X POST "$API/repos/$REPO/releases/$rel_id/assets?name=$name" -F "attachment=@$1" >/dev/null
}
for f in dist/devbox_* dist/devbox-hub_*; do [ -f "$f" ] && upload "$f"; done
# The installers ride along as assets so `curl|sh` and `irm|iex` pull current copies.
for s in install.sh install.ps1; do
  echo "  ⬆ $s"
  api -X POST "$API/repos/$REPO/releases/$rel_id/assets?name=$s" -F "attachment=@$s" >/dev/null
done
# Rewrite SHA256SUMS to the de-versioned names so `shasum -c` matches the assets.
while read -r h n; do printf '%s  %s\n' "$h" "$(deversion "$n")"; done \
  < dist/SHA256SUMS > dist/SHA256SUMS.release
echo "  ⬆ SHA256SUMS"
api -X POST "$API/repos/$REPO/releases/$rel_id/assets?name=SHA256SUMS" \
  -F "attachment=@dist/SHA256SUMS.release" >/dev/null

echo
echo "🎉 https://git.shoemoney.ai/$REPO/releases/tag/$TAG"
