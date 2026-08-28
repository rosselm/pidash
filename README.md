# pidash

[![ci](https://github.com/rosselm/pidash/actions/workflows/ci.yml/badge.svg)](https://github.com/rosselm/pidash/actions/workflows/ci.yml)

A live metrics dashboard for this Raspberry Pi. Go backend, no dependencies,
frontend embedded in the binary.

```
http://<pi-address>:8090/
```

`install.sh` prints the exact URL when it finishes.

## Download

Grab a prebuilt binary from [Releases](https://github.com/rosselm/pidash/releases)
— no Go toolchain needed, and no runtime dependencies, since the frontend is
embedded in the binary:

```bash
curl -LO https://github.com/rosselm/pidash/releases/latest/download/pidash-linux-arm64
chmod +x pidash-linux-arm64
./pidash-linux-arm64 -addr :8090
```

`pidash-linux-arm64` covers a Pi 3/4/5 on 64-bit Raspberry Pi OS; `armv7` and
`armv6` builds cover 32-bit installs and the Zero. To run it as a service
instead, clone and use [`install.sh`](install.sh) — see [Install](#install).

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
| Journal drawer (`J`) | one shared `journalctl -f -o json` |

Throttle flags are reported in both tenses: the low bits of the firmware word are
"right now", the same bits 16 places up are "since boot". This board currently
reads `0x50000` — under-voltage and throttling **have occurred**, nothing active.
That is a power-supply signal worth keeping visible, which is why a past-only
flag still lights the panel amber.

## The journal drawer

The journal is not a card. It lives in a drawer fixed to the bottom of the
viewport, because a log panel at the end of a long grid is only reachable by
scrolling past everything else — and the moment you actually want logs is the
moment you are staring at a spike somewhere further up the page.

- **`J`** (or backtick) toggles it, **`Esc`** closes it.
- Drag its top edge to resize; open/closed and height are remembered.
- It follows the newest line by default and re-arms as soon as you scroll back
  down. **`↓ latest`** appears while you are scrolled away.
- While the drawer is shut, the `journal` button in the top bar counts anything
  at warning priority or worse, so a failure still announces itself.

500 lines stay searchable in the buffer; 300 are rendered. New lines are
appended rather than re-rendering the list, so the arrival animation fires only
on genuinely new rows.

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

The service runs as a login user rather than a dedicated service account,
because it needs three group memberships: `docker` (the socket), `video`
(`/dev/vchiq`, for `vcgencmd`) and `adm` (the journal). `install.sh` sets
`User=` to whoever runs it and warns about any of the three that are missing.
The unit is otherwise read-only — `ProtectSystem=strict`, `ProtectHome=read-only`, `NoNewPrivileges`.

Two hardening settings are deliberately *not* tightened further:

- `PrivateDevices` stays off. Turning it on hides `/dev/vchiq`, which silently
  removes SoC temperature, core voltage and the throttle flags.
- `RestrictAddressFamilies` includes `AF_NETLINK`. Go enumerates interface
  addresses over netlink; without it every interface renders with a blank IP.

## Flags

```
-addr           :8090                    listen address
-interval       1s                       sampling interval
-proc-interval  3s                       how often to walk /proc for the process table
-top            8                        processes in the top table
-expose-cmdline false                    publish full process command lines
-units          otelcol-contrib,pi-temp-exporter,docker,ssh
-log-units      otelcol-contrib,pi-temp-exporter   (empty = whole journal)
-docker-sock    /var/run/docker.sock     empty to disable the panel
-version        print version and exit
```

`-proc-interval` is separate from `-interval` because walking every
`/proc/<pid>/stat` is by far the most expensive thing done per tick, and a top-N
table does not need to move as fast as a gauge. Measured on an idle Pi 4 with
~190 processes, over two 60-second windows:

| `-proc-interval` | CPU used in 60s | share of one core |
|---|---|---|
| `1s` | 1.24 s | 2.07% |
| `3s` (default) | 0.89 s | 1.48% |

A monitoring tool that perturbs what it measures is a problem, so this is worth
re-checking after a change: `systemctl show pidash -p CPUUsageNSec` twice, a
minute apart, is enough.

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

## Security

**There is no authentication.** Anyone who can reach the port can read every
metric. That is a deliberate fit for a trusted home LAN, but it has one
consequence worth stating plainly:

`/proc/<pid>/cmdline` routinely contains credentials — `--token=`, `--password=`,
`--api-key=` — passed as flags. On the machine this was written for, 2 of 190
readable command lines matched that pattern. So **only `argv[0]` is published**:
the executable path, never the arguments. `-expose-cmdline` opts back in to full
command lines and logs a warning at startup.

If the network is not trusted, bind to loopback and reach it over an SSH tunnel:

```bash
pidash -addr 127.0.0.1:8090
ssh -N -L 8090:127.0.0.1:8090 pi@raspberrypi     # from the client
```

The service itself needs no privileges beyond the three groups in the unit, and
never writes anything: `ProtectSystem=strict`, `ProtectHome=read-only`,
`NoNewPrivileges`.

## Tests

```bash
go test ./...          # parsers, decoders, and the sampler's ranking
go test -race ./...    # note: does not run on a Pi, see below
```

The suite covers the pure functions — `/proc/stat`, `/proc/meminfo`,
`/proc/net/dev` and `/proc/<pid>/stat` parsing, the throttle bit-word, journald's
two MESSAGE encodings, Docker's CPU-percent and page-cache arithmetic,
`systemctl show` records, and process ranking — at 90–100% each. Two of them are
regression tests for bugs that actually shipped: filesystems deduplicating by
device, and `argv` never being published by default.

`-race` aborts on this board with `ThreadSanitizer: unsupported VMA range` (the
Pi kernel uses a 39-bit address space, TSan wants 48). CI runs it on amd64.

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

## Checking the UI

pidash is a visual program, and for several commits it was verified only by
reading its own source — which is how a table whose numeric columns collided,
and a Storage panel listing the root filesystem three times, both shipped.
[`tools/uicheck.py`](tools/uicheck.py) drives the running dashboard in headless
Chromium and fails on the things source review cannot see:

```bash
sudo apt install chromium
python3 -m venv .venv && .venv/bin/pip install playwright
.venv/bin/python tools/uicheck.py --url http://localhost:8090 --out /tmp/shots
```

It asserts no console errors, no horizontal overflow at 1680/1280/980/820/420px,
no card clipping its own content, no table cell wrapping, that the drawer opens
on `J`, and that the per-core bars, filesystems and four throttle flags all
render. It also writes screenshots of the dashboard and the open drawer.

Playwright ships no arm64 browser build, so it drives the system Chromium and
downloads nothing.

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

## Licence

MIT — see [LICENSE](LICENSE).
