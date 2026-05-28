// cmd/zdevd/main_test.go
//
// Unit tests for path helpers in main.go. No I/O — pure path-formatting.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultStatePath_ShapeMatchesSocket asserts that defaultStatePath()
// returns a path under $HOME/Library/Application Support/zdev/ ending in
// "zdevd-state.json" — mirrors defaultSocketPath()'s directory exactly.
func TestDefaultStatePath_ShapeMatchesSocket(t *testing.T) {
	got := defaultStatePath()

	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME not set")
	}

	wantDir := filepath.Join(home, "Library", "Application Support", "zdev")
	wantFile := "zdevd-state.json"

	if !strings.HasPrefix(got, wantDir) {
		t.Errorf("defaultStatePath() = %q, want path under %q", got, wantDir)
	}
	if !strings.HasSuffix(got, wantFile) {
		t.Errorf("defaultStatePath() = %q, want suffix %q", got, wantFile)
	}

	// Also verify it shares the same directory as defaultSocketPath.
	socketDir := filepath.Dir(defaultSocketPath())
	stateDir := filepath.Dir(got)
	if socketDir != stateDir {
		t.Errorf("defaultStatePath() dir = %q, defaultSocketPath() dir = %q; expected same dir",
			stateDir, socketDir)
	}
}
