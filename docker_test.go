package main

import "testing"

func TestStatsCPUPercent(t *testing.T) {
	mk := func(cur, pre, sysCur, sysPre uint64, online int) dockerStats {
		var st dockerStats
		st.CPUStats.CPUUsage.TotalUsage = cur
		st.PreCPUStats.CPUUsage.TotalUsage = pre
		st.CPUStats.SystemUsage = sysCur
		st.PreCPUStats.SystemUsage = sysPre
		st.CPUStats.OnlineCPUs = online
		return st
	}
	tests := []struct {
		name string
		st   dockerStats
		want float64
	}{
		// 10% of total system time across 4 cores is 40% of one core.
		{"one tenth of four cores", mk(200, 100, 2000, 1000, 4), 40},
		{"idle", mk(100, 100, 2000, 1000, 4), 0},
		// First sample: precpu is zero, so there is no delta to divide by.
		{"first sample", mk(100, 0, 1000, 0, 4), 40},
		{"no system delta", mk(200, 100, 1000, 1000, 4), 0},
		{"counter went backwards", mk(100, 200, 2000, 1000, 4), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statsCPUPercent(tc.st); got != tc.want {
				t.Errorf("statsCPUPercent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStatsCPUPercentFallsBackToPerCPUCount(t *testing.T) {
	// Older engines omit online_cpus and only send the percpu_usage array.
	var st dockerStats
	st.CPUStats.CPUUsage.TotalUsage = 200
	st.PreCPUStats.CPUUsage.TotalUsage = 100
	st.CPUStats.SystemUsage = 2000
	st.PreCPUStats.SystemUsage = 1000
	st.CPUStats.CPUUsage.PerCPUUsage = []uint64{1, 2, 3, 4}
	if got := statsCPUPercent(st); got != 40 {
		t.Errorf("statsCPUPercent = %v, want 40", got)
	}
}

func TestStatsMemory(t *testing.T) {
	tests := []struct {
		name    string
		usage   uint64
		limit   uint64
		stats   map[string]uint64
		wantUse uint64
		wantPct float64
	}{
		{
			// cgroup v2: page cache is subtracted, matching `docker stats`.
			name: "v2 inactive_file", usage: 300, limit: 1000,
			stats: map[string]uint64{"inactive_file": 100}, wantUse: 200, wantPct: 20,
		},
		{
			name: "v1 total_inactive_file", usage: 300, limit: 1000,
			stats: map[string]uint64{"total_inactive_file": 50}, wantUse: 250, wantPct: 25,
		},
		{
			name: "cache larger than usage is ignored", usage: 100, limit: 1000,
			stats: map[string]uint64{"inactive_file": 500}, wantUse: 100, wantPct: 10,
		},
		{
			// This board: the memory controller is off, so the engine reports
			// nothing and the caller falls back to summing cgroup RSS.
			name: "no accounting", usage: 0, limit: 0, stats: nil, wantUse: 0, wantPct: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var st dockerStats
			st.MemoryStats.Usage = tc.usage
			st.MemoryStats.Limit = tc.limit
			st.MemoryStats.Stats = tc.stats
			used, pct := statsMemory(st)
			if used != tc.wantUse || pct != tc.wantPct {
				t.Errorf("statsMemory = %d,%v want %d,%v", used, pct, tc.wantUse, tc.wantPct)
			}
		})
	}
}

func TestHealthFromStatus(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Up 12 days (healthy)", "healthy"},
		{"Up 3 seconds (health: starting)", "health: starting"},
		{"Up 2 minutes (unhealthy)", "unhealthy"},
		{"Up 12 days", ""},               // no healthcheck configured
		{"Exited (0) 5 minutes ago", ""}, // parens, but not a health state
		{"Restarting (1) 2 seconds ago", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := healthFromStatus(tc.in); got != tc.want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
