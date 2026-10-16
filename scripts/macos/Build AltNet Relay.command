#!/bin/bash
# AltNet Relay for Mac -- builds the app on this Mac and makes an installer
# (.dmg) next to this file. Double-click it.
#
# The first time, macOS may say it can't open it because it's from an
# unidentified developer: right-click this file, choose Open, then Open.
#
# What it does, in order:
#   1. Installs Apple's Command Line Tools if they aren't there (Apple's own
#      window asks you to click Install). The app's window code needs them.
#   2. Downloads Go (the programming language the app is written in) into a
#      folder next to this file -- about 65 MB, only the first time -- and
#      checks it against its published fingerprint.
#   3. Builds the app from the source in src/ (everything it needs is in
#      this folder; nothing else is downloaded).
#   4. Wraps it as "AltNet Relay.app" and puts it in "AltNet Relay <version>.dmg".
#
# Nothing is installed anywhere else on your Mac, and no password is asked.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
VER="$(cat "$HERE/VERSION")"
LOG="$HERE/build.log"
exec > >(tee "$LOG") 2>&1

say()  { printf '\n==> %s\n' "$1"; }
finish() { echo; read -r -p "Press Return to close this window. " _ || true; }
fail() {
    printf '\n*** %s\n' "$1"
    echo "The details are in build.log, in this folder."
    finish
    exit 1
}
trap 'fail "Something went wrong (see above)."' ERR

say "AltNet Relay $VER for Mac"
sw_vers 2>/dev/null | sed 's/^/    /' || true
# Files that arrived by download or AirDrop are marked as such; the app built
# here shouldn't inherit that.
xattr -dr com.apple.quarantine "$HERE" 2>/dev/null || true

# ── 1. Apple's Command Line Tools ─────────────────────────────────────────
have_tools() { xcode-select -p >/dev/null 2>&1 && /usr/bin/xcrun --find clang >/dev/null 2>&1; }
if ! have_tools; then
    say "Installing Apple's Command Line Tools"
    echo "    A window from Apple will open: click Install and agree."
    echo "    It takes a few minutes. This window waits for it."
    xcode-select --install >/dev/null 2>&1 || true
    for _ in $(seq 1 240); do
        have_tools && break
        sleep 10
    done
    have_tools || fail "The Command Line Tools aren't installed yet. When they are, double-click this file again."
fi
echo "    Command Line Tools: ok"

# ── 2. Go ─────────────────────────────────────────────────────────────────
GOVER=1.26.2
case "$(uname -m)" in
    x86_64) GOARCH=amd64; GOSUM=bc3f1500d9968c36d705442d90ba91addf9271665033748b82532682e90a7966 ;;
    arm64)  GOARCH=arm64; GOSUM=32af1522bf3e3ff3975864780a429cc0b41d190ec7bf90faa661d6d64566e7af ;;
    *) fail "This Mac's processor ($(uname -m)) isn't supported." ;;
esac
TOOLS="$HERE/.toolchain"
GOROOT_HERE="$TOOLS/go"
if [ ! -x "$GOROOT_HERE/bin/go" ] || [ "$("$GOROOT_HERE/bin/go" env GOVERSION 2>/dev/null)" != "go$GOVER" ]; then
    say "Downloading Go $GOVER (about 65 MB, only this once)"
    mkdir -p "$TOOLS"
    TGZ="$TOOLS/go$GOVER.darwin-$GOARCH.tar.gz"
    curl -fL --progress-bar -o "$TGZ" "https://go.dev/dl/go$GOVER.darwin-$GOARCH.tar.gz" \
        || fail "Couldn't download Go. Is the Mac connected to the internet?"
    echo "$GOSUM  $TGZ" | shasum -a 256 -c - >/dev/null \
        || { rm -f "$TGZ"; fail "The Go download didn't match its published fingerprint, so it wasn't used. Try again."; }
    rm -rf "$GOROOT_HERE"
    tar -C "$TOOLS" -xzf "$TGZ"
    rm -f "$TGZ"
fi
export GOROOT="$GOROOT_HERE"
export PATH="$GOROOT/bin:$PATH"
export GOPATH="$TOOLS/gopath" GOCACHE="$TOOLS/cache" GOMODCACHE="$TOOLS/gopath/pkg/mod"
# Everything the app needs is in src/ (vendor/): no downloads, no surprises.
export GOFLAGS=-mod=vendor GOPROXY=off GOTOOLCHAIN=local GOARCH CGO_ENABLED=1
export CGO_CFLAGS="-mmacosx-version-min=12.0"
export CGO_LDFLAGS="-framework UniformTypeIdentifiers -mmacosx-version-min=12.0"
echo "    $(go version)"

# ── 3. Build ──────────────────────────────────────────────────────────────
say "Building the app (a minute or two the first time)"
BUILD="$HERE/build"
rm -rf "$BUILD"
mkdir -p "$BUILD"
( cd "$HERE/src/app/desktop" && go build -tags desktop,production -trimpath -buildvcs=false \
    -ldflags "-s -w" -o "$BUILD/AltNetRelay" . )

# ── 4. The .app ───────────────────────────────────────────────────────────
say "Putting the app together"
APP="$BUILD/AltNet Relay.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
mv "$BUILD/AltNetRelay" "$APP/Contents/MacOS/AltNetRelay"

ICONSET="$BUILD/AltNetRelay.iconset"
mkdir -p "$ICONSET"
PNG="$HERE/src/app/desktop/build/appicon.png"
for s in 16 32 128 256 512; do
    sips -z "$s" "$s" "$PNG" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
    d=$((s * 2))
    sips -z "$d" "$d" "$PNG" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/iconfile.icns"
rm -rf "$ICONSET"

cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleName</key><string>AltNet Relay</string>
    <key>CFBundleDisplayName</key><string>AltNet Relay</string>
    <key>CFBundleExecutable</key><string>AltNetRelay</string>
    <key>CFBundleIdentifier</key><string>org.panmox.altnetrelay</string>
    <key>CFBundleVersion</key><string>$VER</string>
    <key>CFBundleShortVersionString</key><string>$VER</string>
    <key>CFBundleIconFile</key><string>iconfile</string>
    <key>LSMinimumSystemVersion</key><string>12.0</string>
    <key>LSApplicationCategoryType</key><string>public.app-category.social-networking</string>
    <key>NSHighResolutionCapable</key><true/>
    <key>NSHumanReadableCopyright</key><string>(c) 2026 PANMOX</string>
    <key>NSMicrophoneUsageDescription</key><string>AltNet Relay uses the microphone for voice and video calls.</string>
    <key>NSCameraUsageDescription</key><string>AltNet Relay uses the camera for video calls, and only when you turn it on.</string>
</dict>
</plist>
EOF
# A local signature, so macOS lets the app ask for the microphone, the
# camera and notifications. (It isn't an Apple-approved signature: on another
# Mac it would need right-click, Open the first time.)
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 \
    || echo "    (couldn't sign it locally; it still runs on this Mac)"

# ── 5. The .dmg ───────────────────────────────────────────────────────────
say "Making the installer"
STAGE="$BUILD/dmg"
mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
DMG="$HERE/AltNet Relay $VER.dmg"
rm -f "$DMG"
hdiutil create -volname "AltNet Relay" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null
rm -rf "$STAGE"

trap - ERR
say "Done"
echo "    The installer is: $(basename "$DMG"), in this folder."
echo "    Open it and drag AltNet Relay onto Applications."
open -R "$DMG" || true
finish
