#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-"$ROOT_DIR/dist"}"
VERSION="${VERSION:-"$(git -C "$ROOT_DIR" describe --tags --always --dirty)"}"

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

targets=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  binary="ctr-go"
  if [[ "$goos" == "windows" ]]; then
    binary="ctr-go.exe"
  fi

  package="ctr-go_${VERSION}_${goos}_${goarch}"
  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT

  env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w" -buildvcs=false \
    -o "$workdir/$binary" "$ROOT_DIR/cmd/ctr-go"

  cp "$ROOT_DIR/README.md" "$workdir/README.md"
  cp "$ROOT_DIR/LICENSE" "$workdir/LICENSE"

  if [[ "$goos" == "windows" ]]; then
    (cd "$workdir" && zip -qr "$DIST_DIR/${package}.zip" .)
  else
    (cd "$workdir" && tar -czf "$DIST_DIR/${package}.tar.gz" .)
  fi

  rm -rf "$workdir"
  trap - EXIT
done

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$DIST_DIR" && sha256sum * > SHA256SUMS)
else
  (cd "$DIST_DIR" && shasum -a 256 * > SHA256SUMS)
fi
