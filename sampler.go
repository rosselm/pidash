// The sampler owns every read of /proc and fans one shared snapshot out to all
// viewers. Per-client polling would multiply the /proc traffic by the number of
// open tabs, and the CPU percentages would disagree between them.
package main

import (
	"context"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// slowState holds everything too expensive to gather every tick. Docker's
// stats endpoint alone holds a connection open for a full sampling interval.
type slowState struct {
	Containers []Container
	Units      []Unit
	DockerErr  string
}

type Sampler struct {
	interval time.Duration
	docker   *dockerClient
	unitList []string
	topN     int

	// procEvery throttles the process table: walking every /proc/<pid>/stat is
	// by far the most expensive thing done per tick, and a top-N table does not
	// need to move as fast as a gauge.
	procEvery int
	// exposeCmdline publishes full command lines. Off by default: the API is
	// unauthenticated, and argv routinely carries tokens and passwords.
	exposeCmdline bool

	mu   sync.Mutex
	subs map[chan *Snapshot]struct{}
	last *Snapshot

	prevAt    time.Time
	prevCPU   cpuTimes
	prevCores []cpuTimes
	prevNet   map[string]netCounters

	tick          int
	prevProcs     map[int]procTime
	prevProcTotal uint64
	lastProcs     []Proc

	slow atomic.Pointer[slowState]
}

func NewSampler(interval, procInterval time.Duration, docker *dockerClient, units []string, topN int, exposeCmdline bool) *Sampler {
	every := 1
	if interval > 0 {
		every = int(procInterval / interval)
	}
	if every < 1 {
		every = 1
	}
	return &Sampler{
		interval:      interval,
		docker:        docker,
		unitList:      units,
		topN:          topN,
		procEvery:     every,
		exposeCmdline: exposeCmdline,
		subs:          map[chan *Snapshot]struct{}{},
	}
}

// Subscribe returns a channel of snapshots plus the most recent one, so a page
// paints immediately instead of waiting a full tick.
func (s *Sampler) Subscribe() (<-chan *Snapshot, *Snapshot, func()) {
	ch := make(chan *Snapshot, 4)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	last := s.last
	s.mu.Unlock()
	return ch, last, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

func (s *Sampler) Latest() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Sampler) Run(ctx context.Context) {
	s.collect() // prime the deltas; the first tick then has real percentages
	go s.runSlow(ctx)

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap := s.collect()
			s.mu.Lock()
			s.last = snap
			for ch := range s.subs {
				select {
				case ch <- snap:
				default: // a slow browser gets the next one, not a backlog
				}
			}
			s.mu.Unlock()
		}
	}
}

// runSlow refreshes docker and systemd state on its own, longer cadence.
func (s *Sampler) runSlow(ctx context.Context) {
	tick := func() {
		st := &slowState{}
		if s.docker != nil {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			containers, err := s.docker.list(cctx)
			cancel()
			if err != nil {
				st.DockerErr = err.Error()
			}
			st.Containers = containers
		}
		uctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st.Units = readUnits(uctx, s.unitList)
		cancel()
		s.slow.Store(st)
	}
	tick()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

func (s *Sampler) collect() *Snapshot {
	now := time.Now()
	elapsed := now.Sub(s.prevAt).Seconds()

	aggCPU, coreCPU := readCPUTimes()
	netNow := readNetCounters()
	s.tick++

	snap := &Snapshot{
		TS:      now.UnixMilli(),
		Host:    readHost(),
		Mem:     readMem(),
		Thermal: readThermal(),
		Disks:   readDisks(),
	}
	snap.Host.Cores = runtime.NumCPU()

	// --- cpu ---
	c := CPU{
		FreqMHz:  curFreqMHz(),
		MaxMHz:   readIntFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq") / 1000,
		MinMHz:   readIntFile("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_min_freq") / 1000,
		Governor: cpuGovernor(),
		PerCore:  make([]float64, len(coreCPU)),
	}
	c.Load1, c.Load5, c.Load15, snap.Host.Running, snap.Host.Procs = readLoad()
	if !s.prevAt.IsZero() {
		c.Total = pctBusy(aggCPU, s.prevCPU)
		if dt := float64(aggCPU.total - s.prevCPU.total); dt > 0 {
			c.User = clampPct(100 * float64(aggCPU.user-s.prevCPU.user) / dt)
			c.System = clampPct(100 * float64(aggCPU.system-s.prevCPU.system) / dt)
			c.IOWait = clampPct(100 * float64(aggCPU.iowait-s.prevCPU.iowait) / dt)
		}
		for i := range coreCPU {
			if i < len(s.prevCores) {
				c.PerCore[i] = pctBusy(coreCPU[i], s.prevCores[i])
			}
		}
	}
	snap.CPU = c

	// --- network ---
	if elapsed > 0 && s.prevNet != nil {
		for name, cur := range netNow {
			prev, ok := s.prevNet[name]
			if !ok {
				continue
			}
			addr, up := ifaceAddr(name)
			snap.Nets = append(snap.Nets, Net{
				Name:    name,
				RxBps:   perSecond(cur.rx, prev.rx, elapsed),
				TxBps:   perSecond(cur.tx, prev.tx, elapsed),
				RxTotal: cur.rx,
				TxTotal: cur.tx,
				RxErr:   cur.rxErr,
				TxErr:   cur.txErr,
				RxDrop:  cur.rxDrop,
				Addr:    addr,
				Up:      up,
			})
		}
		sort.Slice(snap.Nets, func(i, j int) bool {
			// Interfaces carrying traffic first, then alphabetical.
			li, lj := snap.Nets[i].RxTotal+snap.Nets[i].TxTotal, snap.Nets[j].RxTotal+snap.Nets[j].TxTotal
			if (li > 0) != (lj > 0) {
				return li > 0
			}
			return snap.Nets[i].Name < snap.Nets[j].Name
		})
	}

	// --- processes ---
	// Sampled on its own, slower cadence, so the CPU share is measured across
	// the whole gap rather than a single tick of it.
	if s.prevProcs == nil || s.tick%s.procEvery == 0 {
		procNow := readProcTimes()
		if s.prevProcs != nil {
			s.lastProcs = s.topProcesses(procNow, aggCPU.total-s.prevProcTotal, snap.Mem.Total)
		}
		s.prevProcs, s.prevProcTotal = procNow, aggCPU.total
	}
	snap.Procs = s.lastProcs

	if st := s.slow.Load(); st != nil {
		snap.Containers = st.Containers
		snap.Units = st.Units
	}

	s.prevAt, s.prevCPU, s.prevCores, s.prevNet = now, aggCPU, coreCPU, netNow
	return snap
}

// topProcesses ranks by CPU share, then fills in the expensive per-process
// details (cmdline, owner, RSS) for the winners only.
func (s *Sampler) topProcesses(cur map[int]procTime, totalJiffies uint64, memTotal uint64) []Proc {
	if totalJiffies == 0 {
		return nil
	}
	ncpu := float64(runtime.NumCPU())
	type scored struct {
		pid  int
		name string
		cpu  float64
	}
	ranked := make([]scored, 0, len(cur))
	for pid, p := range cur {
		prev, ok := s.prevProcs[pid]
		if !ok || p.jiffies < prev.jiffies {
			continue // new process, or a recycled pid
		}
		// Percent of a single core, matching top's default view.
		pct := 100 * float64(p.jiffies-prev.jiffies) / float64(totalJiffies) * ncpu
		ranked = append(ranked, scored{pid: pid, name: p.name, cpu: pct})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].cpu != ranked[j].cpu {
			return ranked[i].cpu > ranked[j].cpu
		}
		return ranked[i].pid < ranked[j].pid
	})
	if len(ranked) > s.topN {
		ranked = ranked[:s.topN]
	}

	out := make([]Proc, 0, len(ranked))
	for _, r := range ranked {
		p := Proc{PID: r.pid, Name: r.name, CPU: r.cpu, RSS: procRSS(r.pid)}
		p.Cmd = joinArgs(procArgs(r.pid), s.exposeCmdline)
		p.User = procUser(r.pid)
		if memTotal > 0 {
			p.MemP = 100 * float64(p.RSS) / float64(memTotal)
		}
		out = append(out, p)
	}
	return out
}

// joinArgs renders a process's argv for publication. Without full disclosure
// only argv[0] is returned: the snapshot API is unauthenticated on the LAN, and
// credentials passed as flags would otherwise be readable by anyone on it.
func joinArgs(args []string, full bool) string {
	if len(args) == 0 {
		return ""
	}
	if full {
		return strings.Join(args, " ")
	}
	return args[0]
}

func perSecond(cur, prev uint64, elapsed float64) float64 {
	if cur < prev || elapsed <= 0 {
		return 0 // counter wrapped or the interface was reset
	}
	return float64(cur-prev) / elapsed
}
