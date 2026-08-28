#!/usr/bin/env bash
#
# Reproduce Raspberry Pi under-voltage on demand.
#
# The firmware's sticky "since boot" bits tell you under-voltage happened, but
# not when, how long for, or under what conditions — and they cannot be cleared
# without a reboot. This pins every core while sampling the live bits each
# second, so a marginal supply shows up as a measurement rather than a rumour.
#
#   ./tools/psu-test.sh
#   LOAD=180 ./tools/psu-test.sh          # longer soak
#
# Run it once as a baseline, then again after changing one thing — a different
# wall socket, no extension lead, another PSU — and compare. Changing one
# variable at a time is the whole point.
#
# Exit status: 0 if the rail held, 1 if under-voltage was seen.
set -uo pipefail

BASELINE=${BASELINE:-20}     # seconds of idle before loading
LOAD=${LOAD:-90}             # seconds with every core pinned
COOLDOWN=${COOLDOWN:-20}     # seconds of idle afterwards

command -v vcgencmd >/dev/null || {
  echo "vcgencmd not found — this reads the VideoCore firmware and only runs on a Pi" >&2
  exit 2
}

SAMPLES=$(mktemp); LOAD_PIDS=()
cleanup() {
  # Busy loops must not outlive the script, however it exits.
  [ ${#LOAD_PIDS[@]} -gt 0 ] && kill "${LOAD_PIDS[@]}" 2>/dev/null
  rm -f "$SAMPLES"
}
trap cleanup EXIT INT TERM

sample_for() {                      # phase, seconds
  local phase=$1 secs=$2 i word temp freq
  for ((i = 0; i < secs; i++)); do
    word=$(vcgencmd get_throttled | cut -d= -f2)
    temp=$(vcgencmd measure_temp | tr -dc '0-9.')
    freq=$(( $(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq 2>/dev/null || echo 0) / 1000 ))
    # Decode the bits here rather than in awk: Debian ships mawk, which has
    # neither strtonum() nor and(). Bash understands 0x literals natively.
    printf '%s %d %d %s %s\n' "$phase" "$(( word & 1 ))" "$(( (word & 4) != 0 ))" "$temp" "$freq" >> "$SAMPLES"
    # A live under-voltage bit is the thing being hunted, so say so as it happens.
    if (( (word & 1) != 0 )); then printf '!'; else printf '.'; fi
    sleep 1
  done
  echo
}

echo "pidash psu-test — ${BASELINE}s idle, ${LOAD}s loaded, ${COOLDOWN}s idle"
echo "  '.' = rail ok   '!' = under-voltage active"
echo "  board : $(tr -d '\0' < /proc/device-tree/model 2>/dev/null || echo unknown)"
echo "  at start: $(vcgencmd get_throttled)  $(vcgencmd measure_temp)"
echo

printf '  baseline  '
sample_for baseline "$BASELINE"

printf '  load      '
for ((c = 0; c < $(nproc); c++)); do
  # Pure shell spin: no toolchain needed, and it keeps a core pinned for the
  # whole window rather than in bursts.
  sh -c 'while :; do :; done' & LOAD_PIDS+=($!)
done
sample_for load "$LOAD"
kill "${LOAD_PIDS[@]}" 2>/dev/null; LOAD_PIDS=()

printf '  cooldown  '
sample_for cooldown "$COOLDOWN"

awk '
  { n++; uv = $2 + 0; th = $3 + 0
    tot_uv += uv; tot_th += th
    if (uv) { run++; if (run > longest) longest = run } else run = 0
    if ($1 == "load")     { l_n++; l_uv += uv }
    if ($1 != "load")     { i_n++; i_uv += uv }
    if (tmin == "" || $4 < tmin) tmin = $4
    if ($4 > tmax) tmax = $4
    if (fmin == "" || $5 < fmin) fmin = $5
    if ($5 > fmax) fmax = $5
  }
  END {
    printf "\n  under-voltage : %d of %d seconds (%.0f%%)\n", tot_uv, n, 100*tot_uv/n
    printf "  throttled     : %d of %d seconds\n", tot_th, n
    printf "  longest spell : %d seconds\n", longest
    printf "  while loaded  : %d of %d (%.0f%%)\n", l_uv, l_n, l_n ? 100*l_uv/l_n : 0
    printf "  while idle    : %d of %d (%.0f%%)\n", i_uv, i_n, i_n ? 100*i_uv/i_n : 0
    printf "  temperature   : %.1f -> %.1f C\n", tmin, tmax
    printf "  clock         : %d -> %d MHz\n", fmin, fmax
    print  ""
    if (tot_uv == 0)
      print "  VERDICT: rail held throughout."
    else if (i_uv > 0)
      print "  VERDICT: under-voltage even at idle — suspect the supply or its cable, not the workload."
    else
      print "  VERDICT: under-voltage under load only — the supply cannot hold the rail at full draw."
    # Temperature is reported so a hot board is not mistaken for a weak one:
    # thermal throttling on a Pi 4 starts around 80 C, far above anything a
    # supply problem produces.
    if (tmax >= 80) print "  NOTE: the board also got hot enough to throttle thermally; that is a separate problem."
    exit (tot_uv > 0)
  }
' "$SAMPLES"
