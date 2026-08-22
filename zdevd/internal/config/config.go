// Package config implements the daemon's startup-only TOML configuration
// surface (CONFIG-01..05).
//
// Contract:
//
//   - CONFIG-01: file-not-found is non-fatal — Defaults() with env overrides
//     applied is returned silently.
//   - CONFIG-02: 12 documented keys decode flat (no nested tables).
//   - CONFIG-03: hybrid env/TOML for user-facing keys (ZDEV_WORKSPACE,
//     ZDEV_SIDEBAR_WIDTH, and legacy glyph overrides);
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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
)

// Config is decoded from ~/.config/zdev/sidebar.toml. The flat snake_case
// keys map 1:1 to the documented schema. CONFIG-02 historically restricted
// the file to flat keys; the [[agent]] table (CONFIG-06, 2026-05) adds the
// first nested structure so users can register new agents (claude,
// opencode, future N) without recompiling.
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

	// Collapse configures the grouped sidebar's fold behavior
	// (ZDEV_SIDEBAR_GROUP=collapse — the env knob remains the master
	// switch; this section tunes WHICH groups fold). Nested table:
	//
	//	[collapse]
	//	initiatives = true          # fold initiative groups (default true)
	//	containers  = true          # fold homeless containers, e.g.
	//	                            # projects/ (default true)
	//	expand      = ["marketplace"]  # group keys pinned open, never fold
	//
	// Attention and attendance ALWAYS pierce regardless of settings —
	// there is deliberately no way to configure a group that hides a
	// waiting or dead agent.
	Collapse CollapseConfig `toml:"collapse"`

	// ShowUnmanaged is set by ZDEV_SIDEBAR_UNMANAGED=show. When true, tmux
	// sessions that have no corresponding projects-file entry are rendered
	// below the managed block, dimmed. Default false (hide) preserves
	// existing sidebar behavior. ENV-only; no TOML key (operators toggle
	// at shell level, not via sidebar.toml restart).
	ShowUnmanaged bool `toml:"-"`

	// Agents is the multi-agent registry (CONFIG-06). Entries overlay the
	// built-ins by name; new names append and enabled=false removes.
	Agents []AgentSpec `toml:"agent"`
}

// AgentSpec is one entry under [[agent]] in sidebar.toml. Drives:
//
//   - agents.Registry.Classify: marker-glyph → agent-name mapping
//   - the sidebar chip per attributed agent (glyph)
//   - bin/zdev's default agent-launcher selection (launch line + PATH probe)
//
// All fields except Name are optional. A new entry with only Name exists for
// hook-driven state; an entry matching a built-in replaces that entire spec.
type AgentSpec struct {
	// Name is the agent identifier used as the map key on the wire and
	// passed to zdev-notify as its first arg. Lowercase, no spaces.
	Name string `toml:"name"`

	// Enabled removes a built-in agent when explicitly false. Nil and true
	// both mean enabled. A disabled entry needs only name + enabled=false.
	Enabled *bool `toml:"enabled"`

	// Glyph is the single-character icon shown in the sidebar chip when
	// the agent is attributed to a pane (e.g., "✻", "○", "π").
	Glyph string `toml:"glyph"`

	// WaitingMarkers lists pane-title prefixes (including any trailing
	// space the agent uses) that mean "this agent is blocking on the
	// user". Examples: "● claude ", "✳ ".
	WaitingMarkers []string `toml:"waiting_markers"`

	// FinishedMarkers lists pane-title prefixes that mean "this agent
	// has finished its task and is presenting a result". Example: "◆ ".
	FinishedMarkers []string `toml:"finished_markers"`

	// SpinnerMarkers lists pane-title prefixes meaning "this agent is
	// actively working". Typically Braille U+2800–U+28FF prefixes.
	SpinnerMarkers []string `toml:"spinner_markers"`

	// Command is the executable used for PATH probing before Launch runs.
	// Set it when Launch begins with a wrapper such as sh or env. When empty,
	// the first word of Launch is used.
	Command string `toml:"command"`

	// Launch is the shell command that `bin/zdev` invokes to start the
	// agent pane when this agent is the chosen default. Empty means
	// the agent is detection-only (no auto-launch).
	Launch string `toml:"launch"`
}

// BuiltinAgents returns the default agent registry — used when the user's
// sidebar.toml has no [[agent]] entries. The first entry whose binary is
// on $PATH wins the auto-launch in bin/zdev (claude before opencode).
//
// The canonical defaults live in agents.Builtin(); this function projects
// them into the TOML-tagged AgentSpec shape the config loader expects.
func BuiltinAgents() []AgentSpec {
	specs := agents.Builtin()
	out := make([]AgentSpec, len(specs))
	for i, s := range specs {
		out[i] = AgentSpec{
			Name:            s.Name,
			Glyph:           s.Glyph,
			WaitingMarkers:  s.WaitingMarkers,
			FinishedMarkers: s.FinishedMarkers,
			SpinnerMarkers:  s.SpinnerMarkers,
			Command:         s.Command,
			Launch:          s.Launch,
		}
	}
	return out
}

// CollapseConfig tunes which groups fold under ZDEV_SIDEBAR_GROUP=collapse.
// Pointer booleans distinguish "absent" (default true) from an explicit
// false — a TOML bool cannot otherwise express unset.
type CollapseConfig struct {
	Initiatives *bool    `toml:"initiatives"`
	Containers  *bool    `toml:"containers"`
	Expand      []string `toml:"expand"`
}

// CollapseInitiatives resolves the initiatives fold setting (default true).
func (c CollapseConfig) CollapseInitiatives() bool {
	return c.Initiatives == nil || *c.Initiatives
}

// CollapseContainers resolves the containers fold setting (default true).
func (c CollapseConfig) CollapseContainers() bool {
	return c.Containers == nil || *c.Containers
}

// Defaults returns the code-defined fallback values used when no TOML file is
// present or a key is missing from the file. Numeric/list/glyph values match
// the Phase 3 bash baseline + REQUIREMENTS DATA-01/03/06/08/09 + VIS-12.
func Defaults() Config {
	return Config{
		Workspace:         os.Getenv("HOME") + "/workspace",
		Width:             50,
		StaleSeconds:      3600,                                                                // VIS-12
		WaitWarnSeconds:   60,                                                                  // DATA-09 (≥60s orange)
		WaitUrgentSeconds: 300,                                                                 // DATA-09 (≥300s red)
		PortsMax:          4,                                                                   // DATA-06
		DefaultBranches:   []string{"main", "master", "develop", "trunk"},                      // DATA-01
		DefaultShells:     []string{"zsh", "bash", "sh", "fish", "claude", "claude.exe", "pi"}, // DATA-03 (260512-cpa: codex→pi)
		PRRefreshSeconds:  300,
		GitFloorSeconds:   10,
		ClaudeGlyph:       "✻", // DATA-08
		PiGlyph:           "π", // DATA-08 (260512-cpa: was ◉ for codex)
		// Agents is intentionally left nil here; callers should consult
		// EffectiveAgents() which substitutes BuiltinAgents() when empty.
		// Keeping the field nil at the Defaults() layer means a user's
		// [[agent]] block in TOML wins via a simple zero-vs-non-zero check.
	}
}

// EffectiveAgents overlays user entries onto the built-ins. A new name is
// appended, a matching name MERGES onto the built-in field by field —
// every field except name is optional, so a glyph-only override keeps the
// built-in's markers, command, and launch — and enabled=false removes it.
// Presence-awareness: an omitted list (nil) keeps the built-in's; an
// explicitly empty list (waiting_markers = []) clears it. This makes
// adding one agent independent from the rest of the registry and gives
// removal an explicit, reviewable form.
func (c Config) EffectiveAgents() []AgentSpec {
	out := BuiltinAgents()
	index := make(map[string]int, len(out))
	for i, spec := range out {
		index[strings.ToLower(strings.TrimSpace(spec.Name))] = i
	}
	removed := make(map[string]bool)
	for _, spec := range c.Agents {
		name := strings.ToLower(strings.TrimSpace(spec.Name))
		if name == "" {
			continue
		}
		spec.Name = name
		if spec.Enabled != nil && !*spec.Enabled {
			removed[name] = true
			continue
		}
		delete(removed, name)
		if i, ok := index[name]; ok {
			merged := out[i]
			if spec.Glyph != "" {
				merged.Glyph = spec.Glyph
			}
			if spec.WaitingMarkers != nil {
				merged.WaitingMarkers = spec.WaitingMarkers
			}
			if spec.FinishedMarkers != nil {
				merged.FinishedMarkers = spec.FinishedMarkers
			}
			if spec.SpinnerMarkers != nil {
				merged.SpinnerMarkers = spec.SpinnerMarkers
			}
			if spec.Command != "" {
				merged.Command = spec.Command
			}
			if spec.Launch != "" {
				merged.Launch = spec.Launch
			}
			merged.Enabled = spec.Enabled
			out[i] = merged
		} else {
			index[name] = len(out)
			out = append(out, spec)
		}
	}
	filtered := out[:0]
	for _, spec := range out {
		if !removed[spec.Name] {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

// AgentRegistry returns the freshly-built agents.Registry that the rest
// of the daemon (hub, renderer, classifier) should consume. It bridges
// the config-side AgentSpec onto the runtime agents.Spec — same data,
// different home package, to avoid an import cycle (agents must not
// import config since hub eventually pulls agents in via state).
func (c Config) AgentRegistry() *agents.Registry {
	specs := c.EffectiveAgents()
	runtime := make([]agents.Spec, len(specs))
	for i, s := range specs {
		runtime[i] = agents.Spec{
			Name:            s.Name,
			Glyph:           s.Glyph,
			WaitingMarkers:  s.WaitingMarkers,
			FinishedMarkers: s.FinishedMarkers,
			SpinnerMarkers:  s.SpinnerMarkers,
			Command:         s.Command,
			Launch:          s.Launch,
		}
	}
	return agents.NewRegistry(runtime)
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

	if err := validateAgents(cfg.Agents); err != nil {
		slog.Error("config: invalid [[agent]] entry", "path", path, "err", err)
		return Config{}, err
	}

	return applyEnvOverrides(cfg), nil
}

// commandToken permits exactly one executable token for AgentSpec.Command:
// a bare name or a path, no whitespace, no shell metacharacters, not
// option-like. The value crosses zdev-show's tab-separated line protocol
// into a generated `command -v <token>` shell fragment (bin/zdev), so
// anything looser can change record boundaries or the generated program.
var commandToken = regexp.MustCompile(`^[A-Za-z0-9._+/-]+$`)

// validateAgents rejects agent specs whose fields would corrupt the
// zdev-show agents line protocol or the shell chain built from it. Launch
// stays deliberately free-form (it is the user's own launch command and is
// exec'd, not quoted) except that it must be a single line — a newline
// splits the tab-separated record; a tab inside Launch is harmless because
// the consumer's read slurps the remainder of the line into that field.
func validateAgents(specs []AgentSpec) error {
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if spec.Command != "" && (!commandToken.MatchString(spec.Command) || strings.HasPrefix(spec.Command, "-")) {
			return fmt.Errorf("agent %q: command %q must be a single executable name or path (no whitespace, shell metacharacters, or leading dash)", name, spec.Command)
		}
		if strings.ContainsAny(spec.Launch, "\n\r") {
			return fmt.Errorf("agent %q: launch must be a single line", name)
		}
		if strings.ContainsAny(name, " \t\n\r") {
			return fmt.Errorf("agent %q: name must not contain whitespace", name)
		}
	}
	return nil
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
	cfg.ShowUnmanaged = os.Getenv("ZDEV_SIDEBAR_UNMANAGED") == "show"
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
