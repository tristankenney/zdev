package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/demo"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	socketpkg "github.com/tristankenney/zdev/zdevd/internal/socket"
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

// TestDemoSubcmdBadFlag: unknown flag returns rc=2 without hanging.
func TestDemoSubcmdBadFlag(t *testing.T) {
	var rc int
	captureStderr(t, func() {
		rc = demoSubcmd([]string{"--bad-flag-that-does-not-exist"})
	})
	if rc != 2 {
		t.Errorf("demoSubcmd(bad flag) rc=%d, want 2", rc)
	}
}

// TestDemoRoundTrip wires demo.DemoSource + socket.Server directly (same code
// path as demoSubcmd, without the signal.NotifyContext), dials as a subscriber,
// and verifies the hello → snapshot round-trip.
func TestDemoRoundTrip(t *testing.T) {
	// The socket's parent must be 0700 (M7 bind-time enforcement, matching
	// production's runtimeDir); t.TempDir() creates its numbered subdir at
	// 0777&~umask (0755 on a default umask), so tighten it explicitly.
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o700); err != nil {
		t.Fatalf("chmod sock dir: %v", err)
	}
	sockPath := filepath.Join(sockDir, "demo.sock")

	src, err := demo.New()
	if err != nil {
		t.Fatalf("demo.New: %v", err)
	}
	srv := socketpkg.NewServer(sockPath)
	srv.SetHub(src)
	if err := srv.Listen(); err != nil {
		t.Fatalf("srv.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = src.Run(ctx) }()
	go func() { _ = srv.Serve(ctx) }()

	// Dial and send a hello frame.
	conn, derr := net.DialTimeout("unix", sockPath, time.Second)
	if derr != nil {
		t.Fatalf("dial demo socket: %v", derr)
	}
	defer conn.Close()

	if _, werr := conn.Write([]byte(`{"type":"hello","v":1,"tmux_pane":"","tmux_session":""}` + "\n")); werr != nil {
		t.Fatalf("write hello: %v", werr)
	}

	// The server should respond with a snapshot immediately.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64*1024)
	n, rerr := conn.Read(buf)
	if rerr != nil {
		t.Fatalf("read snapshot: %v", rerr)
	}
	body := strings.TrimRight(string(buf[:n]), "\n")
	if !strings.Contains(body, `"type":"snapshot"`) {
		t.Errorf("response not a snapshot: %q", body)
	}
	if !strings.Contains(body, `"projects"`) {
		t.Errorf("response missing projects field: %q", body)
	}
	if !strings.Contains(body, `"schema"`) {
		t.Errorf("response missing schema field: %q", body)
	}
}

// TestNotifyMuteSubcmd_WritesSentinel: `zdevd notify-mute 60` writes a
// future unix timestamp to hub.MutePath() and prints a human "muted
// until HH:MM" confirmation. Routes through HOME so the test stays
// hermetic on both platforms (XDG_STATE_HOME on linux, ~/Library on
// darwin both derive from $HOME).
func TestNotifyMuteSubcmd_WritesSentinel(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, ".local", "state"))

	stdout := captureStdout(t, func() {
		if rc := notifyMuteSubcmd([]string{"60"}); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(stdout, "muted until ") {
		t.Errorf("stdout = %q, want prefix \"muted until\"", stdout)
	}

	// File must exist and contain a timestamp ~60s in the future.
	path := hub.MutePath()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	got := strings.TrimSpace(string(b))
	if got == "" {
		t.Fatalf("sentinel empty")
	}
	// Don't assert exact value (race with Now()), just sanity.
	if len(got) < 8 {
		t.Errorf("sentinel %q looks too short to be a unix timestamp", got)
	}
}

// TestNotifyMuteSubcmd_Unmute: `zdevd notify-mute 0` removes the
// sentinel file, and is idempotent (calling it again with no file
// present is still rc=0).
func TestNotifyMuteSubcmd_Unmute(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, ".local", "state"))

	// Seed: mute first, then unmute.
	if rc := notifyMuteSubcmd([]string{"60"}); rc != 0 {
		t.Fatalf("seed mute rc=%d", rc)
	}
	stdout := captureStdout(t, func() {
		if rc := notifyMuteSubcmd([]string{"0"}); rc != 0 {
			t.Fatalf("unmute rc = %d, want 0", rc)
		}
	})
	if !strings.Contains(stdout, "notifications: active") {
		t.Errorf("stdout = %q, want \"notifications: active\"", stdout)
	}
	if _, err := os.Stat(hub.MutePath()); !os.IsNotExist(err) {
		t.Errorf("sentinel must be removed after unmute; stat err = %v", err)
	}

	// Idempotent: second unmute on already-absent file still succeeds.
	if rc := notifyMuteSubcmd([]string{"0"}); rc != 0 {
		t.Errorf("idempotent unmute rc=%d, want 0", rc)
	}
}

// TestNotifyMuteSubcmd_AcceptsDurationString: "30m" / "2h" parse via
// time.ParseDuration; bare integers stay supported.
func TestNotifyMuteSubcmd_AcceptsDurationString(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, ".local", "state"))

	for _, arg := range []string{"30m", "2h", "90"} {
		captureStdout(t, func() {
			if rc := notifyMuteSubcmd([]string{arg}); rc != 0 {
				t.Errorf("%s: rc=%d, want 0", arg, rc)
			}
		})
	}
}

// TestNotifyMuteSubcmd_BadInput: garbage durations exit rc=2 with a
// usage-style error on stderr.
func TestNotifyMuteSubcmd_BadInput(t *testing.T) {
	var rc int
	stderr := captureStderr(t, func() {
		rc = notifyMuteSubcmd([]string{"not-a-duration"})
	})
	if rc != 2 {
		t.Errorf("rc=%d, want 2", rc)
	}
	if !strings.Contains(stderr, "invalid duration") {
		t.Errorf("stderr = %q, want \"invalid duration\"", stderr)
	}
}
