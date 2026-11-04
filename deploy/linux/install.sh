#!/bin/sh
# Install or update BackTrackarr as a systemd service.
#   curl -fsSL https://raw.githubusercontent.com/DrSeussFreak/BackTrackarr/main/deploy/linux/install.sh | sudo sh
# Options (environment): VERSION=v0.2.0 (default: latest release), PORT=8787
set -eu

REPO="DrSeussFreak/BackTrackarr"
VERSION="${VERSION:-latest}"
BIN=/usr/local/bin/backtrackarr
CONF_DIR=/etc/backtrackarr
DATA_DIR=/var/lib/backtrackarr

[ "$(id -u)" -eq 0 ] || { echo "Run as root (sudo)." >&2; exit 1; }
command -v systemctl >/dev/null || { echo "systemd is required (or use Docker)." >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv7*|armhf) ARCH=armv7 ;;
  *) echo "Unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$VERSION" = latest ]; then
  URL="https://github.com/$REPO/releases/latest/download/backtrackarr_linux_$ARCH.tar.gz"
else
  URL="https://github.com/$REPO/releases/download/$VERSION/backtrackarr_linux_$ARCH.tar.gz"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
echo "Downloading $URL"
if command -v curl >/dev/null; then curl -fsSL "$URL" -o "$TMP/bt.tar.gz"; else wget -qO "$TMP/bt.tar.gz" "$URL"; fi
tar -xzf "$TMP/bt.tar.gz" -C "$TMP"
SRC="$TMP/backtrackarr_linux_$ARCH"

id backtrackarr >/dev/null 2>&1 || useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin backtrackarr
mkdir -p "$CONF_DIR" "$DATA_DIR"
chown backtrackarr:backtrackarr "$DATA_DIR"
chmod 750 "$DATA_DIR"

if systemctl is-active --quiet backtrackarr; then systemctl stop backtrackarr; fi
install -m 755 "$SRC/backtrackarr" "$BIN"

if [ ! -f "$CONF_DIR/backtrackarr.env" ]; then
  # First install: start from the example settings with Linux paths.
  sed -e "s#^LISTEN=.*##" "$SRC/backtrackarr.env.example" > "$CONF_DIR/backtrackarr.env"
  if [ -n "${PORT:-}" ]; then echo "LISTEN=:$PORT" >> "$CONF_DIR/backtrackarr.env"; fi
  chmod 640 "$CONF_DIR/backtrackarr.env"
  chown root:backtrackarr "$CONF_DIR/backtrackarr.env"
  FIRST=1
fi

install -m 644 "$SRC/backtrackarr.service" /etc/systemd/system/backtrackarr.service
systemctl daemon-reload
systemctl enable --now backtrackarr

echo
echo "BackTrackarr $("$BIN" version 2>/dev/null || true) is running: http://$(hostname -I 2>/dev/null | awk '{print $1}'):${PORT:-8787}"
echo "Settings: $CONF_DIR/backtrackarr.env   Data: $DATA_DIR"
if [ "${FIRST:-0}" = 1 ]; then
  echo "Edit the settings (at least ADMIN_PASSWORD and PLEX_URL/PLEX_TOKEN), then: sudo systemctl restart backtrackarr"
fi
echo "Logs: journalctl -u backtrackarr -f"
