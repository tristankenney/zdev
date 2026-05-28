//go:build live

// Package livehelpers contains shared scaffolding for //go:build live tests
// across the zdevd module. It exists ONLY under the live build tag so it
// does not pollute default test compilation. Both internal/socket and
// cmd/zdev-sidebar live tests import these helpers to avoid duplicating
// ~30 LOC of process-spawn + wait-for-bind logic.
//
// None of these helpers run under `make test` — they require a built binary
// (`make build`), real subprocesses, and real Unix sockets.
//
// Plan 04-06 (ARCH-07 / ARCH-09 SC2 drills) introduces this package. Adding
// a new live test? Reuse BuildDaemon / StartDaemon / WaitForListening (or the
// SetupDaemon convenience wrapper) here rather than re-rolling them per-test.
package livehelpers

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// BuildDaemon stats $MODULE_ROOT/bin/zdevd; if missing, runs `make build`
// from the module root. Returns the absolute binary path. Subsequent calls
// in the same test binary are O(1) — they stat-only.
//
// The module root is discovered by walking up from the test's CWD until a
// go.mod is found. Tests do not need to chdir into the zdevd directory.
func BuildDaemon(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("could not find go.mod ancestor of %s", wd)
		}
		root = parent
	}
	binPath := filepath.Join(root, "bin", "zdevd")
	if _, err := os.Stat(binPath); err == nil {
		return binPath
	}
	cmd := exec.Command("make", "build")
	cmd.Dir = root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("make build: %v", err)
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("expected binary at %s after make build, got: %v", binPath, err)
	}
	return binPath
}

// StartDaemon spawns the daemon under exec.CommandContext with HOME and
// XDG_*_HOME isolated to homeDir. Caller controls the cmd lifecycle —
// register your own t.Cleanup to send SIGTERM/SIGKILL and Wait. SetupDaemon
// (below) is the convenience wrapper that handles cleanup automatically;
// only call StartDaemon directly when scripting kill -9 mid-test.
//
// stdout / stderr are routed to os.Stderr so daemon log output appears
// alongside the test output (use `go test -v` to see it).
//
// Inherited env is filtered: TMUX, TMUX_PANE, and ZDEVD_DEBOUNCE_MS are
// stripped so tests run from inside a tmux pane don't trigger the daemon's
// recursion guard (tmuxctl: refusing to start with TMUX env var set) and
// don't inherit a debounce override that would skew timing-sensitive tests.
func StartDaemon(t *testing.T, ctx context.Context, binaryPath, homeDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, binaryPath)
	cmd.Env = filteredEnv(homeDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	return cmd
}

// StartDaemonWithTmuxSocket spawns the daemon under exec.CommandContext with
// HOME / XDG_*_HOME / TMPDIR isolated to homeDir AND the tmux socket name
// injected via the ZDEVD_TMUX_SOCKET env var. The daemon's supervisor will
// route through `tmux -L <tmuxSocket>` instead of the user's default tmux
// server. Caller controls the cmd lifecycle — register your own t.Cleanup
// to send SIGTERM/SIGKILL and Wait. SetupDaemonWithTmuxSocket (below) is the
// convenience wrapper that handles cleanup automatically.
//
// Plan 04.1 (CONTEXT D-04): added so live tests can isolate the daemon's
// tmux socket from the user's real tmux server. Existing StartDaemon stays
// unchanged for back-compat.
//
// If tmuxSocket is the empty string, the env var is set to "" and the
// daemon's WithSocketName option short-circuits — equivalent to calling
// StartDaemon directly. Callers should always pass a non-empty value.
func StartDaemonWithTmuxSocket(t *testing.T, ctx context.Context, binaryPath, homeDir, tmuxSocket string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, binaryPath)
	cmd.Env = append(filteredEnv(homeDir), "ZDEVD_TMUX_SOCKET="+tmuxSocket)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	return cmd
}

// filteredEnv builds the daemon's environment from the test's environment
// minus the recursion-guard, debounce-override, and host-shared TMPDIR
// variables, plus the HOME / XDG_*_HOME / TMPDIR isolation overrides.
//
// TMPDIR is overridden because the daemon's notif watcher (Phase 3) does
// fsnotify.Add($TMPDIR) at startup. The host's TMPDIR is shared with the
// developer's tmux session and the zdev-sidebar-toggle script, which races
// mkdir/rmdir of lock files there. fsnotify v1.10 kqueue races with that
// directory churn during Add and surfaces transient lstat errors that
// fatal-exit the daemon. Using a per-test TMPDIR isolates the watcher
// from host activity and makes the live tests deterministic.
func filteredEnv(homeDir string) []string {
	skip := map[string]struct{}{
		"TMUX":              {},
		"TMUX_PANE":         {},
		"ZDEVD_DEBOUNCE_MS": {},
		"HOME":              {},
		"XDG_STATE_HOME":    {},
		"XDG_CONFIG_HOME":   {},
		"TMPDIR":            {},
	}
	parent := os.Environ()
	out := make([]string, 0, len(parent)+4)
	for _, kv := range parent {
		eq := -1
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				eq = i
				break
			}
		}
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if _, drop := skip[key]; drop {
			continue
		}
		out = append(out, kv)
	}
	tmpDir := filepath.Join(homeDir, "tmp")
	_ = os.MkdirAll(tmpDir, 0o700)
	out = append(out,
		"HOME="+homeDir,
		"XDG_STATE_HOME="+filepath.Join(homeDir, ".local", "state"),
		"XDG_CONFIG_HOME="+filepath.Join(homeDir, ".config"),
		"TMPDIR="+tmpDir,
	)
	return out
}

// WaitForListening polls net.Dial("unix", socketPath) every 25ms until
// success or timeout. Returns the elapsed duration on success.
//
// Uses Dial (not Stat) because the socket file can be present before the
// daemon has called Listen — Stat would race and return success while a
// subsequent Subscribe still fails with ECONNREFUSED.
func WaitForListening(t *testing.T, socketPath string, timeout time.Duration) (time.Duration, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	start := time.Now()
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			_ = conn.Close()
			return time.Since(start), nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return time.Since(start), fmt.Errorf("daemon did not bind %s within %v", socketPath, timeout)
}

// IsolatedHome creates a fresh per-test HOME directory under /tmp (NOT
// t.TempDir()) and registers cleanup. We deliberately avoid t.TempDir()
// because macOS unix-socket paths are capped at 104 bytes (sys/un.h
// SUN_LEN); t.TempDir() lives under /var/folders/.../T/TestName/NNN/...
// which alone is ~85 chars BEFORE the daemon appends "Library/Application
// Support/zdev/zdevd.sock" (another 47 chars) → bind() fails with EINVAL.
//
// /tmp/zdevd-live-XXXX is ~21 chars, leaving plenty of headroom for the
// daemon's defaultSocketPath() suffix.
func IsolatedHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zdevd-live-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// SocketPathFor returns the socket path the daemon will bind given a HOME
// override — mirrors cmd/zdevd/main.go's defaultSocketPath() exactly:
// $HOME/Library/Application Support/zdev/zdevd.sock on macOS.
func SocketPathFor(homeDir string) string {
	return filepath.Join(homeDir, "Library", "Application Support", "zdev", "zdevd.sock")
}

// SetupDaemon is the all-in-one convenience helper. Returns the running cmd
// and the absolute socket path; registers t.Cleanup for SIGTERM + Wait
// (with SIGKILL fallback after 3s). Use this when a test does NOT need to
// script a kill -9 mid-execution.
//
// The socket path mirrors cmd/zdevd/main.go's defaultSocketPath() exactly:
// $HOME/Library/Application Support/zdev/zdevd.sock on macOS.
func SetupDaemon(t *testing.T, ctx context.Context) (*exec.Cmd, string) {
	t.Helper()
	homeDir := IsolatedHome(t)
	binaryPath := BuildDaemon(t)
	cmd := StartDaemon(t, ctx, binaryPath, homeDir)
	socketPath := SocketPathFor(homeDir)
	if _, err := WaitForListening(t, socketPath, 5*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatalf("SetupDaemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	return cmd, socketPath
}

// SetupDaemonWithTmuxSocket is the all-in-one convenience helper that
// isolates BOTH the daemon's HOME and its tmux socket. Returns the running
// cmd and the absolute socket path; registers t.Cleanup for SIGTERM + Wait
// (with SIGKILL fallback after 3s). Use this for live tests that should not
// touch the user's default tmux server (D-01 / D-04).
//
// The per-test tmux socket cleanup (`tmux -L <tmuxSocket> kill-server`)
// is the test's responsibility — register it as a separate t.Cleanup BEFORE
// calling this function so it runs AFTER the daemon-reap cleanup (Go runs
// t.Cleanup LIFO; per CONTEXT D-06 / specifics, the daemon's tmux subprocess
// must see clean EOF rather than mid-shutdown SIGPIPE).
func SetupDaemonWithTmuxSocket(t *testing.T, ctx context.Context, tmuxSocket string) (*exec.Cmd, string) {
	t.Helper()
	homeDir := IsolatedHome(t)
	binaryPath := BuildDaemon(t)
	cmd := StartDaemonWithTmuxSocket(t, ctx, binaryPath, homeDir, tmuxSocket)
	socketPath := SocketPathFor(homeDir)
	if _, err := WaitForListening(t, socketPath, 5*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatalf("SetupDaemonWithTmuxSocket: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	return cmd, socketPath
}
