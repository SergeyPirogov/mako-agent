#!/bin/sh
# Builds Mako.app (a universal binary for Apple Silicon and Intel, menu bar only) and Mako-<version>-mac.zip.
# Run on a Mac with Go and Xcode command line tools: go/packaging/mac/build-app.sh [version]
set -eu
cd "$(dirname "$0")/../.."
VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
# The clone Mako is built from (the main one, when building in a worktree): Mako updates itself from it.
SOURCE="$(dirname "$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || echo .)")"
OUT="dist"
APP="$OUT/Mako.app"
rm -rf "$OUT"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

# The menu bar icon needs cgo (AppKit); each architecture is built natively by clang, then joined.
for arch in arm64 amd64; do
  CGO_ENABLED=1 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X 'main.source=$SOURCE'" -o "$OUT/mako-$arch" ./cmd/mako
done
lipo -create -output "$APP/Contents/MacOS/mako" "$OUT/mako-arm64" "$OUT/mako-amd64"
rm "$OUT/mako-arm64" "$OUT/mako-amd64"

sed "s/__VERSION__/$VERSION/g" packaging/mac/Info.plist > "$APP/Contents/Info.plist"

# The app icon from assets/icon.png (1024 px).
ICONSET="$OUT/icon.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
  sips -z $size $size ../assets/icon.png --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) ../assets/icon.png --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/icon.icns"
rm -rf "$ICONSET"

# Ad-hoc signature: enough to run on Apple Silicon; not notarized, so the first start is right click → Open.
codesign --force --deep --sign - "$APP"
ditto -c -k --keepParent "$APP" "$OUT/Mako-$VERSION-mac.zip"
echo "$APP"
echo "$OUT/Mako-$VERSION-mac.zip"
