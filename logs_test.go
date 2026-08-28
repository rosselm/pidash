package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseJournalLine(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantOK   bool
		wantUnit string
		wantMsg  string
		wantPrio int
	}{
		{
			name: "ordinary entry", wantOK: true,
			in:       `{"__REALTIME_TIMESTAMP":"1700000000000000","MESSAGE":"started","PRIORITY":"6","_SYSTEMD_UNIT":"pidash.service"}`,
			wantUnit: "pidash", wantMsg: "started", wantPrio: 6,
		},
		{
			// journald sends MESSAGE as an array of byte values when it is not
			// valid UTF-8; dropping those would silently lose log lines.
			name: "byte-array message", wantOK: true,
			in:       `{"__REALTIME_TIMESTAMP":"1700000000000000","MESSAGE":[104,105],"PRIORITY":"3","_SYSTEMD_UNIT":"x.service"}`,
			wantUnit: "x", wantMsg: "hi", wantPrio: 3,
		},
		{
			name: "falls back to syslog identifier", wantOK: true,
			in:       `{"__REALTIME_TIMESTAMP":"1700000000000000","MESSAGE":"m","PRIORITY":"4","SYSLOG_IDENTIFIER":"sudo"}`,
			wantUnit: "sudo", wantMsg: "m", wantPrio: 4,
		},
		{
			name: "no unit at all becomes kernel", wantOK: true,
			in:       `{"__REALTIME_TIMESTAMP":"1700000000000000","MESSAGE":"oops"}`,
			wantUnit: "kernel", wantMsg: "oops", wantPrio: 6, // default priority
		},
		{"not json", `not json at all`, false, "", "", 0},
		{"no message field", `{"PRIORITY":"6"}`, false, "", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseJournalLine([]byte(tc.in))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Unit != tc.wantUnit || got.Message != tc.wantMsg || got.Priority != tc.wantPrio {
				t.Errorf("got %+v, want unit=%q msg=%q prio=%d", got, tc.wantUnit, tc.wantMsg, tc.wantPrio)
			}
			if got.TS != 1700000000000 && tc.name != "no unit at all becomes kernel" {
				t.Errorf("TS = %d, want microseconds converted to milliseconds", got.TS)
			}
		})
	}
}

func TestParseJournalLineMissingTimestampUsesNow(t *testing.T) {
	before := time.Now().UnixMilli()
	got, ok := parseJournalLine([]byte(`{"MESSAGE":"m","PRIORITY":"6"}`))
	if !ok {
		t.Fatal("expected the line to parse")
	}
	if got.TS < before {
		t.Errorf("TS = %d, want a wall-clock fallback at or after %d", got.TS, before)
	}
}

func TestDecodeJournalMessage(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"string", `"hello"`, "hello", true},
		{"bytes", `[104,101,108,108,111]`, "hello", true},
		{"empty string is still a message", `""`, "", true},
		{"number is not a message", `42`, "", false},
		{"absent", ``, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeJournalMessage(json.RawMessage(tc.in))
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("got %q,%v want %q,%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLogTailKeepsBoundedBacklog(t *testing.T) {
	tail := NewLogTail(3)
	for i := 0; i < 10; i++ {
		tail.publish(LogLine{Message: string(rune('a' + i))})
	}
	_, history, cancel := tail.Subscribe()
	defer cancel()
	if len(history) != 3 {
		t.Fatalf("backlog = %d entries, want 3", len(history))
	}
	if history[0].Message != "h" || history[2].Message != "j" {
		t.Errorf("backlog = %v, want the three most recent", history)
	}
	if history[2].Seq != 10 {
		t.Errorf("Seq = %d, want a monotonic count of 10", history[2].Seq)
	}
}

func TestLogTailDoesNotBlockOnAStalledSubscriber(t *testing.T) {
	// A browser that stops reading must not wedge the journal reader.
	tail := NewLogTail(2)
	_, _, cancel := tail.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ { // far beyond the channel buffer
			tail.publish(LogLine{Message: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a subscriber that never reads")
	}
}

func TestLogTailUnsubscribeIsIdempotent(t *testing.T) {
	tail := NewLogTail(2)
	_, _, cancel := tail.Subscribe()
	cancel()
	cancel() // must not panic by closing an already-closed channel
	tail.publish(LogLine{Message: "after"})
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q, want third", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
