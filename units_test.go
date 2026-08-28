package main

import "testing"

func TestParseUnitShow(t *testing.T) {
	// Two records, blank-line separated, exactly as systemctl emits them.
	const out = `Id=otelcol-contrib.service
Description=OpenTelemetry Collector Contrib
ActiveState=active
SubState=running
ControlGroup=/system.slice/otelcol-contrib.service
ActiveEnterTimestampMonotonic=60000000
NRestarts=2
MemoryCurrent=[not set]
MainPID=1234

Id=ssh.service
Description=OpenBSD Secure Shell server
ActiveState=failed
SubState=dead
ControlGroup=/system.slice/ssh.service
ActiveEnterTimestampMonotonic=0
NRestarts=0
MemoryCurrent=8388608
MainPID=0
`
	rss := func(path string) uint64 {
		if path == "/sys/fs/cgroup/system.slice/otelcol-contrib.service" {
			return 99
		}
		t.Errorf("unexpected cgroup path %q", path)
		return 0
	}
	units := parseUnitShow(out, 1000, rss)
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2", len(units))
	}

	u := units[0]
	if u.Name != "otelcol-contrib" {
		t.Errorf("Name = %q, want the .service suffix stripped", u.Name)
	}
	if u.Active != "active" || u.Sub != "running" || u.Restarts != 2 || u.PID != 1234 {
		t.Errorf("got %+v", u)
	}
	// MemoryCurrent is "[not set]" when the memory cgroup controller is off,
	// so the cgroup RSS fallback must be used instead of reporting zero.
	if u.Mem != 99 {
		t.Errorf("Mem = %d, want the cgroup fallback value 99", u.Mem)
	}
	if want := int64(1000 + 60); u.Since != want {
		t.Errorf("Since = %d, want %d (boot + monotonic microseconds)", u.Since, want)
	}

	s := units[1]
	if s.Active != "failed" {
		t.Errorf("Active = %q, want failed", s.Active)
	}
	if s.Mem != 8388608 {
		t.Errorf("Mem = %d, want the reported MemoryCurrent", s.Mem)
	}
	// A unit that never became active has no start time to render an age from.
	if s.Since != 0 {
		t.Errorf("Since = %d, want 0 when ActiveEnterTimestampMonotonic is 0", s.Since)
	}
}

func TestParseUnitShowSkipsRecordsWithoutAnId(t *testing.T) {
	units := parseUnitShow("Description=nothing\nActiveState=active\n", 0, nil)
	if len(units) != 0 {
		t.Errorf("got %d units, want none for a record with no Id", len(units))
	}
}

func TestParseUnitShowEmpty(t *testing.T) {
	if got := parseUnitShow("", 0, nil); len(got) != 0 {
		t.Errorf("got %d units, want none", len(got))
	}
}
