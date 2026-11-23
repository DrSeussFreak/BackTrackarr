#!/bin/bash
# Builds a signed BackTrackarr APK with only the Android SDK.
#
#   KEYSTORE=/safe/path/app.p12 KEYSTORE_PASS_FILE=/safe/path/app.pass \
#   SERVER_URL=https://backtrackarr.example.com OUT=/path/BackTrackarr.apk ./build.sh
#
# SERVER_URL is optional: without it the app asks on first launch. A missing
# keystore is created. Keep it: Android only installs updates signed by the
# same key. Never commit it.
set -euo pipefail
cd "$(dirname "$0")"

SDK="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
JBR="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
[ -d "$JBR" ] && export JAVA_HOME="$JBR"
JAVA_BIN="${JAVA_HOME:+$JAVA_HOME/bin/}"
TOOLS="$SDK/build-tools/$(ls "$SDK/build-tools" | sort -V | tail -1)"
PLATFORM="$SDK/platforms/$(ls "$SDK/platforms" | grep '^android-36' | sort -V | tail -1)/android.jar"
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT
OUT="${OUT:-$PWD/build/BackTrackarr.apk}"
KEYSTORE="${KEYSTORE:-$PWD/.signing/backtrackarr.p12}"
PASS_FILE="${KEYSTORE_PASS_FILE:-$PWD/.signing/backtrackarr.pass}"
mkdir -p "$(dirname "$OUT")" "$(dirname "$KEYSTORE")" "$(dirname "$PASS_FILE")"

if [ ! -f "$KEYSTORE" ]; then
  umask 077
  openssl rand -hex 24 > "$PASS_FILE"
  "${JAVA_BIN}keytool" -genkeypair -keystore "$KEYSTORE" -storetype PKCS12 -storepass:file "$PASS_FILE" \
    -alias backtrackarr -keyalg RSA -keysize 4096 -validity 10000 -dname "CN=BackTrackarr" 2>/dev/null
fi

# Icons, from the app's own logo.
LOGO="../../internal/web/static/logo-512.png"
cp -R res "$WORK/res"
for pair in mdpi:48 hdpi:72 xhdpi:96 xxhdpi:144 xxxhdpi:192; do
  dir="$WORK/res/mipmap-${pair%%:*}"
  mkdir -p "$dir"
  sips -z "${pair##*:}" "${pair##*:}" "$LOGO" --out "$dir/ic_launcher.png" >/dev/null
done

# The optional default address is generated, so none is stored in the source.
mkdir -p "$WORK/gen/com/drseussfreak/backtrackarr" "$WORK/classes" "$WORK/dex"
ESCAPED="$(printf '%s' "${SERVER_URL:-}" | sed 's/\\/\\\\/g; s/"/\\"/g')"
cat > "$WORK/gen/com/drseussfreak/backtrackarr/Defaults.java" <<EOF
package com.drseussfreak.backtrackarr;
final class Defaults { static final String SERVER = "$ESCAPED"; private Defaults() {} }
EOF

"$TOOLS/aapt2" compile --dir "$WORK/res" -o "$WORK/res.zip"
"$TOOLS/aapt2" link -o "$WORK/base.apk" -I "$PLATFORM" --manifest AndroidManifest.xml \
  --java "$WORK/gen" --min-sdk-version 26 --target-sdk-version 36 "$WORK/res.zip"

find src "$WORK/gen" -name '*.java' > "$WORK/sources"
"${JAVA_BIN}javac" -nowarn -Xlint:none -source 8 -target 8 -bootclasspath "$PLATFORM" -d "$WORK/classes" "@$WORK/sources" 2>&1 \
  | { grep -v -E 'obsolete|suppress|deprecat|Note:' || true; }
find "$WORK/classes" -name '*.class' > "$WORK/classlist"
"$TOOLS/d8" --release --min-api 26 --lib "$PLATFORM" --output "$WORK/dex" "@$WORK/classlist"

cp "$WORK/base.apk" "$WORK/unsigned.apk"
(cd "$WORK/dex" && zip -q -j "$WORK/unsigned.apk" classes.dex)
"$TOOLS/zipalign" -f -p 4 "$WORK/unsigned.apk" "$WORK/aligned.apk"
"$TOOLS/apksigner" sign --ks "$KEYSTORE" --ks-pass "file:$PASS_FILE" --ks-key-alias backtrackarr --out "$OUT" "$WORK/aligned.apk"
"$TOOLS/apksigner" verify "$OUT"
rm -f "$OUT.idsig"
echo "Built $OUT"
