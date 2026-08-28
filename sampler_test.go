package main

import (
	"strings"
	"testing"
	"time"
)

func TestJoinArgs(t *testing.T) {
	full := []string{"/usr/bin/app", "--token=s3cret", "--verbose"}
	tests := []struct {
		name string
		args []string
		full bool
		want string
	}{
		// The default must not publish argv: the snapshot API is
		// unauthenticated, and credentials are routinely passed as flags.
		{"redacted by default", full, false, "/usr/bin/app"},
		{"opted in", full, true, "/usr/bin/app --token=s3cret --verbose"},
		{"argv0 with spaces survives", []string{"/opt/my app/bin", "-x"}, false, "/opt/my app/bin"},
		{"kernel thread", nil, false, ""},
		{"kernel thread opted in", nil, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinArgs(tc.args, tc.full); got != tc.want {
				t.Errorf("joinArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoinArgsNeverLeaksSecretsByDefault(t *testing.T) {
	got := joinArgs([]string{"mysqld", "--password=hunter2", "--api-key=abc"}, false)
	for _, secret := range []string{"hunter2", "abc", "password", "api-key"} {
		if strings.Contains(got, secret) {
			t.Errorf("joinArgs leaked %q in %q", secret, got)
		}
	}
}

func TestNewSamplerProcEvery(t *testing.T) {
	tests := []struct {
		name               string
		interval, procIntv time.Duration
		want               int
	}{
		{"three ticks", time.Second, 3 * time.Second, 3},
		{"same cadence", time.Second, time.Second, 1},
		{"proc faster than tick clamps to every tick", time.Second, 100 * time.Millisecond, 1},
		{"sub-second interval", 500 * time.Millisecond, 3 * time.Second, 6},
		{"zero interval does not divide by zero", 0, 3 * time.Second, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSampler(tc.interval, tc.procIntv, nil, nil, 8, false)
			if s.procEvery != tc.want {
				t.Errorf("procEvery = %d, want %d", s.procEvery, tc.want)
			}
		})
	}
}

func TestTopProcessesSkipsRecycledPIDs(t *testing.T) {
	s := NewSampler(time.Second, time.Second, nil, nil, 5, false)
	s.prevProcs = map[int]procTime{
		1: {jiffies: 100, name: "old"},
		2: {jiffies: 50, name: "steady"},
	}
	cur := map[int]procTime{
		1: {jiffies: 10, name: "new"}, // pid reused: counter went backwards
		2: {jiffies: 60, name: "steady"},
		3: {jiffies: 5, name: "fresh"}, // did not exist last sample
	}
	got := s.topProcesses(cur, 1000, 1<<30)
	for _, p := range got {
		if p.PID == 1 {
			t.Error("a recycled pid must not report a negative or wild CPU share")
		}
		if p.PID == 3 {
			t.Error("a brand-new process has no delta to measure yet")
		}
		if p.CPU < 0 {
			t.Errorf("pid %d has negative CPU %v", p.PID, p.CPU)
		}
	}
}

func TestTopProcessesHonoursTopN(t *testing.T) {
	s := NewSampler(time.Second, time.Second, nil, nil, 2, false)
	s.prevProcs = map[int]procTime{}
	cur := map[int]procTime{}
	for pid := 1; pid <= 10; pid++ {
		s.prevProcs[pid] = procTime{jiffies: 0, name: "p"}
		cur[pid] = procTime{jiffies: uint64(pid), name: "p"}
	}
	got := s.topProcesses(cur, 1000, 1<<30)
	if len(got) != 2 {
		t.Fatalf("got %d processes, want 2", len(got))
	}
	if got[0].CPU < got[1].CPU {
		t.Error("results must be ordered by descending CPU")
	}
}

func TestTopProcessesZeroElapsedJiffies(t *testing.T) {
	s := NewSampler(time.Second, time.Second, nil, nil, 5, false)
	s.prevProcs = map[int]procTime{1: {jiffies: 0}}
	if got := s.topProcesses(map[int]procTime{1: {jiffies: 5}}, 0, 1<<30); got != nil {
		t.Errorf("got %v, want nil when no CPU time has elapsed", got)
	}
}
