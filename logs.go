// A live journald tail, re-broadcast to every connected browser.
//
// One `journalctl -f` process serves all viewers: spawning one per tab would
// put an unbounded number of readers on the journal.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LogLine struct {
	TS       int64  `json:"ts"`
	Priority int    `json:"prio"`
	Unit     string `json:"unit"`
	Message  string `json:"msg"`
	Seq      uint64 `json:"seq"`
}

// LogTail follows the journal and fans lines out to subscribers, keeping a
// small backlog so a browser that connects late still sees context.
type LogTail struct {
	mu      sync.Mutex
	subs    map[chan LogLine]struct{}
	ring    []LogLine
	seq     uint64
	backlog int
}

func NewLogTail(backlog int) *LogTail {
	return &LogTail{subs: map[chan LogLine]struct{}{}, backlog: backlog}
}

func (t *LogTail) Subscribe() (<-chan LogLine, []LogLine, func()) {
	ch := make(chan LogLine, 256)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	history := append([]LogLine(nil), t.ring...)
	t.mu.Unlock()
	return ch, history, func() {
		t.mu.Lock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
		t.mu.Unlock()
	}
}

func (t *LogTail) publish(l LogLine) {
	t.mu.Lock()
	t.seq++
	l.Seq = t.seq
	t.ring = append(t.ring, l)
	if len(t.ring) > t.backlog {
		t.ring = t.ring[len(t.ring)-t.backlog:]
	}
	for ch := range t.subs {
		select {
		case ch <- l:
		default: // a stalled browser must not block the journal reader
		}
	}
	t.mu.Unlock()
}

// Run follows the journal until ctx is cancelled, restarting journalctl if it
// exits (log rotation, a systemd restart) rather than going permanently quiet.
func (t *LogTail) Run(ctx context.Context, units []string) {
	for ctx.Err() == nil {
		t.follow(ctx, units)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (t *LogTail) follow(ctx context.Context, units []string) {
	args := []string{"-f", "-n", "80", "-o", "json", "--no-pager"}
	for _, u := range units {
		args = append(args, "-u", u)
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		t.publish(LogLine{
			TS: time.Now().UnixMilli(), Priority: 4, Unit: "pidash",
			Message: "journal unavailable: " + err.Error(),
		})
		return
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if l, ok := parseJournalLine(sc.Bytes()); ok {
			t.publish(l)
		}
	}
	_ = cmd.Wait()
}

type journalEntry struct {
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
	Message  json.RawMessage `json:"MESSAGE"`
	Priority string          `json:"PRIORITY"`
	Unit     string          `json:"_SYSTEMD_UNIT"`
	Ident    string          `json:"SYSLOG_IDENTIFIER"`
	Comm     string          `json:"_COMM"`
}

func parseJournalLine(b []byte) (LogLine, bool) {
	var e journalEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return LogLine{}, false
	}
	msg, ok := decodeJournalMessage(e.Message)
	if !ok {
		return LogLine{}, false
	}
	l := LogLine{Message: msg, Priority: 6}
	if p, err := strconv.Atoi(e.Priority); err == nil {
		l.Priority = p
	}
	if us, err := strconv.ParseInt(e.Realtime, 10, 64); err == nil {
		l.TS = us / 1000
	} else {
		l.TS = time.Now().UnixMilli()
	}
	l.Unit = strings.TrimSuffix(firstNonEmpty(e.Unit, e.Ident, e.Comm, "kernel"), ".service")
	return l, true
}

// decodeJournalMessage handles both forms journald emits: a plain string, or
// an array of byte values when the message is not valid UTF-8.
func decodeJournalMessage(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return string(bytes), true
	}
	return "", false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
