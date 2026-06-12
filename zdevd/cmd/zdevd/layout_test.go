package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
)

func TestParseInventory(t *testing.T) {
	// Two work panes + a sidebar. The sidebar's title intentionally
	// contains the '|' delimiter to exercise the bounded split (title is
	// the final field and must survive intact). Window-level width/session
	// repeat per row and are taken from the first.
	// 10th field is @zdev-team (empty here — not a teammate window).
	out := strings.Join([]string{
		"%9|0|0|50|50|0|1|240|work||zdev-sidebar",
		"%0|51|0|94|50|1|0|240|work||nvim | src/main.go",
		"%1|146|0|94|50|0|0|240|work||bash",
	}, "\n")

	win, ok := parseInventory("@3", out)
	if !ok {
		t.Fatal("parseInventory returned ok=false on valid input")
	}
	if win.ID != "@3" || win.Session != "work" || win.EffectiveWidth != 240 {
		t.Errorf("window-level fields wrong: id=%q session=%q width=%d", win.ID, win.Session, win.EffectiveWidth)
	}
	if len(win.Panes) != 3 {
		t.Fatalf("got %d panes, want 3", len(win.Panes))
	}
	sb := win.Panes[0]
	if sb.ID != "%9" || sb.Left != 0 || sb.Width != 50 || !sb.SidebarOpt {
		t.Errorf("sidebar pane parsed wrong: %+v", sb)
	}
	mid := win.Panes[1]
	if mid.ID != "%0" || !mid.Active || mid.Title != "nvim | src/main.go" {
		t.Errorf("middle pane parsed wrong: %+v", mid)
	}
}

func TestParseInventoryEmpty(t *testing.T) {
	if _, ok := parseInventory("@3", ""); ok {
		t.Error("expected ok=false for empty output")
	}
	if _, ok := parseInventory("@3", "\n  \n"); ok {
		t.Error("expected ok=false for whitespace-only output")
	}
	// Malformed (too few fields) rows are skipped; a block of only such
	// rows yields no usable panes.
	if _, ok := parseInventory("@3", "%0|0|0"); ok {
		t.Error("expected ok=false when every row is malformed")
	}
}

func TestResolveSidebarCommand(t *testing.T) {
	// Executable present → `exec <path>`.
	dir := t.TempDir()
	bin := filepath.Join(dir, "render")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZDEV_SIDEBAR_RENDER", bin)
	if got := resolveSidebarCommand(); got != "exec "+bin {
		t.Errorf("executable path: got %q, want %q", got, "exec "+bin)
	}

	// Missing/non-executable → loud error pane command (printf + idle
	// loop), NOT a bare exec that would die back to a blank pane.
	missing := filepath.Join(dir, "nope")
	t.Setenv("ZDEV_SIDEBAR_RENDER", missing)
	got := resolveSidebarCommand()
	if !strings.HasPrefix(got, "printf %b ") || !strings.Contains(got, "while :; do sleep") {
		t.Errorf("missing binary: expected a printf+idle error command, got %q", got)
	}
	if strings.HasPrefix(got, "exec ") {
		t.Errorf("missing binary must NOT produce a bare exec (silent-blank risk): %q", got)
	}

	// A non-executable regular file is treated as missing.
	noexec := filepath.Join(dir, "plain")
	if err := os.WriteFile(noexec, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZDEV_SIDEBAR_RENDER", noexec)
	if got := resolveSidebarCommand(); strings.HasPrefix(got, "exec ") {
		t.Errorf("non-executable file should not be exec'd: %q", got)
	}
}

func TestShSingleQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"a b c":     "'a b c'",
		"it's here": `'it'\''s here'`,
		"":          "''",
		`a'b'c`:     `'a'\''b'\''c'`,
	}
	for in, want := range cases {
		if got := shSingleQuote(in); got != want {
			t.Errorf("shSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLockDirSanitizes(t *testing.T) {
	// tmux window ids carry '@' / '%' / ':' which aren't path-safe; they
	// must collapse to '_' so the lock path is a single directory name.
	got := lockDir("@3")
	base := filepath.Base(got)
	if strings.ContainsAny(base, "@%:") {
		t.Errorf("lockDir base still has unsafe chars: %q", base)
	}
	if !strings.HasPrefix(base, "zdev-sidebar-") || !strings.HasSuffix(base, ".lock") {
		t.Errorf("unexpected lock name: %q", base)
	}
	// Distinct windows get distinct locks; same window is stable.
	if lockDir("@3") != got {
		t.Error("lockDir not stable for the same window id")
	}
	if lockDir("@4") == got {
		t.Error("lockDir collided for different window ids")
	}
}

func TestConfigFromEnvWiring(t *testing.T) {
	// The subcommand feeds os.LookupEnv into the pure parser; confirm the
	// wiring honors a set value and falls back otherwise.
	t.Setenv("ZDEV_SIDEBAR_THRESHOLD", "190")
	os.Unsetenv("ZDEV_SIDEBAR_WIDTH")
	cfg := layout.ConfigFromEnv(os.LookupEnv)
	if cfg.Threshold != 190 {
		t.Errorf("threshold not read from env: %d", cfg.Threshold)
	}
	if cfg.Width != layout.DefaultWidth {
		t.Errorf("width should default: %d", cfg.Width)
	}
}
