// Package config implements the daemon's startup-only TOML configuration
// surface (CONFIG-01..05).
//
// Contract:
//
//   - CONFIG-01: file-not-found is non-fatal — Defaults() with env overrides
//     applied is returned silently.
//   - CONFIG-02: 12 documented keys decode flat (no nested tables).
//   - CONFIG-03: hybrid env/TOML for 4 user-facing keys (ZDEV_WORKSPACE,
//     ZDEV_SIDEBAR_WIDTH, ZDEV_SIDEBAR_CLAUDE_GLYPH, ZDEV_SIDEBAR_PI_GLYPH);
//     env wins when set. The 8 cadence/threshold keys are TOML-only (D4-13).
//   - CONFIG-04: unknown keys are logged at WARN via MetaData.Undecoded() and
//     ignored — config still loads.
//   - CONFIG-05: load-once at startup; restart required for changes (no hot
//     reload). Operator workflow is `launchctl kickstart -k`.
//
// D4-14 (parse error refuses startup): when ~/.config/zdev/sidebar.toml
// exists but parses with an error, Load logs structured slog.Error with
// line/col context and returns a non-nil error so the daemon's run() returns
// non-nil and main exits 1. launchd KeepAlive=Crashed:true respawns;
// ThrottleInterval=30 prevents flapping.
package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the flat schema decoded from ~/.config/zdev/sidebar.toml. The 12
// snake_case TOML keys map 1:1 to the documented sidebar.toml schema; no
// nested tables (CONFIG-02).
type Config struct {
	Workspace         string   `toml:"workspace"`
	Width             int      `toml:"width"`
	StaleSeconds      int      `toml:"stale_seconds"`
	WaitWarnSeconds   int      `toml:"wait_warn_seconds"`
	WaitUrgentSeconds int      `toml:"wait_urgent_seconds"`
	PortsMax          int      `toml:"ports_max"`
	DefaultBranches   []string `toml:"default_branches"`
	DefaultShells     []string `toml:"default_shells"`
	PRRefreshSeconds  int      `toml:"pr_refresh_seconds"`
	GitFloorSeconds   int      `toml:"git_floor_seconds"`
	ClaudeGlyph       string   `toml:"claude_glyph"`
	PiGlyph           string   `toml:"pi_glyph"` // 260512-cpa: was codex_glyph
}

// Defaults returns the code-defined fallback values used when no TOML file is
// present or a key is missing from the file. Numeric/list/glyph values match
// the Phase 3 bash baseline + REQUIREMENTS DATA-01/03/06/08/09 + VIS-12.
func Defaults() Config {
	return Config{
		Workspace:         os.Getenv("HOME") + "/workspace",
		Width:             50,
		StaleSeconds:      3600, // VIS-12
		WaitWarnSeconds:   60,   // DATA-09 (≥60s orange)
		WaitUrgentSeconds: 300,  // DATA-09 (≥300s red)
		PortsMax:          4,    // DATA-06
		DefaultBranches:   []string{"main", "master", "develop", "trunk"},                                     // DATA-01
		DefaultShells:     []string{"zsh", "bash", "sh", "fish", "claude", "claude.exe", "pi"}, // DATA-03 (260512-cpa: codex→pi)
		PRRefreshSeconds:  300,
		GitFloorSeconds:   10,
		ClaudeGlyph:       "✻", // DATA-08
		PiGlyph:           "π", // DATA-08 (260512-cpa: was ◉ for codex)
	}
}

// Load reads a TOML config file at path and returns the merged config.
//
// Behavior matrix:
//   - File missing: return Defaults()+env overrides, nil error (CONFIG-01).
//   - Parse error: log slog.Error with line/col (D4-14, Pitfall 2 mitigation),
//     return zero-value Config and the error.
//   - Unknown keys: log slog.Warn per key, continue (CONFIG-04).
//   - Success: layer env overrides on the decoded Config (CONFIG-03), return.
func Load(path string) (Config, error) {
	cfg := Defaults()

	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
			// CONFIG-01: silent default when no file present.
			return applyEnvOverrides(cfg), nil
		}
		// D4-14: parse failure surfaces with line/col when the underlying
		// error is a *toml.ParseError. Type-mismatch on a known key reaches
		// the generic decode-error branch; the caller (main) exits 1 in
		// either path so launchd KeepAlive throttles per ThrottleInterval=30.
		var pe toml.ParseError
		if errors.As(err, &pe) {
			slog.Error("config parse failed",
				"path", path,
				"line", pe.Position.Line,
				"col", pe.Position.Col,
				"msg", pe.Message,
				"context", pe.ErrorWithPosition(),
			)
		} else {
			slog.Error("config decode failed", "path", path, "err", err)
		}
		return Config{}, err
	}

	// CONFIG-04: WARN on every unknown key (table or scalar). md.Undecoded
	// returns a []toml.Key; toml.Key is a []string, so .String() joins the
	// dotted path for readable logging.
	for _, key := range md.Undecoded() {
		slog.Warn("config: unknown key (ignored)", "path", path, "key", key.String())
	}

	return applyEnvOverrides(cfg), nil
}

// applyEnvOverrides layers CONFIG-03's 4 hybrid env vars on top of cfg. Env
// wins over TOML when set to a non-empty value. The 8 cadence/threshold keys
// are intentionally TOML-only per D4-13 (calibration knobs that take effect
// via `launchctl kickstart -k`, not ad-hoc shell exports).
//
// Env-var failures (e.g., ZDEV_SIDEBAR_WIDTH="oops") are SOFT — the original
// TOML/default value is preserved. We don't loud-fail on env vars because
// they're a convenience layer; the strict-error path is reserved for TOML
// parse failures (D4-14).
func applyEnvOverrides(cfg Config) Config {
	if v := os.Getenv("ZDEV_WORKSPACE"); v != "" {
		cfg.Workspace = v
	}
	if v := os.Getenv("ZDEV_SIDEBAR_WIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Width = n
		}
	}
	if v := os.Getenv("ZDEV_SIDEBAR_CLAUDE_GLYPH"); v != "" {
		cfg.ClaudeGlyph = v
	}
	if v := os.Getenv("ZDEV_SIDEBAR_PI_GLYPH"); v != "" {
		cfg.PiGlyph = v
	}
	return cfg
}

// DefaultPath returns the canonical sidebar.toml location. XDG_CONFIG_HOME
// takes precedence when set (XDG Base Directory spec); otherwise falls back
// to ~/.config/zdev/sidebar.toml.
func DefaultPath() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "zdev", "sidebar.toml")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "zdev", "sidebar.toml")
}
