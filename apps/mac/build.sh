#!/bin/bash
# Builds a universal, ad-hoc signed BackTrackarr.app with Xcode's command line tools.
#
#   SERVER_URL=https://backtrackarr.example.com OUT="$HOME/Applications/BackTrackarr.app" ./build.sh
#
# SERVER_URL is optional: without it the app asks on first launch.
set -euo pipefail
cd "$(dirname "$0")"

OUT="${OUT:-$PWD/build/BackTrackarr.app}"
case "$OUT" in *.app) ;; *) echo "OUT must end in .app" >&2; exit 1 ;; esac
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT
APP="$WORK/BackTrackarr.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

# Compile a private copy: file-sync tools can touch the original mid-build.
mkdir -p "$WORK/src"
cp main.swift "$WORK/src/main.swift"
for arch in arm64 x86_64; do
  swiftc -O -swift-version 5 -target "$arch-apple-macos12" -framework Cocoa -framework WebKit "$WORK/src/main.swift" -o "$WORK/app-$arch"
done
lipo -create "$WORK/app-arm64" "$WORK/app-x86_64" -output "$APP/Contents/MacOS/BackTrackarr"

LOGO="../../internal/web/static/logo-512.png"
ICONS="$WORK/AppIcon.iconset"
mkdir -p "$ICONS"
for size in 16 32 128 256; do
  sips -z "$size" "$size" "$LOGO" --out "$ICONS/icon_${size}x${size}.png" >/dev/null
  double=$((size * 2))
  sips -z "$double" "$double" "$LOGO" --out "$ICONS/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONS" -o "$APP/Contents/Resources/AppIcon.icns"

cat > "$APP/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>BackTrackarr</string>
  <key>CFBundleDisplayName</key><string>BackTrackarr</string>
  <key>CFBundleIdentifier</key><string>com.drseussfreak.backtrackarr</string>
  <key>CFBundleExecutable</key><string>BackTrackarr</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.entertainment</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSAppTransportSecurity</key><dict><key>NSAllowsArbitraryLoads</key><true/></dict>
</dict></plist>
EOF
if [ -n "${SERVER_URL:-}" ]; then
  plutil -insert BTDefaultServer -string "$SERVER_URL" "$APP/Contents/Info.plist"
fi

codesign --force --deep --sign - "$APP"
codesign --verify --deep --strict "$APP"
mkdir -p "$(dirname "$OUT")"
rm -rf -- "$OUT"
ditto "$APP" "$OUT"
echo "Built $OUT"
