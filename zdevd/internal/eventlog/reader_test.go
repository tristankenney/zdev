package eventlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLines writes the given text lines to path (each terminated with
// '\n'). Used by reader tests to seed events.ndjson and events.ndjson.1.
func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var buf bytes.Buffer
	for _, ln := range lines {
		buf.WriteString(ln)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestTailLinesCurrentOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	writeLines(t, path, []string{
		`{"ts":"t1","type":"a"}`,
		`{"ts":"t2","type":"b"}`,
		`{"ts":"t3","type":"c"}`,
		`{"ts":"t4","type":"d"}`,
		`{"ts":"t5","type":"e"}`,
	})

	got, err := TailLines(path, 3)
	if err != nil {
		t.Fatalf("TailLines: %v", err)
	}
	want := []string{
		`{"ts":"t5","type":"e"}`,
		`{"ts":"t4","type":"d"}`,
		`{"ts":"t3","type":"c"}`,
	}
	assertLines(t, got, want)
}

func TestTailLinesSpansBothFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")

	// .1 is older — written first.
	writeLines(t, path+".1", []string{
		`{"ts":"old1","type":"a"}`,
		`{"ts":"old2","type":"b"}`,
		`{"ts":"old3","type":"c"}`,
	})
	// current is newer.
	writeLines(t, path, []string{
		`{"ts":"new1","type":"d"}`,
		`{"ts":"new2","type":"e"}`,
	})

	got, err := TailLines(path, 4)
	if err != nil {
		t.Fatalf("TailLines: %v", err)
	}
	// Newest-first: current (newest backwards) then .1 (newest backwards).
	want := []string{
		`{"ts":"new2","type":"e"}`,
		`{"ts":"new1","type":"d"}`,
		`{"ts":"old3","type":"c"}`,
		`{"ts":"old2","type":"b"}`,
	}
	assertLines(t, got, want)
}

func TestTailLinesMissingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	// Neither path nor path+".1" exist.
	got, err := TailLines(path, 5)
	if err != nil {
		t.Fatalf("TailLines on missing files returned err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d lines", len(got))
	}
}

func TestTailLinesMissingDotOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	writeLines(t, path, []string{
		`{"ts":"a","type":"x"}`,
		`{"ts":"b","type":"y"}`,
	})

	got, err := TailLines(path, 5)
	if err != nil {
		t.Fatalf("TailLines: %v", err)
	}
	want := []string{
		`{"ts":"b","type":"y"}`,
		`{"ts":"a","type":"x"}`,
	}
	assertLines(t, got, want)
}

// eventLine marshals an Event with the given unix-second ts and type into
// the same NDJSON wire form the Writer emits, for seeding Scan tests.
func eventLine(t *testing.T, unixTS int64, typ, session string) string {
	t.Helper()
	b, err := json.Marshal(Event{Ts: time.Unix(unixTS, 0).UTC(), Type: typ, Session: session})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(b)
}

func TestScanSinceFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	writeLines(t, path, []string{
		eventLine(t, 1000, "state-change", "a"),
		eventLine(t, 2000, "state-change", "b"),
		eventLine(t, 3000, "state-change", "c"),
		eventLine(t, 4000, "state-change", "d"),
	})

	// since strictly after the first two events: only c and d survive.
	got, err := Scan(path, 2500)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Session != "c" || got[1].Session != "d" {
		t.Errorf("chronological order broken: got %q, %q", got[0].Session, got[1].Session)
	}
	// Boundary: an event exactly at `since` is included (>= semantics).
	got, err = Scan(path, 3000)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 || got[0].Session != "c" {
		t.Errorf("since==ts boundary: got %+v, want c,d", got)
	}
	// since==0 returns everything.
	got, err = Scan(path, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("since==0: got %d events, want 4", len(got))
	}
}

func TestScanMalformedSkip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	writeLines(t, path, []string{
		eventLine(t, 1000, "state-change", "good1"),
		`{not valid json at all`,
		``,                                    // blank line
		`{"ts":"not-a-timestamp","type":"x"}`, // unparseable ts → skip
		eventLine(t, 2000, "state-change", "good2"),
	})

	got, err := Scan(path, 0)
	if err != nil {
		t.Fatalf("Scan returned err on malformed lines (should fail-soft): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (malformed skipped): %+v", len(got), got)
	}
	if got[0].Session != "good1" || got[1].Session != "good2" {
		t.Errorf("wrong survivors: %q, %q", got[0].Session, got[1].Session)
	}
}

func TestScanEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")

	// Missing file (neither path nor path+".1"): empty, no error.
	got, err := Scan(path, 0)
	if err != nil {
		t.Fatalf("Scan on missing file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file: got %d events, want 0", len(got))
	}

	// Empty file: empty, no error.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	got, err = Scan(path, 0)
	if err != nil {
		t.Fatalf("Scan on empty file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty file: got %d events, want 0", len(got))
	}
}

func TestScanSpansRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	// .1 is older — its events precede the current file chronologically.
	writeLines(t, path+".1", []string{
		eventLine(t, 1000, "state-change", "old1"),
		eventLine(t, 2000, "state-change", "old2"),
	})
	writeLines(t, path, []string{
		eventLine(t, 3000, "state-change", "new1"),
		eventLine(t, 4000, "state-change", "new2"),
	})

	got, err := Scan(path, 1500)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"old2", "new1", "new2"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Session != w {
			t.Errorf("event %d: got %q, want %q", i, got[i].Session, w)
		}
	}
}

// assertLines compares actual output against expected lines (each as a
// string). Used by all four reader tests.
func assertLines(t *testing.T, got [][]byte, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n got=%s\nwant=%v", len(got), len(want), debugLines(got), want)
	}
	for i := range got {
		if string(got[i]) != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func debugLines(lns [][]byte) string {
	var b bytes.Buffer
	for i, ln := range lns {
		b.WriteString(" [")
		b.WriteString(itoa(i))
		b.WriteString("] ")
		b.Write(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// itoa is a tiny formatter avoiding strconv in this debug helper to keep
// the test file's imports minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
