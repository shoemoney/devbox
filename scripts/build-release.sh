#!/usr/bin/env bash
set -euo pipefail

# Cross-compile both devbox binaries for all release targets.
# Override version: VERSION=v1.2.3 ./scripts/build-release.sh

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=$VERSION"

BINS=(devbox devbox-hub)
TARGETS=(linux/amd64 darwin/arm64 darwin/amd64 windows/amd64)

rm -rf dist && mkdir -p dist

echo "Building version: $VERSION"

for bin in "${BINS[@]}"; do
  for target in "${TARGETS[@]}"; do
    os="${target%/*}"
    arch="${target#*/}"
    out="dist/${bin}_${VERSION}_${os}_${arch}"
    [ "$os" = "windows" ] && out="${out}.exe"
    echo "  -> $out"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      go build -ldflags "$LDFLAGS" -o "$out" "./cmd/${bin}"
  done
done

# Generate checksums.
echo "Generating dist/SHA256SUMS"
(
  cd dist
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 * > SHA256SUMS
  else
    sha256sum * > SHA256SUMS
  fi
)

echo
echo "Done. Artifacts in dist/:"
ls -lh dist/
