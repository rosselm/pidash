// systemd unit health, via `systemctl show`.
//
// There is no stable D-Bus story without cgo or a third-party library, and
// systemctl's key=value output is a documented, parseable interface.
package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Unit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      string `json:"active"` // active, inactive, failed, activating
	Sub         string `json:"sub"`    // running, dead, exited
	Since       int64  `json:"since"`  // unix seconds, 0 if unknown
	Restarts    int    `json:"restarts"`
	Mem         uint64 `json:"mem"`
	PID         int    `json:"pid"`
}

var unitProps = []string{
	"Id", "Description", "ActiveState", "SubState", "ControlGroup",
	"ActiveEnterTimestampMonotonic", "NRestarts", "MemoryCurrent", "MainPID",
}

// readUnits queries all requested units in a single systemctl invocation.
// Records come back in request order, separated by blank lines.
func readUnits(ctx context.Context, names []string) []Unit {
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"show", "--no-pager", "--property=" + strings.Join(unitProps, ",")}, names...)
	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil && len(out) == 0 {
		return nil
	}

	bootUnix := bootTimeUnix()
	var units []Unit
	cur := map[string]string{}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		u := Unit{
			Name:        strings.TrimSuffix(cur["Id"], ".service"),
			Description: cur["Description"],
			Active:      cur["ActiveState"],
			Sub:         cur["SubState"],
		}
		u.Restarts, _ = strconv.Atoi(cur["NRestarts"])
		u.PID, _ = strconv.Atoi(cur["MainPID"])
		if v, err := strconv.ParseUint(cur["MemoryCurrent"], 10, 64); err == nil {
			u.Mem = v
		} else if cg := cur["ControlGroup"]; cg != "" {
			// systemd prints "[not set]" when the memory controller is off.
			u.Mem = cgroupRSS("/sys/fs/cgroup" + cg)
		}
		// systemd reports monotonic microseconds since boot; convert to wall
		// clock so the browser can render an age without knowing boot time.
		if us, err := strconv.ParseInt(cur["ActiveEnterTimestampMonotonic"], 10, 64); err == nil && us > 0 {
			u.Since = bootUnix + us/1_000_000
		}
		if u.Name != "" {
			units = append(units, u)
		}
		cur = map[string]string{}
	}

	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur[k] = v
		}
	}
	flush()
	return units
}

// bootTimeUnix derives wall-clock boot time from uptime, which is all that is
// needed to turn systemd's monotonic stamps into timestamps. Boot time does
// not move, so it is computed once.
var bootTimeUnix = sync.OnceValue(func() int64 {
	var secs float64
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			secs, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return time.Now().Unix() - int64(secs)
})
