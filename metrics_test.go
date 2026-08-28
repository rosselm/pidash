package main

import (
	"strings"
	"testing"
)

func TestParseCPUTimes(t *testing.T) {
	const stat = `cpu  100 10 40 800 20 0 5 0 0 0
cpu0 50 5 20 400 10 0 2 0 0 0
cpu1 50 5 20 400 10 0 3 0 0 0
intr 12345
ctxt 999
`
	agg, cores := parseCPUTimes(strings.NewReader(stat))
	if want := uint64(975); agg.total != want {
		t.Errorf("agg.total = %d, want %d", agg.total, want)
	}
	// busy excludes both idle (800) and iowait (20).
	if want := uint64(155); agg.busy != want {
		t.Errorf("agg.busy = %d, want %d", agg.busy, want)
	}
	if want := uint64(110); agg.user != want { // user + nice
		t.Errorf("agg.user = %d, want %d", agg.user, want)
	}
	if len(cores) != 2 {
		t.Fatalf("got %d cores, want 2", len(cores))
	}
	if cores[0].total == 0 || cores[1].total == 0 {
		t.Error("per-core totals not populated")
	}
}

func TestParseCPUTimesStopsAtNonCPULines(t *testing.T) {
	// "intr" must not be mistaken for a core, or every host would report an
	// extra phantom CPU.
	_, cores := parseCPUTimes(strings.NewReader("cpu 1 1 1 1 1 1 1 1\ncpu0 1 1 1 1 1 1 1 1\nintr 5 5 5 5 5 5 5 5\n"))
	if len(cores) != 1 {
		t.Errorf("got %d cores, want 1", len(cores))
	}
}

func TestPctBusy(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur cpuTimes
		want      float64
	}{
		{"half busy", cpuTimes{total: 0, busy: 0}, cpuTimes{total: 200, busy: 100}, 50},
		{"idle", cpuTimes{total: 0, busy: 0}, cpuTimes{total: 200, busy: 0}, 0},
		{"pegged", cpuTimes{total: 0, busy: 0}, cpuTimes{total: 200, busy: 200}, 100},
		{"no elapsed time", cpuTimes{total: 100, busy: 50}, cpuTimes{total: 100, busy: 50}, 0},
		{"counter reset clamps", cpuTimes{total: 500, busy: 400}, cpuTimes{total: 600, busy: 900}, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pctBusy(tc.cur, tc.prev); got != tc.want {
				t.Errorf("pctBusy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	const meminfo = `MemTotal:        3888056 kB
MemFree:          415948 kB
MemAvailable:    2682820 kB
Buffers:          157236 kB
Cached:          2076996 kB
SReclaimable:      50000 kB
SwapTotal:        524288 kB
SwapFree:         500000 kB
`
	m := parseMeminfo(strings.NewReader(meminfo))
	if want := uint64(3888056 * 1024); m.Total != want {
		t.Errorf("Total = %d, want %d", m.Total, want)
	}
	// Used is derived from MemAvailable, not MemFree: page cache is available.
	if want := uint64((3888056 - 2682820) * 1024); m.Used != want {
		t.Errorf("Used = %d, want %d", m.Used, want)
	}
	if want := uint64((2076996 + 50000) * 1024); m.Cached != want {
		t.Errorf("Cached = %d, want %d (SReclaimable must be included)", m.Cached, want)
	}
	if want := uint64((524288 - 500000) * 1024); m.SwapUsed != want {
		t.Errorf("SwapUsed = %d, want %d", m.SwapUsed, want)
	}
	if m.Pct < 30 || m.Pct > 32 {
		t.Errorf("Pct = %v, want ~31", m.Pct)
	}
}

func TestParseMeminfoNoSwap(t *testing.T) {
	m := parseMeminfo(strings.NewReader("MemTotal: 1000 kB\nMemAvailable: 500 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"))
	if m.SwapPct != 0 {
		t.Errorf("SwapPct = %v, want 0 when there is no swap", m.SwapPct)
	}
}

func TestParseNetDev(t *testing.T) {
	const dev = `Inter-|   Receive                     |  Transmit
 face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed
    lo: 100 1 0 0 0 0 0 0 100 1 0 0 0 0 0 0
  eth0: 200 2 1 3 0 0 0 0 400 4 5 0 0 0 0 0
 wlan0: 600 6 0 0 0 0 0 0 800 8 0 0 0 0 0 0
veth123: 900 9 0 0 0 0 0 0 900 9 0 0 0 0 0 0
`
	got := parseNetDev(strings.NewReader(dev))
	if _, ok := got["lo"]; ok {
		t.Error("loopback should be excluded")
	}
	if _, ok := got["veth123"]; ok {
		t.Error("docker veth pairs should be excluded")
	}
	if len(got) != 2 {
		t.Fatalf("got %d interfaces, want 2: %v", len(got), got)
	}
	eth := got["eth0"]
	if eth.rx != 200 || eth.tx != 400 || eth.rxErr != 1 || eth.rxDrop != 3 || eth.txErr != 5 {
		t.Errorf("eth0 = %+v", eth)
	}
}

func TestSelectMountsDeduplicatesByDevice(t *testing.T) {
	// Regression: PrivateTmp=yes gives the service private bind mounts of the
	// root device, which were reported as three separate 111 GB filesystems.
	const mounts = `/dev/mmcblk0p2 / ext4 rw,relatime 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/mmcblk0p1 /boot/firmware vfat rw 0 0
/dev/mmcblk0p2 /tmp ext4 rw 0 0
/dev/mmcblk0p2 /var/tmp ext4 rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
`
	got := selectMounts(strings.NewReader(mounts))
	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got), got)
	}
	if got[0].mount != "/" || got[1].mount != "/boot/firmware" {
		t.Errorf("got %+v, want / and /boot/firmware", got)
	}
}

func TestSelectMountsKeepsShortestPathRegardlessOfOrder(t *testing.T) {
	const mounts = `/dev/sda1 /var/tmp ext4 rw 0 0
/dev/sda1 / ext4 rw 0 0
`
	got := selectMounts(strings.NewReader(mounts))
	if len(got) != 1 || got[0].mount != "/" {
		t.Errorf("got %+v, want the shortest path (/) to win", got)
	}
}

func TestSelectMountsEscapedSpaces(t *testing.T) {
	got := selectMounts(strings.NewReader(`/dev/sdb1 /mnt/my\040disk ext4 rw 0 0` + "\n"))
	if len(got) != 1 || got[0].mount != "/mnt/my disk" {
		t.Errorf("got %+v, want the \\040 escape decoded", got)
	}
}

func TestDecodeThrottle(t *testing.T) {
	tests := []struct {
		name              string
		word              uint64
		wantNow, wantEver bool
		check             func(*testing.T, []Flag)
	}{
		{"all clear", 0x0, false, false, nil},
		{
			// What this board actually reports: nothing active, but
			// under-voltage and throttling have both happened since boot.
			name: "0x50000 past only", word: 0x50000, wantNow: false, wantEver: true,
			check: func(t *testing.T, f []Flag) {
				if !f[0].EverOn || f[0].Now {
					t.Errorf("under-voltage = %+v, want ever-but-not-now", f[0])
				}
				if !f[2].EverOn || f[2].Now {
					t.Errorf("throttled = %+v, want ever-but-not-now", f[2])
				}
				if f[1].EverOn || f[3].EverOn {
					t.Error("arm-capped and soft-temp should be clear")
				}
			},
		},
		{
			name: "actively under-voltage", word: 0x1, wantNow: true, wantEver: false,
			check: func(t *testing.T, f []Flag) {
				if !f[0].Now {
					t.Error("bit 0 should set under-voltage now")
				}
			},
		},
		{"everything at once", 0xF000F, true, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, flags, now, ever := decodeThrottle(tc.word)
			if len(flags) != 4 {
				t.Fatalf("got %d flags, want 4", len(flags))
			}
			if now != tc.wantNow || ever != tc.wantEver {
				t.Errorf("now=%v ever=%v, want now=%v ever=%v", now, ever, tc.wantNow, tc.wantEver)
			}
			if !strings.HasPrefix(raw, "0x") {
				t.Errorf("raw = %q, want a 0x-prefixed word", raw)
			}
			if tc.check != nil {
				tc.check(t, flags)
			}
		})
	}
}

func TestParseThrottleHex(t *testing.T) {
	tests := []struct {
		in     string
		want   uint64
		wantOK bool
	}{
		{"throttled=0x50000\n", 0x50000, true},
		{"throttled=0x0", 0, true},
		{"throttled=0xF000F", 0xF000F, true},
		{"garbage", 0, false},
		{"throttled=0xZZZ", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseThrottleHex(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseThrottleHex(%q) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestParseProcStat(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantName    string
		wantJiffies uint64
		wantOK      bool
	}{
		{
			name: "ordinary", wantName: "bash", wantJiffies: 30, wantOK: true,
			in: "42 (bash) S 1 42 42 0 -1 4194304 100 200 0 0 10 20 0 0 20 0 1 0 900 1000 50 18446744073709551615",
		},
		{
			// comm is bounded by parens and may contain both spaces and parens,
			// which is why the split is on the LAST ')'.
			name: "comm with spaces and parens", wantName: "my (weird) proc", wantJiffies: 7, wantOK: true,
			in: "9 (my (weird) proc) S 1 9 9 0 -1 0 1 2 0 0 3 4 0 0 20 0 1 0 5 6 7 8",
		},
		{"truncated", "1 (init) S 1 2 3", "", 0, false},
		{"no parens", "1 init S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22", "", 0, false},
		{"empty", "", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcStat([]byte(tc.in))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.name != tc.wantName || got.jiffies != tc.wantJiffies {
				t.Errorf("got %+v, want name=%q jiffies=%d", got, tc.wantName, tc.wantJiffies)
			}
		})
	}
}

func TestParseCmdline(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"trailing NUL", "nginx\x00-g\x00daemon off;\x00", []string{"nginx", "-g", "daemon off;"}},
		{"no trailing NUL", "sleep\x0060", []string{"sleep", "60"}},
		{"single arg", "systemd\x00", []string{"systemd"}},
		{"kernel thread has none", "", nil},
		{"only NULs", "\x00\x00", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCmdline([]byte(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("arg %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPerSecond(t *testing.T) {
	tests := []struct {
		name      string
		cur, prev uint64
		elapsed   float64
		want      float64
	}{
		{"steady", 2000, 1000, 1, 1000},
		{"over two seconds", 2000, 1000, 2, 500},
		{"counter wrapped", 5, 4_000_000_000, 1, 0},
		{"no time passed", 2000, 1000, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := perSecond(tc.cur, tc.prev, tc.elapsed); got != tc.want {
				t.Errorf("perSecond = %v, want %v", got, tc.want)
			}
		})
	}
}
