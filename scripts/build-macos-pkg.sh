#!/usr/bin/env bash
set -euo pipefail
export COPYFILE_DISABLE=1

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-"$ROOT_DIR/dist"}"
VERSION="${VERSION:-"$(git -C "$ROOT_DIR" describe --tags --always --dirty)"}"
PKG_VERSION="${VERSION#v}"
ARCHES="${MACOS_PKG_ARCHES:-"$(go env GOARCH)"}"

if ! command -v pkgbuild >/dev/null 2>&1; then
  echo "pkgbuild is required to build macOS installer packages" >&2
  exit 1
fi

mkdir -p "$DIST_DIR"

for arch in $ARCHES; do
  workdir="$(mktemp -d)"
  trap 'rm -rf "$workdir"' EXIT

  payload="$workdir/payload"
  app="$payload/Applications/codex-tg.app"
  mkdir -p "$payload/usr/local/bin" "$app/Contents/MacOS" "$app/Contents/Resources"

  env CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -ldflags="-s -w" -buildvcs=false \
    -o "$payload/usr/local/bin/ctr-go" "$ROOT_DIR/cmd/ctr-go"

  env CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -ldflags="-s -w" -buildvcs=false \
    -o "$app/Contents/MacOS/ctr-go-tray" "$ROOT_DIR/cmd/ctr-go-tray"

  chmod 0755 "$payload/usr/local/bin/ctr-go" "$app/Contents/MacOS/ctr-go-tray"
  cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>
  <string>ctr-go-tray</string>
  <key>CFBundleIdentifier</key>
  <string>tech.mideco.codex-tg.tray</string>
  <key>CFBundleName</key>
  <string>codex-tg</string>
  <key>CFBundleDisplayName</key>
  <string>codex-tg</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>${PKG_VERSION}</string>
  <key>CFBundleVersion</key>
  <string>${PKG_VERSION}</string>
  <key>LSUIElement</key>
  <true/>
</dict>
</plist>
PLIST

  find "$payload" -name '._*' -delete
  xattr -cr "$payload" 2>/dev/null || true

  pkgbuild \
    --root "$payload" \
    --identifier "tech.mideco.codex-tg" \
    --version "$PKG_VERSION" \
    --install-location "/" \
    "$DIST_DIR/codex-tg_${VERSION}_darwin_${arch}.pkg"

  rm -rf "$workdir"
  trap - EXIT
done
