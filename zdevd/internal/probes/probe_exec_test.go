package probes

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestAugmentExecError_NilInput passes nil through unchanged.
func TestAugmentExecError_NilInput(t *testing.T) {
	if got := augmentExecError(nil); got != nil {
		t.Errorf("augmentExecError(nil) = %v, want nil", got)
	}
}

// TestAugmentExecError_NonExitError passes non-ExitError through unchanged.
func TestAugmentExecError_NonExitError(t *testing.T) {
	want := errors.New("context deadline exceeded")
	got := augmentExecError(want)
	if got != want {
		t.Errorf("augmentExecError returned %v, want identity for non-ExitError", got)
	}
}

// TestAugmentExecError_SurfaceStderr runs a real subprocess that exits
// non-zero with content on stderr, then verifies augmentExecError includes
// the stderr tail in the error message. This is the production path that
// previously produced "err=exit status 1" in slog — staff-review M2.
func TestAugmentExecError_SurfaceStderr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2)
	cancel() // immediately cancel to avoid leaving the timer hanging in test
	_ = ctx

	// Use `sh -c` to emit a recognizable stderr message and exit non-zero.
	cmd := exec.Command("sh", "-c", "printf 'auth required: please run gh auth login' >&2; exit 4")
	_, err := cmd.Output()
	if err == nil {
		t.Fatal("expected non-nil error from failing subprocess")
	}
	wrapped := augmentExecError(err)
	if wrapped == nil {
		t.Fatal("augmentExecError returned nil for non-nil error")
	}
	msg := wrapped.Error()
	if !strings.Contains(msg, "exit status 4") {
		t.Errorf("wrapped error missing original status: %q", msg)
	}
	if !strings.Contains(msg, "auth required") {
		t.Errorf("wrapped error missing stderr context: %q", msg)
	}
	// errors.Is must still match the original *exec.ExitError type so callers
	// using errors.As(err, *exec.ExitError) continue to work.
	var ee *exec.ExitError
	if !errors.As(wrapped, &ee) {
		t.Error("errors.As(wrapped, *exec.ExitError) returned false; wrap must preserve the underlying type")
	}
}

// TestAugmentExecError_TruncatesLongStderr asserts that very long stderr
// output is truncated to a slog-friendly length and newlines collapse.
func TestAugmentExecError_TruncatesLongStderr(t *testing.T) {
	// 600 chars of content on stderr including newlines.
	cmd := exec.Command("sh", "-c", "for i in $(seq 1 30); do printf 'line %02d of stderr noise\n' $i >&2; done; exit 1")
	_, err := cmd.Output()
	if err == nil {
		t.Fatal("expected non-nil error from failing subprocess")
	}
	wrapped := augmentExecError(err)
	msg := wrapped.Error()
	if strings.Contains(msg, "\n") {
		t.Errorf("wrapped message must collapse newlines to ' | ': %q", msg)
	}
	if !strings.Contains(msg, "...") {
		t.Errorf("expected truncation marker '...' in long-stderr message, got %q", msg)
	}
	// Sanity: the truncated tail is bounded.
	if len(msg) > 1024 {
		t.Errorf("wrapped message length %d exceeds reasonable bound", len(msg))
	}
}
