# pidash

A live metrics dashboard for this Raspberry Pi. Go backend, no dependencies,
frontend embedded in the binary.

```
http://192.168.1.101:8090/
```

## Why it exists separately from the OTel pipeline

The collector on this host pushes to the Mac gateway, so *the moment the network
partitions or the gateway goes down, the Grafana view goes dark* — which is
exactly when you want to look at the Pi. pidash reads `/proc`, `/sys` and the
VideoCore firmware directly and serves its own UI, so it stays up when the
telemetry path does not. It shares no code, no config and no port with
`otelcol-contrib`.

## What it shows

| Panel | Source |
|---|---|
| Temp / CPU / memory gauges | `/sys/class/thermal`, `/proc/stat`, `/proc/meminfo` |
| Processor history, per-core bars | `/proc/stat` deltas, `/sys/.../cpufreq` |
| Thermal & throttle flags | `vcgencmd get_throttled` / `measure_volts core` |
| Memory breakdown, load | `/proc/meminfo`, `/proc/loadavg` |
| Storage | `/proc/mounts` + `statfs(2)` |
| Network | `/proc/net/dev` deltas, `/sys/class/net/*/operstate` |
| Containers | Docker Engine API over `/var/run/docker.sock` |
| Services | `systemctl show` |
| Top processes | `/proc/<pid>/{stat,statm,cmdline,status}` |
| Journal tail | one shared `journalctl -f -o json` |

Throttle flags are reported in both tenses: the low bits of the firmware word are
"right now", the same bits 16 places up are "since boot". This board currently
reads `0x50000` — under-voltage and throttling **have occurred**, nothing active.
That is a power-supply signal worth keeping visible, which is why a past-only
flag still lights the panel amber.

## Rearranging the dashboard

Every card is movable and resizable, and the arrangement is remembered per
browser:

- **Drag a card by its header** to move it. A dashed placeholder shows where it
  will land.
- **Drag the grip in the bottom-right corner** to resize: horizontally it snaps
  to the 12-column grid (3–12 columns), vertically it sets a pixel height. A
  readout shows `span/12 · height` while you drag.
- **`reset layout`** in the top bar restores the defaults. It only appears once
  you have actually changed something.

Layout lives in `localStorage` under `pidash.layout.v1` — order, spans and
heights, nothing else. It is per-browser, not shared between devices, and the
server never sees it. Charts repaint through a `ResizeObserver`, so a resized
card redraws its chart at the new size rather than stretching a stale bitmap.

Below 1280px the grid reflows on its own and saved spans give way to the
responsive rules; the three gauges stay on one row as far down as they fit.

## Install

```bash
./install.sh          # build, install to /usr/local/bin, enable the unit
systemctl status pidash
journalctl -u pidash -f
```

Re-run `install.sh` after any change; it rebuilds and restarts in place.

The service runs as `rosselm` rather than a dedicated user, because it needs
three group memberships that user already has: `docker` (the socket), `video`
(`/dev/vchiq`, for `vcgencmd`) and `adm` (the journal). The unit is otherwise
read-only — `ProtectSystem=strict`, `ProtectHome=read-only`, `NoNewPrivileges`.

Two hardening settings are deliberately *not* tightened further:

- `PrivateDevices` stays off. Turning it on hides `/dev/vchiq`, which silently
  removes SoC temperature, core voltage and the throttle flags.
- `RestrictAddressFamilies` includes `AF_NETLINK`. Go enumerates interface
  addresses over netlink; without it every interface renders with a blank IP.

## Flags

```
-addr        :8090                       listen address
-interval    1s                          sampling interval
-top         8                            processes in the top table
-units       otelcol-contrib,pi-temp-exporter,docker,ssh
-log-units   otelcol-contrib,pi-temp-exporter    (empty = whole journal)
-docker-sock /var/run/docker.sock         empty to disable the panel
```

## API

Three endpoints, all useful from the shell:

```bash
curl -s localhost:8090/api/snapshot | jq .     # one pretty-printed sample
curl -sN localhost:8090/api/stream             # SSE, one snapshot per interval
curl -sN localhost:8090/api/logs               # SSE, journal lines
curl -s localhost:8090/healthz
```

`/api/stream` and `/api/logs` are Server-Sent Events. One sampler goroutine
feeds every connected browser: opening ten tabs does not multiply the `/proc`
traffic, and all tabs agree on the same CPU percentages.

## Known limitation on this board: cgroup memory accounting is off

`/sys/fs/cgroup/cgroup.controllers` lists `cpuset cpu io pids` — no `memory`.
Raspberry Pi OS ships that way. The consequence is that Docker's stats endpoint
returns an empty `memory_stats` and systemd reports `MemoryCurrent=[not set]`,
so neither container nor service memory is directly readable.

pidash works around it by summing the RSS of every process in the cgroup
(`cgroup.procs` is present regardless of controller). That over-counts shared
pages slightly, but it is a real number rather than a dash.

To get exact accounting instead, append to `/boot/firmware/cmdline.txt` (one
line, space-separated) and reboot:

```
cgroup_memory=1 cgroup_enable=memory
```

That costs a small amount of kernel memory overhead and **requires a reboot**,
so it is left as a deliberate choice rather than done for you.

## Layout

```
main.go       flags, HTTP routes, SSE plumbing
sampler.go    the single sampling loop; fans one snapshot out to all viewers
metrics.go    /proc, /sys and vcgencmd readers
docker.go     Engine API over the unix socket
units.go      systemd unit state
logs.go       shared journal tail with a replay backlog
web/          index.html, style.css, app.js — embedded via go:embed
```

Standard library only, matching the constraint the rest of this host's tooling
works under. The frontend has no build step and no external assets: no CDN, no
web fonts, so it loads fine on a LAN with no internet route.
