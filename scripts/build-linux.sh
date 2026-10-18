#!/usr/bin/env bash
# Builds AltNet Relay for Linux: a .deb for Ubuntu and Debian, and a .tar.gz
# for other distributions. Run it inside WSL's Ubuntu (or any Debian/Ubuntu
# with Go, the Wails CLI, build-essential, libgtk-3-dev and
# libwebkit2gtk-4.1-dev):
#
#   wsl -d Ubuntu -u root -- bash scripts/build-linux.sh
#
# Output goes to dist/ next to the Windows installer. The AltNet core is read
# from ../AltNet (a clone of github.com/panmauk/AltNet next to this folder),
# or from ALTNET_CORE.
set -euo pipefail
export PATH=/usr/local/go/bin:$HOME/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

HERE="$(cd "$(dirname "$0")/.." && pwd)"
CORE="${ALTNET_CORE:-$HERE/../AltNet}"
VER="$(sed -n 's/.*"productVersion": "\(.*\)".*/\1/p' "$HERE/app/desktop/wails.json")"
WORK="$HOME/relay-linux-build"
OUT="$HERE/dist"
PKG=altnet-relay
fail() { echo "build-linux: $*" >&2; exit 1; }

command -v go >/dev/null || fail "Go not found"
command -v wails >/dev/null || fail "the Wails CLI not found (go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0)"
pkg-config --exists webkit2gtk-4.1 || fail "libwebkit2gtk-4.1-dev is missing"
[ -n "$CORE" ] || fail "set ALTNET_CORE to the AltNet core repository root"
[ -f "$CORE/core/peer/peer.go" ] || [ -d "$CORE/core/peer" ] || fail "AltNet core not found at $CORE"

echo "==> copying sources (version $VER)"
# On WSL's own file system: building on /mnt/... is slow and trips over
# Windows permissions.
rm -rf "$WORK"
mkdir -p "$WORK/relay" "$WORK/altnet"
tar -C "$HERE" --exclude=./dist --exclude=./app/desktop/build/bin --exclude=./.claude -cf - . | tar -C "$WORK/relay" -xf -
cp "$CORE/go.mod" "$WORK/altnet/"
cp -r "$CORE/core" "$WORK/altnet/"
# Point go.mod at the copied core tree in the build workspace.
sed -i 's#^replace altnet => .*#replace altnet => ../altnet#' "$WORK/relay/go.mod"
sed -i 's#^replace altnet => .*#replace altnet => ../../../altnet#' "$WORK/relay/app/desktop/go.mod"

echo "==> tests (engine, server, everything) on Linux"
(cd "$WORK/relay" && go test ./... 2>&1 | grep -E '^(ok|FAIL|---|panic)' )
(cd "$WORK/relay/app/desktop" && go test -tags webkit2_41 . 2>&1 | grep -E '^(ok|FAIL|---|panic)' )

echo "==> building the app"
(cd "$WORK/relay/app/desktop" && wails build -tags webkit2_41 -trimpath -s 2>&1 | grep -vE 'sponsor|^\s*$' | tail -4)
BIN="$WORK/relay/app/desktop/build/bin/AltNetRelay"
[ -x "$BIN" ] || fail "no binary was built"
GLIBC_NEEDED="$(objdump -T "$BIN" | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1)"
echo "    needs $GLIBC_NEEDED or newer"

echo "==> packaging the .deb"
ROOT="$WORK/${PKG}_${VER}_amd64"
mkdir -p "$ROOT/DEBIAN" "$ROOT/opt/$PKG" "$ROOT/usr/bin" "$ROOT/usr/share/applications" "$ROOT/usr/share/pixmaps"
install -m 0755 "$BIN" "$ROOT/opt/$PKG/AltNetRelay"
ln -s "/opt/$PKG/AltNetRelay" "$ROOT/usr/bin/$PKG"
install -m 0644 "$HERE/app/desktop/build/appicon.png" "$ROOT/usr/share/pixmaps/$PKG.png"
if command -v convert >/dev/null; then
    for s in 128 256 512; do
        mkdir -p "$ROOT/usr/share/icons/hicolor/${s}x${s}/apps"
        convert "$HERE/app/desktop/build/appicon.png" -resize ${s}x${s} "$ROOT/usr/share/icons/hicolor/${s}x${s}/apps/$PKG.png"
    done
fi
cat > "$ROOT/usr/share/applications/$PKG.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=AltNet Relay
GenericName=Private messenger
Comment=Private messages and calls on the AltNet network
Exec=$PKG
Icon=$PKG
Terminal=false
Categories=Network;InstantMessaging;Chat;
Keywords=altnet;chat;messenger;calls;p2p;
StartupWMClass=AltNetRelay
EOF
cat > "$ROOT/DEBIAN/control" <<EOF
Package: $PKG
Version: $VER
Section: net
Priority: optional
Architecture: amd64
Depends: libwebkit2gtk-4.1-0, libgtk-3-0
Installed-Size: $(du -ks "$ROOT/opt" | cut -f1)
Maintainer: PANMOX <panmauk@panmox.org>
Homepage: https://panmox.org
Description: AltNet Relay - private messages and calls on AltNet
 End-to-end encrypted chat, groups, workspaces, and voice and video calls,
 carried peer to peer over the AltNet network. Needs a free AltNet account.
EOF
cat > "$ROOT/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
update-desktop-database -q /usr/share/applications 2>/dev/null || true
gtk-update-icon-cache -q -t /usr/share/icons/hicolor 2>/dev/null || true
exit 0
EOF
chmod 755 "$ROOT/DEBIAN/postinst"
DEB="$WORK/AltNetRelay_${VER}_amd64.deb"
dpkg-deb --build --root-owner-group "$ROOT" "$DEB" >/dev/null

echo "==> packaging the .tar.gz (other distributions)"
TGZDIR="$WORK/AltNetRelay-$VER-linux-amd64"
mkdir -p "$TGZDIR"
install -m 0755 "$BIN" "$TGZDIR/AltNetRelay"
install -m 0644 "$HERE/app/desktop/build/appicon.png" "$TGZDIR/altnet-relay.png"
cat > "$TGZDIR/install.sh" <<'EOF'
#!/bin/sh
# Installs AltNet Relay for this user only (no administrator needed):
# the program in ~/.local/opt, a menu entry and an icon.
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
DEST="$HOME/.local/opt/altnet-relay"
mkdir -p "$DEST" "$HOME/.local/share/applications" "$HOME/.local/share/icons"
cp "$HERE/AltNetRelay" "$DEST/AltNetRelay"
chmod 755 "$DEST/AltNetRelay"
cp "$HERE/altnet-relay.png" "$HOME/.local/share/icons/altnet-relay.png"
cat > "$HOME/.local/share/applications/altnet-relay.desktop" <<EOD
[Desktop Entry]
Type=Application
Name=AltNet Relay
Comment=Private messages and calls on the AltNet network
Exec="$DEST/AltNetRelay"
Icon=$HOME/.local/share/icons/altnet-relay.png
Terminal=false
Categories=Network;InstantMessaging;Chat;
EOD
echo "Installed. Open AltNet Relay from your applications menu."
echo "It needs the WebKitGTK 4.1 and GTK 3 libraries (on most desktops they are already there)."
EOF
chmod 755 "$TGZDIR/install.sh"
cat > "$TGZDIR/README.txt" <<EOF
AltNet Relay $VER for Linux (64-bit)

Ubuntu or Debian: use AltNetRelay_${VER}_amd64.deb instead
  (sudo apt install ./AltNetRelay_${VER}_amd64.deb).

Anything else: run ./install.sh, then open AltNet Relay from your menu.
It needs WebKitGTK 4.1 and GTK 3, and $GLIBC_NEEDED or newer.
EOF
TGZ="$WORK/AltNetRelay-$VER-linux-amd64.tar.gz"
tar -C "$WORK" -czf "$TGZ" "AltNetRelay-$VER-linux-amd64"

mkdir -p "$OUT"
cp "$DEB" "$TGZ" "$OUT/"
echo "==> done"
ls -la "$OUT/$(basename "$DEB")" "$OUT/$(basename "$TGZ")"
dpkg-deb -I "$DEB" | sed -n '2,12p'
