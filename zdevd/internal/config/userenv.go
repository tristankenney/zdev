package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ApplyUserEnv gap-fills the process environment from the persisted
// settings file ~/.config/zdev/env (written by install.sh's prompts).
// Called once at the top of main() in zdevd and zdev-sidebar so every
// existing os.Getenv reader sees the settings — neither process can get
// them any other way: launchd/systemd jobs don't inherit the user's
// shell env, and renderer panes are spawned by the tmux SERVER, which
// never sources rc files. This is the Go mirror of the loader at the
// top of bin/zdev and bin/zdev-sidebar-toggle.
//
// Rules:
//   - a key already set in the real environment ALWAYS wins;
//   - only ZDEV-prefixed keys are applied (ZDEV_*, ZDEVD_*) — the file
//     is user-edited, and gap-filling arbitrary keys (PATH, LANG, …)
//     from it would be a foot-gun;
//   - '#' comments, blank lines, and malformed lines are skipped;
//   - values are used verbatim (no quoting or expansion — install.sh
//     writes absolute paths).
func ApplyUserEnv() {
	cfgDir := os.Getenv("XDG_CONFIG_HOME")
	if cfgDir == "" {
		cfgDir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	b, err := os.ReadFile(filepath.Join(cfgDir, "zdev", "env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !strings.HasPrefix(k, "ZDEV") {
			continue
		}
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
