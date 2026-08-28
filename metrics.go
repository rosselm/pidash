// Metric collection straight off /proc, /sys and vcgencmd.
//
// Nothing here depends on the OTel collector: the dashboard has to stay up
// precisely when the telemetry pipeline is the thing that fell over.
//
// Standard library only, matching the constraint the rest of this host's
// tooling works under.
package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// userHZ is the kernel's clock tick. Linux fixes this at 100 for userspace
// regardless of CONFIG_HZ, so /proc jiffies are always centiseconds.
const userHZ = 100

type Host struct {
	Hostname string `json:"hostname"`
	Model    string `json:"model"`
	Kernel   string `json:"kernel"`
	Uptime   int64  `json:"uptime"`
	Cores    int    `json:"cores"`
	Procs    int    `json:"procs"`
	Running  int    `json:"running"`
}

type CPU struct {
	Total    float64   `json:"total"`
	PerCore  []float64 `json:"perCore"`
	User     float64   `json:"user"`
	System   float64   `json:"system"`
	IOWait   float64   `json:"iowait"`
	FreqMHz  int       `json:"freqMHz"`
	MaxMHz   int       `json:"maxMHz"`
	MinMHz   int       `json:"minMHz"`
	Load1    float64   `json:"load1"`
	Load5    float64   `json:"load5"`
	Load15   float64   `json:"load15"`
	Governor string    `json:"governor"`
}

type Mem struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Cached    uint64  `json:"cached"`
	Buffers   uint64  `json:"buffers"`
	SwapTotal uint64  `json:"swapTotal"`
	SwapUsed  uint64  `json:"swapUsed"`
	Pct       float64 `json:"pct"`
	SwapPct   float64 `json:"swapPct"`
}

type Flag struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Now    bool   `json:"now"`
	EverOn bool   `json:"ever"`
}

type Thermal struct {
	TempC     float64 `json:"tempC"`
	VoltsCore float64 `json:"voltsCore"`
	Raw       string  `json:"raw"`
	Flags     []Flag  `json:"flags"`
	AnyNow    bool    `json:"anyNow"`
	AnyEver   bool    `json:"anyEver"`
}

type Disk struct {
	Device string  `json:"device"`
	Mount  string  `json:"mount"`
	FSType string  `json:"fstype"`
	Total  uint64  `json:"total"`
	Used   uint64  `json:"used"`
	Free   uint64  `json:"free"`
	Pct    float64 `json:"pct"`
}

type Net struct {
	Name    string  `json:"name"`
	RxBps   float64 `json:"rxBps"`
	TxBps   float64 `json:"txBps"`
	RxTotal uint64  `json:"rxTotal"`
	TxTotal uint64  `json:"txTotal"`
	RxErr   uint64  `json:"rxErr"`
	TxErr   uint64  `json:"txErr"`
	RxDrop  uint64  `json:"rxDrop"`
	Addr    string  `json:"addr"`
	Up      bool    `json:"up"`
}

type Proc struct {
	PID  int     `json:"pid"`
	Name string  `json:"name"`
	Cmd  string  `json:"cmd"`
	User string  `json:"user"`
	CPU  float64 `json:"cpu"`
	RSS  uint64  `json:"rss"`
	MemP float64 `json:"memPct"`
}

// Snapshot is the whole dashboard state for one instant. It is what gets
// marshalled onto the SSE stream, so field names here are the frontend's API.
type Snapshot struct {
	TS         int64       `json:"ts"`
	Host       Host        `json:"host"`
	CPU        CPU         `json:"cpu"`
	Mem        Mem         `json:"mem"`
	Thermal    Thermal     `json:"thermal"`
	Disks      []Disk      `json:"disks"`
	Nets       []Net       `json:"nets"`
	Procs      []Proc      `json:"procs"`
	Containers []Container `json:"containers"`
	Units      []Unit      `json:"units"`
}

// ---------- cpu ----------

type cpuTimes struct {
	total, busy, user, system, iowait uint64
}

func readCPUTimes() (agg cpuTimes, cores []cpuTimes) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()
	return parseCPUTimes(f)
}

// parseCPUTimes reads the aggregate and per-core lines of /proc/stat. Split out
// from the file access so it can be exercised against fixtures.
func parseCPUTimes(r io.Reader) (agg cpuTimes, cores []cpuTimes) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			break // the cpu lines are always first
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		var vals []uint64
		for _, f := range fields[1:] {
			n, _ := strconv.ParseUint(f, 10, 64)
			vals = append(vals, n)
		}
		var t cpuTimes
		for _, v := range vals {
			t.total += v
		}
		t.user = vals[0] + vals[1] // user + nice
		t.system = vals[2]
		t.iowait = vals[4]
		t.busy = t.total - vals[3] - vals[4] // minus idle and iowait
		if fields[0] == "cpu" {
			agg = t
		} else {
			cores = append(cores, t)
		}
	}
	return
}

func pctBusy(cur, prev cpuTimes) float64 {
	dt := float64(cur.total - prev.total)
	if dt <= 0 {
		return 0
	}
	return clampPct(100 * float64(cur.busy-prev.busy) / dt)
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func readLoad() (l1, l5, l15 float64, running, total int) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) < 4 {
		return
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	l5, _ = strconv.ParseFloat(f[1], 64)
	l15, _ = strconv.ParseFloat(f[2], 64)
	if r, t, ok := strings.Cut(f[3], "/"); ok {
		running, _ = strconv.Atoi(r)
		total, _ = strconv.Atoi(t)
	}
	return
}

func readIntFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(string(b)), "\x00")
}

// ---------- memory ----------

func readMem() Mem {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Mem{}
	}
	defer f.Close()
	return parseMeminfo(f)
}

func parseMeminfo(r io.Reader) Mem {
	vals := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(fields[0], 10, 64)
		vals[key] = n * 1024 // meminfo is in kB
	}
	m := Mem{
		Total:     vals["MemTotal"],
		Available: vals["MemAvailable"],
		Cached:    vals["Cached"] + vals["SReclaimable"],
		Buffers:   vals["Buffers"],
		SwapTotal: vals["SwapTotal"],
	}
	m.Used = m.Total - m.Available
	m.SwapUsed = m.SwapTotal - vals["SwapFree"]
	if m.Total > 0 {
		m.Pct = 100 * float64(m.Used) / float64(m.Total)
	}
	if m.SwapTotal > 0 {
		m.SwapPct = 100 * float64(m.SwapUsed) / float64(m.SwapTotal)
	}
	return m
}

// ---------- thermal / throttle ----------

// throttleBits mirrors the firmware's get_throttled word. Low bits are "right
// now"; the same flag 16 bits up is "has happened since boot" — a past-only
// flag is still real signal, so both are reported.
var throttleBits = []struct {
	bit   uint
	name  string
	label string
}{
	{0, "undervoltage", "Under-voltage"},
	{1, "arm_freq_capped", "ARM frequency capped"},
	{2, "throttled", "Currently throttled"},
	{3, "soft_temp_limit", "Soft temperature limit"},
}

func readThermal() Thermal {
	t := Thermal{TempC: float64(readIntFile("/sys/class/thermal/thermal_zone0/temp")) / 1000}

	if out, err := exec.Command("vcgencmd", "measure_volts", "core").Output(); err == nil {
		s := strings.TrimSpace(string(out))
		s = strings.TrimPrefix(s, "volt=")
		s = strings.TrimSuffix(s, "V")
		t.VoltsCore, _ = strconv.ParseFloat(s, 64)
	}

	word, ok := readThrottleWord()
	if !ok {
		return t
	}
	t.Raw, t.Flags, t.AnyNow, t.AnyEver = decodeThrottle(word)
	return t
}

// decodeThrottle expands the firmware's get_throttled word. The low bits are
// "right now"; the same flag sixteen places up is "has happened since boot".
func decodeThrottle(word uint64) (raw string, flags []Flag, anyNow, anyEver bool) {
	raw = "0x" + strconv.FormatUint(word, 16)
	for _, b := range throttleBits {
		f := Flag{
			Name:   b.name,
			Label:  b.label,
			Now:    word&(1<<b.bit) != 0,
			EverOn: word&(1<<(b.bit+16)) != 0,
		}
		anyNow = anyNow || f.Now
		anyEver = anyEver || f.EverOn
		flags = append(flags, f)
	}
	return raw, flags, anyNow, anyEver
}

// parseThrottleHex reads vcgencmd's "throttled=0x50000" output.
func parseThrottleHex(s string) (uint64, bool) {
	_, hex, ok := strings.Cut(strings.TrimSpace(s), "=0x")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(hex, 16, 64)
	return n, err == nil
}

// readThrottleWord prefers the firmware directly, falling back to the sysfs
// node exposed by newer kernels when vcgencmd is unavailable.
func readThrottleWord() (uint64, bool) {
	if out, err := exec.Command("vcgencmd", "get_throttled").Output(); err == nil {
		if n, ok := parseThrottleHex(string(out)); ok {
			return n, true
		}
	}
	for _, p := range []string{
		"/sys/devices/platform/soc/soc:firmware/get_throttled",
		"/sys/class/hwmon/hwmon0/device/get_throttled",
	} {
		if s := readTrimmed(p); s != "" {
			if n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// ---------- disks ----------

// realFS is the set of mount types worth showing. Everything else on a Pi is
// kernel bookkeeping (tmpfs, cgroup, overlay from docker) and only adds noise.
var realFS = map[string]bool{
	"ext4": true, "ext3": true, "ext2": true, "vfat": true,
	"xfs": true, "btrfs": true, "f2fs": true, "exfat": true, "ntfs3": true,
}

// mountRef is one candidate filesystem, before statfs is asked about it.
type mountRef struct{ device, mount, fstype string }

// selectMounts picks the filesystems worth showing from /proc/mounts.
//
// One filesystem can appear at several mount points: bind mounts, and the
// private /tmp and /var/tmp systemd hands a service running under
// PrivateTmp=yes. Keeping the shortest path per device stops the root
// filesystem being reported three times over under three different names.
func selectMounts(r io.Reader) []mountRef {
	byDevice := map[string]mountRef{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !realFS[fields[2]] {
			continue
		}
		m := mountRef{
			device: fields[0],
			mount:  strings.ReplaceAll(fields[1], `\040`, " "),
			fstype: fields[2],
		}
		if prev, seen := byDevice[m.device]; seen && len(prev.mount) <= len(m.mount) {
			continue
		}
		byDevice[m.device] = m
	}
	out := make([]mountRef, 0, len(byDevice))
	for _, m := range byDevice {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mount < out[j].mount })
	return out
}

func readDisks() []Disk {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []Disk
	for _, m := range selectMounts(f) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.mount, &st); err != nil {
			continue
		}
		bs := uint64(st.Bsize)
		d := Disk{
			Device: m.device,
			Mount:  m.mount,
			FSType: m.fstype,
			Total:  st.Blocks * bs,
			Free:   st.Bavail * bs,
		}
		// Match df: capacity excludes root-reserved blocks.
		d.Used = (st.Blocks - st.Bfree) * bs
		if denom := d.Used + d.Free; denom > 0 {
			d.Pct = 100 * float64(d.Used) / float64(denom)
		}
		out = append(out, d)
	}
	return out
}

// ---------- network ----------

type netCounters struct{ rx, tx, rxErr, txErr, rxDrop uint64 }

func readNetCounters() map[string]netCounters {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseNetDev(f)
}

func parseNetDev(r io.Reader) map[string]netCounters {
	out := map[string]netCounters{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") {
			continue
		}
		v := strings.Fields(rest)
		if len(v) < 12 {
			continue
		}
		n := netCounters{}
		n.rx, _ = strconv.ParseUint(v[0], 10, 64)
		n.rxErr, _ = strconv.ParseUint(v[2], 10, 64)
		n.rxDrop, _ = strconv.ParseUint(v[3], 10, 64)
		n.tx, _ = strconv.ParseUint(v[8], 10, 64)
		n.txErr, _ = strconv.ParseUint(v[10], 10, 64)
		out[name] = n
	}
	return out
}

func ifaceAddr(name string) (string, bool) {
	up := readTrimmed("/sys/class/net/"+name+"/operstate") == "up"
	return addrCache.lookup(name), up
}

// addrCache keeps IP lookups off the hot path — addresses change on the order
// of DHCP leases, not seconds.
type ifAddrCache struct {
	mu   sync.Mutex
	at   time.Time
	byIf map[string]string
}

var addrCache = &ifAddrCache{}

func (c *ifAddrCache) lookup(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byIf == nil || time.Since(c.at) > 30*time.Second {
		c.at = time.Now()
		c.byIf = interfaceAddrs()
	}
	return c.byIf[name]
}

// interfaceAddrs maps interface name to its first IPv4 address.
func interfaceAddrs() map[string]string {
	out := map[string]string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			out[i.Name] = ipnet.IP.String()
			break
		}
	}
	return out
}

// ---------- processes ----------

type procTime struct {
	jiffies uint64
	name    string
}

func readProcTimes() map[int]procTime {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make(map[int]procTime, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if pt, ok := parseProcStat(b); ok {
			out[pid] = pt
		}
	}
	return out
}

// parseProcStat pulls the command name and CPU jiffies out of /proc/<pid>/stat.
// comm is wrapped in parens and may itself contain spaces and parens, so the
// split has to be on the LAST ')' rather than on whitespace.
func parseProcStat(b []byte) (procTime, bool) {
	s := string(b)
	lp := strings.IndexByte(s, '(')
	rp := strings.LastIndexByte(s, ')')
	if lp < 0 || rp < lp || rp+2 > len(s) {
		return procTime{}, false
	}
	fields := strings.Fields(s[rp+2:])
	if len(fields) < 22 {
		return procTime{}, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64) // field 14 overall
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return procTime{}, false
	}
	return procTime{jiffies: utime + stime, name: s[lp+1 : rp]}, true
}

func procRSS(pid int) uint64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(os.Getpagesize())
}

func procArgs(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil
	}
	return parseCmdline(b)
}

// parseCmdline splits the NUL-separated argv of /proc/<pid>/cmdline. Splitting
// rather than joining matters: argv[0] alone is what gets published unless full
// command lines are explicitly opted into, and argv[0] may contain spaces.
func parseCmdline(b []byte) []string {
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

func procUser(pid int) string {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return userName(fields[0])
			}
			return ""
		}
	}
	return ""
}

// userName resolves a uid via /etc/passwd. os/user would do it too, but this
// avoids cgo and the result is cached for the process lifetime anyway.
var (
	userOnce sync.Once
	userByID map[string]string
)

func userName(uid string) string {
	userOnce.Do(func() {
		userByID = map[string]string{}
		b, err := os.ReadFile("/etc/passwd")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Split(line, ":")
			if len(f) > 2 {
				userByID[f[2]] = f[0]
			}
		}
	})
	if n, ok := userByID[uid]; ok {
		return n
	}
	return uid
}

// ---------- host ----------

func readHost() Host {
	hn, _ := os.Hostname()
	h := Host{
		Hostname: hn,
		Model:    readTrimmed("/proc/device-tree/model"),
		Kernel:   readTrimmed("/proc/sys/kernel/osrelease"),
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			secs, _ := strconv.ParseFloat(f[0], 64)
			h.Uptime = int64(secs)
		}
	}
	if h.Model == "" {
		// Non-Pi fallback so the dashboard is still usable off-board.
		h.Model = firstCPUModel()
	}
	return h
}

func firstCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok {
			k = strings.TrimSpace(k)
			if k == "Model" || k == "model name" {
				return strings.TrimSpace(v)
			}
		}
	}
	return "unknown"
}

func cpuGovernor() string {
	return readTrimmed("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
}

// curFreqMHz reports the fastest core, which is what "is the SoC boosting"
// actually asks — a single idle core would otherwise hide it.
func curFreqMHz() int {
	paths, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq")
	max := 0
	for _, p := range paths {
		if v := readIntFile(p); v > max {
			max = v
		}
	}
	return max / 1000
}

// ---------- cgroup memory fallback ----------

// cgroupRSS sums the resident set of every process in a cgroup.
//
// It exists because this board boots without the memory controller
// (`cgroup_enable=memory` is absent from cmdline.txt), so both Docker's stats
// endpoint and systemd's MemoryCurrent come back empty. Summing RSS
// double-counts shared pages, but it is the difference between a real number
// and a dash.
func cgroupRSS(dir string) uint64 {
	if dir == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return 0
	}
	var total uint64
	for _, line := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		total += procRSS(pid)
	}
	return total
}

// cgroupMemoryEnabled reports whether the kernel exposes memory accounting at
// all, so callers can skip a fallback that would never be needed.
var cgroupMemoryEnabled = sync.OnceValue(func() bool {
	return strings.Contains(readTrimmed("/sys/fs/cgroup/cgroup.controllers"), "memory")
})

// firstExistingDir returns the first candidate path that exists.
func firstExistingDir(candidates ...string) string {
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}
