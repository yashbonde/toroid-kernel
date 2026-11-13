#!/bin/bash
# Builds build/toroid-chat.apk without Gradle:
#   go build (android/arm64) -> aapt2 -> javac -> d8 -> zip (.so stored) -> zipalign -> apksigner
#
#   ANDROID_HOME=/path/to/sdk ./examples/android-chat/build.sh           # build
#   ANDROID_HOME=/path/to/sdk ./examples/android-chat/build.sh install   # build + adb install -r
#
# The SDK needs platforms/android-29 and build-tools/34.0.0. JDK 17 is taken
# from JAVA_HOME (default: Homebrew openjdk@17). The debug keystore is created
# on first use as debug.jks next to this script (gitignored); keep it, since
# Android refuses to update an app signed with a different key.
set -euo pipefail

P=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$P/../.." && pwd)
SDK=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}
[ -n "$SDK" ] || { echo "set ANDROID_HOME to the Android SDK" >&2; exit 1; }
BT=$SDK/build-tools/${BUILD_TOOLS:-34.0.0}
PLAT=$SDK/platforms/android-29/android.jar
export JAVA_HOME=${JAVA_HOME:-/opt/homebrew/opt/openjdk@17}
export PATH=$JAVA_HOME/bin:$PATH
GO=${GO:-go}
A=$P/android
B=$P/build
KS=${KEYSTORE:-$P/debug.jks}

rm -rf "$B" && mkdir -p "$B/gen" "$B/classes" "$B/dex" "$B/native/lib/arm64-v8a"

# 1. The Go server, named like a JNI library so the package manager extracts it
#    into nativeLibraryDir (the only app-owned path Android 10 lets us exec).
(cd "$ROOT" && CGO_ENABLED=0 GOOS=android GOARCH=arm64 "$GO" build -ldflags "-s -w" -trimpath \
    -o "$B/native/lib/arm64-v8a/libtoroidchat.so" ./examples/android-chat)

# 2. Resources + manifest, then Java -> dex.
"$BT/aapt2" compile --dir "$A/res" -o "$B/res.zip"
"$BT/aapt2" link -o "$B/base.apk" -I "$PLAT" --manifest "$A/AndroidManifest.xml" "$B/res.zip" \
    --java "$B/gen" --min-sdk-version 29 --target-sdk-version 29
javac -Xlint:-options -source 8 -target 8 -bootclasspath "$PLAT" -d "$B/classes" \
    $(find "$A/src" "$B/gen" -name '*.java')
"$BT/d8" --release --min-api 29 --lib "$PLAT" --output "$B/dex" $(find "$B/classes" -name '*.class')

# 3. Assemble: dex compressed, the .so stored (-0) so zipalign -p can page-align it.
cp "$B/base.apk" "$B/unsigned.apk"
(cd "$B/dex" && zip -q "$B/unsigned.apk" classes.dex)
(cd "$B/native" && zip -q -0 "$B/unsigned.apk" lib/arm64-v8a/libtoroidchat.so)
"$BT/zipalign" -p -f 4 "$B/unsigned.apk" "$B/aligned.apk"

# 4. Sign.
[ -f "$KS" ] || keytool -genkeypair -keystore "$KS" -storepass android -keypass android \
    -alias toroidchat -keyalg RSA -keysize 2048 -validity 10000 -dname "CN=Toroid Chat Debug"
"$BT/apksigner" sign --ks "$KS" --ks-pass pass:android --key-pass pass:android \
    --out "$B/toroid-chat.apk" "$B/aligned.apk"
"$BT/apksigner" verify "$B/toroid-chat.apk"
echo "built $B/toroid-chat.apk ($(du -h "$B/toroid-chat.apk" | cut -f1))"

if [ "${1:-}" = "install" ]; then
    adb install -r "$B/toroid-chat.apk"
fi
