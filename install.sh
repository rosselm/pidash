#!/usr/bin/env bash
# Build pidash and install it as a systemd service on this Pi.
#
# Re-runnable: rebuilds, replaces the binary, restarts the unit.
set -euo pipefail

cd "$(dirname "$0")"
GO="${GO:-/usr/local/go/bin/go}"
PORT="${PORT:-8090}"
RUN_USER="${RUN_USER:-${SUDO_USER:-$USER}}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

command -v "$GO" >/dev/null || { echo "go not found at $GO — set GO=/path/to/go" >&2; exit 1; }

echo "==> building ${VERSION}"
"$GO" build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o pidash .

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

echo
addr=$(awk -F' -addr ' '/^ExecStart=/ {split($2,a," "); print a[1]}' pidash.service)
echo "==> pidash is listening on ${addr:-:$PORT}"
if serve=$(tailscale serve status 2>/dev/null | grep -m1 '^https://'); then
  echo "==> reachable at ${serve%% *}"
elif [ "${addr#127.0.0.1}" != "$addr" ]; then
  # Bound to loopback with nothing in front of it: say so rather than print a
  # LAN URL that will not answer.
  echo "    loopback only — reach it over an SSH tunnel, or put a TLS"
  echo "    terminator in front of it (see 'HTTPS' in the README)"
fi
