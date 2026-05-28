package eventlog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
