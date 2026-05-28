package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns the
// captured bytes. Restores stdout on cleanup. NOT safe for parallel tests.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return string(out)
}

// captureStderr mirrors captureStdout for stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	out := <-done
	_ = r.Close()
	return string(out)
}

// fixtureNDJSON: three valid Event lines covering 3 of the 4 categories.
const fixtureNDJSON = `{"ts":"2026-05-05T10:15:32.124Z","type":"state-change","session":"zdev","project":"zdev","from":"alive","to":"waiting"}
{"ts":"2026-05-05T10:15:45.001Z","type":"pr-count","project":"dotfiles","open_before":3,"open_after":2}
{"ts":"2026-05-05T10:16:01.330Z","type":"port-change","session":"work","port":3000,"op":"open"}
`

// TestHistorySubcmdReadsFile: 3 valid Event lines → 3 formatted human-readable
// lines on stdout, exit 0.
func TestHistorySubcmdReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(path, []byte(fixtureNDJSON), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var rc int
	out := captureStdout(t, func() {
		rc = historySubcmd([]string{"-path", path, "-tail", "10"})
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	gotLines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(gotLines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(gotLines), out)
	}
	// eventlog.TailLines returns newest-first: port-change (10:16:01) →
	// pr-count (10:15:45) → state-change (10:15:32).
	if !strings.Contains(gotLines[0], "port-change") || !strings.Contains(gotLines[0], ":3000") {
		t.Errorf("line 0 = %q; expected port-change :3000", gotLines[0])
	}
	if !strings.Contains(gotLines[1], "pr-count") || !strings.Contains(gotLines[1], "3→2") {
		t.Errorf("line 1 = %q; expected pr-count 3→2", gotLines[1])
	}
	if !strings.Contains(gotLines[2], "state-change") || !strings.Contains(gotLines[2], "alive→waiting") {
		t.Errorf("line 2 = %q; expected state-change with alive→waiting", gotLines[2])
	}
}

// TestHistorySubcmdJSONFlag: --json passes raw NDJSON lines through unchanged
// (modulo trailing newline normalization).
func TestHistorySubcmdJSONFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(path, []byte(fixtureNDJSON), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var rc int
	out := captureStdout(t, func() {
		rc = historySubcmd([]string{"-path", path, "-tail", "10", "-json"})
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	// Each output line should be the original raw JSON, newest-first.
	gotLines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(gotLines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(gotLines), out)
	}
	// Newest-first: port-change (10:16:01) is first.
	if !strings.HasPrefix(gotLines[0], `{"ts":"2026-05-05T10:16:01`) {
		t.Errorf("line 0 not raw JSON: %q", gotLines[0])
	}
	if !strings.Contains(gotLines[0], `"type":"port-change"`) {
		t.Errorf("line 0 missing type discriminator: %q", gotLines[0])
	}
}

// TestHistorySubcmdMissingFile: LOG-04 — `zdevd history` against a missing
// file returns exit 0 (eventlog.TailLines returns empty slice for ENOENT)
// and prints nothing. This is the "daemon hasn't yet emitted anything"
// outage path.
func TestHistorySubcmdMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-file.ndjson")

	var rc int
	out := captureStdout(t, func() {
		rc = historySubcmd([]string{"-path", missing, "-tail", "10"})
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (LOG-04 outage-resilience)", rc)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// TestHistorySubcmdNegativeTail: --tail -1 is a usage error (rc=2).
func TestHistorySubcmdNegativeTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(path, []byte(fixtureNDJSON), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var rc int
	stderr := captureStderr(t, func() {
		rc = historySubcmd([]string{"-path", path, "-tail", "-1"})
	})
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr, "tail must be > 0") {
		t.Errorf("stderr = %q, want \"tail must be > 0\"", stderr)
	}
}

// TestDiagSubcmdConnectionRefused: dialing a nonexistent socket exits 1
// without hanging. Locks the "diag during outage exits cleanly" property.
func TestDiagSubcmdConnectionRefused(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.sock")

	var rc int
	stderr := captureStderr(t, func() {
		rc = diagSubcmd([]string{"-socket", missing})
	})
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	if !strings.Contains(stderr, "zdevd diag:") {
		t.Errorf("stderr = %q, want prefix \"zdevd diag:\"", stderr)
	}
}
