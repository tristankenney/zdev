package tmuxq

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestPaneWidth_DefaultConstant pins the fallback width to the same
// 50 the bash zdev-sidebar-toggle uses for ZDEV_SIDEBAR_WIDTH.
func TestPaneWidth_DefaultConstant(t *testing.T) {
	if DefaultWidth != 50 {
		t.Errorf("DefaultWidth = %d, want 50", DefaultWidth)
	}
}

// TestPaneWidth_NoTmuxOnPath synthesises a controlled environment by
// clearing PATH so exec.LookPath fails. PaneWidth must return
// DefaultWidth and a non-nil error so the caller can log the cause
// while still using the returned int.
func TestPaneWidth_NoTmuxOnPath(t *testing.T) {
	t.Setenv("PATH", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := PaneWidth(ctx)
	if err == nil {
		t.Error("expected error when tmux not on PATH; got nil")
	}
	if n != DefaultWidth {
		t.Errorf("n = %d, want DefaultWidth=%d", n, DefaultWidth)
	}
}

// TestPaneWidth_LiveTmux runs the real subprocess when tmux is on PATH
// and we're inside a tmux session. Skips otherwise — the test env on
// CI workstations may have neither.
func TestPaneWidth_LiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	if os.Getenv("TMUX") == "" {
		t.Skip("not running inside a tmux session ($TMUX unset)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := PaneWidth(ctx)
	if err != nil {
		t.Fatalf("PaneWidth: %v", err)
	}
	if n <= 0 {
		t.Errorf("n = %d, want > 0 inside tmux", n)
	}
}

// TestPaneWidth_RespectContextTimeout uses an already-expired context
// to confirm PaneWidth never hangs. The exact return value depends on
// whether the subprocess started before the ctx deadline propagated;
// the contract is "no hang, returns a sensible fallback".
func TestPaneWidth_RespectContextTimeout(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	// 1ns timeout — any real subprocess invocation exceeds it.
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	n, _ := PaneWidth(ctx)
	if n < 0 {
		t.Errorf("n = %d, want non-negative", n)
	}
}
