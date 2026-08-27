#!/usr/bin/env bash
# Build pidash and install it as a systemd service on this Pi.
#
# Re-runnable: rebuilds, replaces the binary, restarts the unit.
set -euo pipefail

cd "$(dirname "$0")"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:-8090}"
RUN_USER="${RUN_USER:-${SUDO_USER:-$USER}}"

command -v "$GO" >/dev/null || { echo "go not found at $GO — set GO=/path/to/go" >&2; exit 1; }

echo "==> building"
"$GO" build -trimpath -ldflags='-s -w' -o pidash .

echo "==> installing /usr/local/bin/pidash"
sudo install -m 0755 pidash /usr/local/bin/pidash

echo "==> checking group membership for ${RUN_USER}"
for g in docker video adm; do
  if id -nG "$RUN_USER" | tr ' ' '\n' | grep -qx "$g"; then
    continue
  fi
  case $g in
    docker) echo "    ! not in 'docker' — the container panel will be empty" ;;
    video)  echo "    ! not in 'video' — no temperature, voltage or throttle flags" ;;
    adm)    echo "    ! not in 'adm' — the journal panel will be empty" ;;
  esac
  echo "      fix: sudo usermod -aG $g $RUN_USER  (then log out and back in)"
done

echo "==> installing systemd unit (User=${RUN_USER})"
sed "s/^User=.*/User=${RUN_USER}/; s/^Group=.*/Group=${RUN_USER}/" pidash.service |
  sudo tee /etc/systemd/system/pidash.service >/dev/null
sudo chmod 0644 /etc/systemd/system/pidash.service
sudo systemctl daemon-reload
sudo systemctl enable pidash
# restart, not `enable --now`: on a re-run the unit is already active, and
# --now would leave the old process running against the new binary.
sudo systemctl restart pidash

sleep 2
systemctl --no-pager --lines=0 status pidash || true

ip=$(hostname -I | awk '{print $1}')
echo
echo "==> pidash is up at http://${ip}:${PORT}/"
