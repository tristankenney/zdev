package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// runWriter starts w.Run on a background goroutine, submits events via fn,
// then cancels the context and blocks on Done. Returns when the writer has
// fully flushed and closed its file. Tests use this instead of time.Sleep
// to deterministically synchronize with the writer goroutine.
func runWriter(t *testing.T, w *Writer, fn func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(ctx) }()

	fn()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel within 2s")
	}
	<-w.Done()
}

// drainSubmit submits an event and busy-waits until the writer goroutine
// has consumed it from the channel. Returns once `len(w.in) == 0`. We use
// this instead of time.Sleep so tests are deterministic.
func drainSubmit(t *testing.T, w *Writer, ev Event) {
	t.Helper()
	w.Submit(ev)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.in) == 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("Submit did not drain within 2s")
}

func TestEventCategories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	w := New(path)

	now := time.Date(2026, 5, 5, 10, 15, 32, 124_000_000, time.UTC)

	runWriter(t, w, func() {
		drainSubmit(t, w, Event{Ts: now, Type: "state-change", Session: "zdev", Project: "zdev", From: "alive", To: "waiting"})
		drainSubmit(t, w, Event{Ts: now.Add(time.Second), Type: "pr-count", Project: "dotfiles", OpenBefore: 3, OpenAfter: 2})
		drainSubmit(t, w, Event{Ts: now.Add(2 * time.Second), Type: "port-change", Session: "work", Port: 3000, Op: "open"})
		drainSubmit(t, w, Event{Ts: now.Add(3 * time.Second), Type: "daemon-start", Version: "phase4-v1", PID: 12345})
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events.ndjson: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), data)
	}

	var got [4]Event
	for i, ln := range lines {
		if err := json.Unmarshal(ln, &got[i]); err != nil {
			t.Fatalf("line %d: unmarshal: %v\n%s", i, err, ln)
		}
	}

	if got[0].Type != "state-change" || got[0].Session != "zdev" || got[0].From != "alive" || got[0].To != "waiting" {
		t.Errorf("state-change line wrong: %+v", got[0])
	}
	// state-change should NOT carry pr-count fields.
	if got[0].OpenBefore != 0 || got[0].OpenAfter != 0 {
		t.Errorf("state-change leaked pr-count fields: %+v", got[0])
	}

	if got[1].Type != "pr-count" || got[1].Project != "dotfiles" || got[1].OpenBefore != 3 || got[1].OpenAfter != 2 {
		t.Errorf("pr-count line wrong: %+v", got[1])
	}
	if got[1].Session != "" || got[1].From != "" {
		t.Errorf("pr-count leaked state-change fields: %+v", got[1])
	}

	if got[2].Type != "port-change" || got[2].Session != "work" || got[2].Port != 3000 || got[2].Op != "open" {
		t.Errorf("port-change line wrong: %+v", got[2])
	}

	if got[3].Type != "daemon-start" || got[3].Version != "phase4-v1" || got[3].PID != 12345 {
		t.Errorf("daemon-start line wrong: %+v", got[3])
	}

	// omitempty on the line (raw JSON) — pr-count line MUST NOT contain "session" key.
	if bytes.Contains(lines[1], []byte(`"session"`)) {
		t.Errorf("pr-count line contains session key (omitempty broken):\n%s", lines[1])
	}
}

func TestEventLogFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	w := New(path)

	now := time.Date(2026, 5, 5, 10, 15, 32, 124_345_678, time.UTC)

	runWriter(t, w, func() {
		drainSubmit(t, w, Event{Ts: now, Type: "daemon-start", Version: "phase4-v1", PID: 99})
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Errorf("file does not end with '\\n': %q", data)
	}
	if bytes.Contains(data, []byte("\r")) {
		t.Errorf("file contains '\\r': %q", data)
	}

	line := bytes.TrimRight(data, "\n")
	// Exactly one line, exactly one JSON object.
	if bytes.Count(line, []byte("\n")) != 0 {
		t.Errorf("multiple lines for one event: %q", line)
	}
	var parsed struct {
		Ts string `json:"ts"`
	}
	if err := json.Unmarshal(line, &parsed); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, parsed.Ts); err != nil {
		t.Errorf("ts not RFC3339Nano: %q (err: %v)", parsed.Ts, err)
	}
	// Sanity: nanosecond precision is preserved on the wire.
	if !strings.Contains(parsed.Ts, ".") {
		t.Errorf("ts has no fractional seconds (RFC3339Nano expected): %q", parsed.Ts)
	}
}

func TestEventLogRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	dotOne := path + ".1"

	// Pre-seed an existing .1 with bytes that MUST be overwritten on rotation
	// (D4-12: drop .1 on next rotation).
	if err := os.WriteFile(dotOne, []byte("STALE-PRIOR-ROTATION-CONTENTS\n"), 0o600); err != nil {
		t.Fatalf("seed dotone: %v", err)
	}

	// 256-byte rotation threshold so a handful of events triggers rotation.
	w := NewWithRotateAt(path, 256)

	runWriter(t, w, func() {
		for i := 0; i < 10; i++ {
			drainSubmit(t, w, Event{
				Ts:         time.Date(2026, 5, 5, 10, 15, 32, i*1_000_000, time.UTC),
				Type:       "pr-count",
				Project:    "dotfiles",
				OpenBefore: i,
				OpenAfter:  i + 1,
			})
		}
	})

	stCurrent, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current: %v", err)
	}
	stDotOne, err := os.Stat(dotOne)
	if err != nil {
		t.Fatalf("stat .1 (should exist after rotation): %v", err)
	}
	if stCurrent.Size() >= 256 {
		t.Errorf("post-rotate current too large: %d (expected < 256)", stCurrent.Size())
	}
	if stDotOne.Size() == 0 {
		t.Errorf(".1 is empty: rotation did not preserve previous contents")
	}
	// Stale prior contents MUST be gone — D4-12 says drop .1 on next rotation.
	dotOneBytes, err := os.ReadFile(dotOne)
	if err != nil {
		t.Fatalf("read .1: %v", err)
	}
	if bytes.Contains(dotOneBytes, []byte("STALE-PRIOR-ROTATION-CONTENTS")) {
		t.Errorf(".1 still contains stale pre-rotation bytes: %q", dotOneBytes)
	}
}

func TestSubmitDropsWhenChannelFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")

	// Capacity 1 — Run is NOT started yet, so the channel can buffer
	// exactly one event before subsequent Submits drop.
	w := NewWithCap(path, 1)

	before := runtime.NumGoroutine()
	w.Submit(Event{Ts: time.Now().UTC(), Type: "state-change", Session: "a", From: "x", To: "y"})
	w.Submit(Event{Ts: time.Now().UTC(), Type: "state-change", Session: "b", From: "x", To: "y"})
	w.Submit(Event{Ts: time.Now().UTC(), Type: "state-change", Session: "c", From: "x", To: "y"})
	after := runtime.NumGoroutine()

	if after != before {
		t.Errorf("Submit started a goroutine (drop path is supposed to be lock-free): before=%d after=%d", before, after)
	}
	if got := len(w.in); got != 1 {
		t.Errorf("channel buffered %d events, want 1 (others should drop)", got)
	}
}

// TestNewProductionCapacity is a sentinel: it guards against accidental
// shrinks of DefaultChanCap. With dual-supervisor (GT + default sockets),
// both supervisors bootstrap simultaneously and generate ~100+ events at
// startup — 256 absorbs this burst without dropping.
func TestNewProductionCapacity(t *testing.T) {
	w := New(filepath.Join(t.TempDir(), "x.ndjson"))
	if got, want := cap(w.in), DefaultChanCap; got != want {
		t.Fatalf("cap(w.in) = %d, want DefaultChanCap (%d)", got, want)
	}
	if DefaultChanCap < 256 {
		t.Fatalf("DefaultChanCap = %d, must be >= 256 to absorb dual-supervisor startup burst", DefaultChanCap)
	}
}

// TestNoTickers is belt-and-suspenders next to scripts/check-no-daemon-fork.sh.
// Forbids time.NewTicker / time.AfterFunc in eventlog.go and reader.go even
// if a future refactor moves the gate.
func TestNoTickers(t *testing.T) {
	for _, fname := range []string{"eventlog.go", "reader.go"} {
		data, err := os.ReadFile(fname)
		if err != nil {
			t.Fatalf("read %s: %v", fname, err)
		}
		// Strip line comments to allow doc-comment mentions of the
		// banned APIs (this file's package doc references them as
		// rationale).
		clean := stripLineComments(data)
		if bytes.Contains(clean, []byte("time.NewTicker")) {
			t.Errorf("%s: contains time.NewTicker (Pitfall 4 violation)", fname)
		}
		if bytes.Contains(clean, []byte("time.AfterFunc")) {
			t.Errorf("%s: contains time.AfterFunc (Pitfall 4 violation)", fname)
		}
	}
}

// stripLineComments replaces every line whose first non-whitespace bytes
// are "//" with an empty line. Crude but sufficient for the no-tickers
// audit — the package docs and decision comments live in `// ...` lines
// and would otherwise trip the substring check.
func stripLineComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for _, ln := range bytes.Split(data, []byte("\n")) {
		trim := bytes.TrimLeft(ln, " \t")
		if bytes.HasPrefix(trim, []byte("//")) {
			out = append(out, '\n')
			continue
		}
		out = append(out, ln...)
		out = append(out, '\n')
	}
	return out
}
