#!/usr/bin/env bash
# Build pidash and install it as a systemd service on this Pi.
#
# Re-runnable: rebuilds, replaces the binary, restarts the unit.
set -euo pipefail

cd "$(dirname "$0")"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:-8090}"

command -v "$GO" >/dev/null || { echo "go not found at $GO — set GO=/path/to/go" >&2; exit 1; }

echo "==> building"
"$GO" build -trimpath -ldflags='-s -w' -o pidash .

echo "==> installing /usr/local/bin/pidash"
sudo install -m 0755 pidash /usr/local/bin/pidash

echo "==> installing systemd unit"
sudo install -m 0644 pidash.service /etc/systemd/system/pidash.service
sudo systemctl daemon-reload
sudo systemctl enable --now pidash

sleep 2
systemctl --no-pager --lines=0 status pidash || true

ip=$(hostname -I | awk '{print $1}')
echo
echo "==> pidash is up at http://${ip}:${PORT}/"
