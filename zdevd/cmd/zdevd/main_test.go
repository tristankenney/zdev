// cmd/zdevd/main_test.go
//
// Unit tests for path helpers in main.go. No I/O — pure path-formatting.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultStatePath_ShapeMatchesSocket asserts the per-platform state
// path shape. On darwin, state and socket share one Application Support
// dir; on Linux the XDG layout deliberately SPLITS them (state under
// $XDG_STATE_HOME, socket under $XDG_RUNTIME_DIR) — first caught by CI:
// this test asserted the darwin shape unconditionally and failed every
// Linux run.
func TestDefaultStatePath_ShapeMatchesSocket(t *testing.T) {
	got := defaultStatePath()

	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME not set")
	}

	const wantFile = "zdevd-state.json"
	if !strings.HasSuffix(got, wantFile) {
		t.Errorf("defaultStatePath() = %q, want suffix %q", got, wantFile)
	}

	switch runtime.GOOS {
	case "darwin":
		wantDir := filepath.Join(home, "Library", "Application Support", "zdev")
		if !strings.HasPrefix(got, wantDir) {
			t.Errorf("defaultStatePath() = %q, want path under %q", got, wantDir)
		}
		if socketDir, stateDir := filepath.Dir(defaultSocketPath()), filepath.Dir(got); socketDir != stateDir {
			t.Errorf("defaultStatePath() dir = %q, defaultSocketPath() dir = %q; darwin expects same dir",
				stateDir, socketDir)
		}
	case "linux":
		if os.Getenv("XDG_STATE_HOME") == "" && !strings.Contains(got, filepath.Join("state", "zdev")) {
			t.Errorf("defaultStatePath() = %q, want a path under ~/.local/state/zdev", got)
		}
	}
}
