#!/bin/sh
# Remove the BackTrackarr service. Your data (/var/lib/backtrackarr) and
# settings (/etc/backtrackarr) are kept unless you pass --purge.
set -eu
[ "$(id -u)" -eq 0 ] || { echo "Run as root (sudo)." >&2; exit 1; }
systemctl disable --now backtrackarr 2>/dev/null || true
rm -f /etc/systemd/system/backtrackarr.service /usr/local/bin/backtrackarr
systemctl daemon-reload
if [ "${1:-}" = "--purge" ]; then
  rm -rf /var/lib/backtrackarr /etc/backtrackarr
  userdel backtrackarr 2>/dev/null || true
  echo "Removed BackTrackarr and all its data."
else
  echo "Removed BackTrackarr. Data kept in /var/lib/backtrackarr, settings in /etc/backtrackarr."
fi
