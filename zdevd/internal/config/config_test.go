// Package config tests cover the five behaviors named in
// 04-RESEARCH.md "Validation Architecture / Phase Requirements → Test Map":
//
//   - TestConfigFileNotFound  (CONFIG-01)
//   - TestConfigSchema        (CONFIG-02)
//   - TestConfigEnvOverride   (CONFIG-03 / D4-13)
//   - TestConfigUnknownKeysWarn (CONFIG-04)
//   - TestConfigParseError    (D4-14 / Pitfall 2)
//
// Plus two sanity tests:
//
//   - TestDefaultPath         (XDG_CONFIG_HOME helper)
//   - TestDefaultsValues      (locked default values from Defaults())
package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// captureSlog redirects the default slog logger to a buffer for the duration
// of t and restores the original on cleanup. Returns the buffer the caller
// inspects via String() for level + key/value assertions.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// writeTOML drops contents into a temp file inside t.TempDir() and returns
// the absolute path.
func writeTOML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sidebar.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// clearHybridEnv unsets the 4 CONFIG-03 hybrid env vars so the test starts
// from a known-clean baseline. Each test that wants to assert env-clean
// behavior calls this first; tests that exercise env-override semantics call
// t.Setenv directly afterwards.
func clearHybridEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ZDEV_WORKSPACE",
		"ZDEV_SIDEBAR_WIDTH",
		"ZDEV_SIDEBAR_CLAUDE_GLYPH",
		"ZDEV_SIDEBAR_PI_GLYPH",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// ---------------------------------------------------------------------------
// CONFIG-01: file-not-found is non-fatal
// ---------------------------------------------------------------------------

func TestConfigFileNotFound(t *testing.T) {
	clearHybridEnv(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist.toml")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(missing) returned err = %v, want nil (CONFIG-01)", err)
	}
	want := Defaults()
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load(missing) = %+v, want %+v", cfg, want)
	}

	t.Run("env_overrides_apply_when_file_missing", func(t *testing.T) {
		clearHybridEnv(t)
		t.Setenv("ZDEV_WORKSPACE", "/env/workspace")
		t.Setenv("ZDEV_SIDEBAR_WIDTH", "120")
		t.Setenv("ZDEV_SIDEBAR_CLAUDE_GLYPH", "C!")
		t.Setenv("ZDEV_SIDEBAR_PI_GLYPH", "X!")

		got, err := Load(missing)
		if err != nil {
			t.Fatalf("Load(missing) with env: err = %v, want nil", err)
		}
		if got.Workspace != "/env/workspace" {
			t.Errorf("Workspace = %q, want %q", got.Workspace, "/env/workspace")
		}
		if got.Width != 120 {
			t.Errorf("Width = %d, want 120", got.Width)
		}
		if got.ClaudeGlyph != "C!" {
			t.Errorf("ClaudeGlyph = %q, want %q", got.ClaudeGlyph, "C!")
		}
		if got.PiGlyph != "X!" {
			t.Errorf("PiGlyph = %q, want %q", got.PiGlyph, "X!")
		}
		// Non-hybrid keys must remain at Defaults() values — file is missing
		// and there's no env hook for them.
		if got.StaleSeconds != 3600 {
			t.Errorf("StaleSeconds = %d, want 3600 (default)", got.StaleSeconds)
		}
	})
}

// ---------------------------------------------------------------------------
// CONFIG-02: 12-key flat schema decodes verbatim
// ---------------------------------------------------------------------------

func TestConfigSchema(t *testing.T) {
	clearHybridEnv(t)

	const fixture = `
workspace = "/tmp/work"
width = 80
stale_seconds = 7200
wait_warn_seconds = 90
wait_urgent_seconds = 600
ports_max = 8
default_branches = ["main", "trunk"]
default_shells = ["zsh", "fish"]
pr_refresh_seconds = 120
git_floor_seconds = 5
claude_glyph = "C"
pi_glyph = "X"
`
	path := writeTOML(t, fixture)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Config{
		Workspace:         "/tmp/work",
		Width:             80,
		StaleSeconds:      7200,
		WaitWarnSeconds:   90,
		WaitUrgentSeconds: 600,
		PortsMax:          8,
		DefaultBranches:   []string{"main", "trunk"},
		DefaultShells:     []string{"zsh", "fish"},
		PRRefreshSeconds:  120,
		GitFloorSeconds:   5,
		ClaudeGlyph:       "C",
		PiGlyph:           "X",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load(full TOML):\n got  = %+v\n want = %+v", cfg, want)
	}
}

// ---------------------------------------------------------------------------
// CONFIG-03 / D4-13: env-var hybrid for 4 user-facing keys; 8 cadence keys
// remain TOML-only.
// ---------------------------------------------------------------------------

func TestConfigEnvOverride(t *testing.T) {
	clearHybridEnv(t)

	const fixture = `
workspace = "A"
width = 40
claude_glyph = "C"
pi_glyph = "X"
stale_seconds = 1234
`
	path := writeTOML(t, fixture)

	t.Run("env_wins_over_toml", func(t *testing.T) {
		clearHybridEnv(t)
		t.Setenv("ZDEV_WORKSPACE", "B")
		t.Setenv("ZDEV_SIDEBAR_WIDTH", "120")
		t.Setenv("ZDEV_SIDEBAR_CLAUDE_GLYPH", "Cnew")
		t.Setenv("ZDEV_SIDEBAR_PI_GLYPH", "Xnew")

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Workspace != "B" {
			t.Errorf("Workspace = %q, want %q", cfg.Workspace, "B")
		}
		if cfg.Width != 120 {
			t.Errorf("Width = %d, want 120", cfg.Width)
		}
		if cfg.ClaudeGlyph != "Cnew" {
			t.Errorf("ClaudeGlyph = %q, want %q", cfg.ClaudeGlyph, "Cnew")
		}
		if cfg.PiGlyph != "Xnew" {
			t.Errorf("PiGlyph = %q, want %q", cfg.PiGlyph, "Xnew")
		}
	})

	t.Run("invalid_width_keeps_toml_value_no_error", func(t *testing.T) {
		clearHybridEnv(t)
		t.Setenv("ZDEV_SIDEBAR_WIDTH", "oops")

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v (want nil; env is soft)", err)
		}
		if cfg.Width != 40 {
			t.Errorf("Width = %d, want 40 (TOML value, env was non-int)", cfg.Width)
		}
	})

	t.Run("negative_width_rejected_keeps_toml_value", func(t *testing.T) {
		clearHybridEnv(t)
		t.Setenv("ZDEV_SIDEBAR_WIDTH", "-5")

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Width != 40 {
			t.Errorf("Width = %d, want 40 (negative env rejected)", cfg.Width)
		}
	})

	t.Run("empty_width_keeps_toml_value", func(t *testing.T) {
		clearHybridEnv(t)
		// Explicitly set then unset to ensure Getenv returns "".
		t.Setenv("ZDEV_SIDEBAR_WIDTH", "")

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Width != 40 {
			t.Errorf("Width = %d, want 40 (empty env is identical to unset)", cfg.Width)
		}
	})

	t.Run("toml_only_keys_not_env_overridable", func(t *testing.T) {
		// D4-13: only 4 user-facing keys are hybrid. The 8 cadence/threshold
		// keys are TOML-only — even if an operator sets ZDEV_STALE_SECONDS
		// out of habit, the loaded value MUST come from TOML (or default).
		clearHybridEnv(t)
		t.Setenv("ZDEV_STALE_SECONDS", "9999")
		t.Setenv("ZDEV_PORTS_MAX", "9999")
		t.Setenv("ZDEV_PR_REFRESH_SECONDS", "9999")
		t.Setenv("ZDEV_WAIT_WARN_SECONDS", "9999")
		t.Setenv("ZDEV_WAIT_URGENT_SECONDS", "9999")
		t.Setenv("ZDEV_GIT_FLOOR_SECONDS", "9999")
		t.Setenv("ZDEV_DEFAULT_BRANCHES", "9999")
		t.Setenv("ZDEV_DEFAULT_SHELLS", "9999")

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.StaleSeconds != 1234 {
			t.Errorf("StaleSeconds = %d, want 1234 (TOML value; not env-overridable per D4-13)", cfg.StaleSeconds)
		}
	})
}

// ---------------------------------------------------------------------------
// CONFIG-04: unknown keys WARN'd via MetaData.Undecoded(), config still
// loads.
// ---------------------------------------------------------------------------

func TestConfigUnknownKeysWarn(t *testing.T) {
	clearHybridEnv(t)
	buf := captureSlog(t)

	const fixture = `
width = 77
unknown_key = "x"
another_unknown = 1
[oops_section]
nested = true
`
	path := writeTOML(t, fixture)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v (CONFIG-04 expects nil error on unknown keys)", err)
	}
	if cfg.Width != 77 {
		t.Errorf("Width = %d, want 77 (known key still applies)", cfg.Width)
	}

	out := buf.String()
	// Three unknown keys should produce three WARN lines mentioning each.
	for _, key := range []string{"unknown_key", "another_unknown", "oops_section"} {
		if !strings.Contains(out, key) {
			t.Errorf("slog output missing WARN for %q\n--- captured ---\n%s", key, out)
		}
	}
	// Count WARN-level lines: TextHandler emits "level=WARN" tokens.
	warnCount := strings.Count(out, "level=WARN")
	if warnCount < 3 {
		t.Errorf("expected ≥3 WARN log lines, got %d\n--- captured ---\n%s", warnCount, out)
	}
}

// ---------------------------------------------------------------------------
// D4-14 / Pitfall 2: parse errors loud-fail with line/col context.
// ---------------------------------------------------------------------------

func TestConfigParseError(t *testing.T) {
	clearHybridEnv(t)

	t.Run("syntax_error_has_line_col_context", func(t *testing.T) {
		clearHybridEnv(t)
		buf := captureSlog(t)

		// Deliberate syntax error: unterminated array on line 2.
		const fixture = "workspace = \"ok\"\nwidth = [\n"
		path := writeTOML(t, fixture)

		cfg, err := Load(path)
		if err == nil {
			t.Fatal("Load: err = nil, want non-nil (D4-14 loud-fail)")
		}
		// Returned Config should be the zero value — caller is expected to
		// exit on error, not consume the partial state.
		if !reflect.DeepEqual(cfg, Config{}) {
			t.Errorf("returned cfg = %+v, want zero Config{}", cfg)
		}

		out := buf.String()
		if !strings.Contains(out, "level=ERROR") {
			t.Errorf("expected ERROR log line, got\n%s", out)
		}
		if !strings.Contains(out, "path=") {
			t.Errorf("expected path= key in ERROR log, got\n%s", out)
		}
		// The ParseError branch surfaces line= and col= keys.
		if !strings.Contains(out, "line=") {
			t.Errorf("expected line= in ERROR log (Pitfall 2 mitigation), got\n%s", out)
		}
		if !strings.Contains(out, "col=") {
			t.Errorf("expected col= in ERROR log (Pitfall 2 mitigation), got\n%s", out)
		}
	})

	t.Run("type_mismatch_returns_err_logs_path", func(t *testing.T) {
		clearHybridEnv(t)
		buf := captureSlog(t)

		// Type mismatch: width is `int`, fixture supplies a string. This may
		// route through the generic decode-error branch (no line/col) on
		// some BurntSushi/toml versions, which is acceptable per D4-14 —
		// the operator still gets path + err message.
		const fixture = `width = "not-an-int"`
		path := writeTOML(t, fixture)

		cfg, err := Load(path)
		if err == nil {
			t.Fatal("Load: err = nil, want non-nil")
		}
		if !reflect.DeepEqual(cfg, Config{}) {
			t.Errorf("returned cfg = %+v, want zero Config{}", cfg)
		}
		out := buf.String()
		if !strings.Contains(out, "level=ERROR") {
			t.Errorf("expected ERROR log line, got\n%s", out)
		}
		if !strings.Contains(out, "path=") {
			t.Errorf("expected path= in ERROR log, got\n%s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// XDG_CONFIG_HOME helper
// ---------------------------------------------------------------------------

func TestDefaultPath(t *testing.T) {
	t.Run("xdg_set", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
		got := DefaultPath()
		want := "/tmp/xdg/zdev/sidebar.toml"
		if got != want {
			t.Errorf("DefaultPath() = %q, want %q", got, want)
		}
	})

	t.Run("xdg_unset_falls_back_to_home", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		_ = os.Unsetenv("XDG_CONFIG_HOME")
		t.Setenv("HOME", "/home/test")
		got := DefaultPath()
		want := "/home/test/.config/zdev/sidebar.toml"
		if got != want {
			t.Errorf("DefaultPath() = %q, want %q", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Defaults() values are locked to REQUIREMENTS DATA-01/03/06/08/09 + VIS-12.
// This test fails the build if a future refactor silently changes a default.
// ---------------------------------------------------------------------------

func TestDefaultsValues(t *testing.T) {
	d := Defaults()

	if d.Width != 50 {
		t.Errorf("Width = %d, want 50", d.Width)
	}
	if d.StaleSeconds != 3600 {
		t.Errorf("StaleSeconds = %d, want 3600 (VIS-12)", d.StaleSeconds)
	}
	if d.WaitWarnSeconds != 60 {
		t.Errorf("WaitWarnSeconds = %d, want 60 (DATA-09)", d.WaitWarnSeconds)
	}
	if d.WaitUrgentSeconds != 300 {
		t.Errorf("WaitUrgentSeconds = %d, want 300 (DATA-09)", d.WaitUrgentSeconds)
	}
	if d.PortsMax != 4 {
		t.Errorf("PortsMax = %d, want 4 (DATA-06)", d.PortsMax)
	}
	if d.ClaudeGlyph != "✻" {
		t.Errorf("ClaudeGlyph = %q, want %q (DATA-08)", d.ClaudeGlyph, "✻")
	}
	if d.PiGlyph != "π" {
		t.Errorf("PiGlyph = %q, want %q (260512-cpa codex→pi default)", d.PiGlyph, "π")
	}
	if len(d.DefaultBranches) != 4 {
		t.Errorf("DefaultBranches len = %d, want 4 (DATA-01)", len(d.DefaultBranches))
	}
	branchSet := map[string]bool{}
	for _, b := range d.DefaultBranches {
		branchSet[b] = true
	}
	if !branchSet["main"] {
		t.Errorf("DefaultBranches missing %q: %v", "main", d.DefaultBranches)
	}
	if len(d.DefaultShells) != 7 {
		t.Errorf("DefaultShells len = %d, want 7 (DATA-03; 260512-cpa: codex+codex-aarch64-a → pi, net -1)", len(d.DefaultShells))
	}
	shellSet := map[string]bool{}
	for _, s := range d.DefaultShells {
		shellSet[s] = true
	}
	if !shellSet["claude"] {
		t.Errorf("DefaultShells missing %q: %v", "claude", d.DefaultShells)
	}
	if d.PRRefreshSeconds != 300 {
		t.Errorf("PRRefreshSeconds = %d, want 300", d.PRRefreshSeconds)
	}
	if d.GitFloorSeconds != 10 {
		t.Errorf("GitFloorSeconds = %d, want 10", d.GitFloorSeconds)
	}
}

// TestBuiltinAgents covers the default agent registry.
func TestBuiltinAgents(t *testing.T) {
	got := BuiltinAgents()
	if len(got) < 2 {
		t.Fatalf("BuiltinAgents() returned %d entries; want >= 2", len(got))
	}
	byName := map[string]AgentSpec{}
	for _, a := range got {
		byName[a.Name] = a
	}
	for _, name := range []string{"claude", "codex", "opencode"} {
		spec, ok := byName[name]
		if !ok {
			t.Errorf("BuiltinAgents missing %q: %+v", name, got)
			continue
		}
		if spec.Glyph == "" {
			t.Errorf("BuiltinAgents[%q].Glyph empty", name)
		}
		if spec.Launch == "" {
			t.Errorf("BuiltinAgents[%q].Launch empty", name)
		}
	}
}

// TestEffectiveAgents_FallbackToBuiltins covers the "no [[agent]] in
// TOML" path: callers see the built-in registry, not an empty slice.
func TestEffectiveAgents_FallbackToBuiltins(t *testing.T) {
	c := Defaults()
	got := c.EffectiveAgents()
	if len(got) != len(BuiltinAgents()) {
		t.Errorf("EffectiveAgents() with no Agents set = %d entries; want %d (builtin count)",
			len(got), len(BuiltinAgents()))
	}
}

// TestEffectiveAgents_UserAgentsOverlay verifies custom agents append without
// making the user duplicate every built-in entry.
func TestEffectiveAgents_UserAgentsOverlay(t *testing.T) {
	c := Defaults()
	c.Agents = []AgentSpec{{Name: "custom-agent", Glyph: "★", Launch: "myagent"}}
	got := c.EffectiveAgents()
	if len(got) != len(BuiltinAgents())+1 {
		t.Fatalf("EffectiveAgents() with 1 user agent = %d entries; want %d", len(got), len(BuiltinAgents())+1)
	}
	if got[len(got)-1].Name != "custom-agent" {
		t.Errorf("last EffectiveAgents entry = %q; want custom-agent", got[len(got)-1].Name)
	}
}

func TestEffectiveAgents_DisableAndOverride(t *testing.T) {
	disabled := false
	c := Defaults()
	c.Agents = []AgentSpec{
		{Name: "codex", Enabled: &disabled},
		{Name: "claude", Glyph: "C", Command: "claude", Launch: "claude --safe"},
	}
	got := c.EffectiveAgents()
	byName := make(map[string]AgentSpec, len(got))
	for _, spec := range got {
		byName[spec.Name] = spec
	}
	if _, ok := byName["codex"]; ok {
		t.Fatal("disabled codex remained in effective registry")
	}
	if byName["claude"].Launch != "claude --safe" {
		t.Errorf("claude override not applied: %+v", byName["claude"])
	}
}

// TestLoad_AgentsTOML covers TOML decoding of a sample [[agent]] block —
// nested-table support (CONFIG-06).
func TestLoad_AgentsTOML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sidebar.toml"
	payload := `
width = 60

[[agent]]
name = "claude"
glyph = "✻"
waiting_markers = ["● claude", "✳ "]
finished_markers = ["◆ claude"]
launch = "claude --custom-flag"

[[agent]]
name = "opencode"
glyph = "○"
waiting_markers = ["● opencode"]
launch = "opencode"
`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Width != 60 {
		t.Errorf("Width = %d; want 60", cfg.Width)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("Agents len = %d; want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "claude" || cfg.Agents[0].Glyph != "✻" {
		t.Errorf("Agents[0] = %+v; want claude with ✻ glyph", cfg.Agents[0])
	}
	if cfg.Agents[1].Name != "opencode" || cfg.Agents[1].Glyph != "○" {
		t.Errorf("Agents[1] = %+v; want opencode with ○ glyph", cfg.Agents[1])
	}
	if cfg.Agents[0].Launch != "claude --custom-flag" {
		t.Errorf("Agents[0].Launch = %q; want %q", cfg.Agents[0].Launch, "claude --custom-flag")
	}
}

// TestCollapseConfig pins the [collapse] table: absent -> both kinds fold
// (default true); explicit false and pinned keys decode.
func TestCollapseConfig(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Collapse.CollapseInitiatives() || !c.Collapse.CollapseContainers() {
		t.Errorf("absent [collapse] must default both kinds to true")
	}

	p := filepath.Join(t.TempDir(), "sidebar.toml")
	body := "[collapse]\ninitiatives = true\ncontainers = false\nexpand = [\"marketplace\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Collapse.CollapseInitiatives() {
		t.Errorf("initiatives=true must resolve true")
	}
	if c2.Collapse.CollapseContainers() {
		t.Errorf("containers=false must resolve false")
	}
	if len(c2.Collapse.Expand) != 1 || c2.Collapse.Expand[0] != "marketplace" {
		t.Errorf("expand = %v", c2.Collapse.Expand)
	}
}

// TestEffectiveAgents_PartialOverrideMerges pins the overlay contract the
// docs promise: every field except name is optional, so a glyph-only
// override of a built-in keeps its markers, command, and launch —
// detection and auto-launch survive a cosmetic customization. An
// explicitly empty list clears the built-in's (presence-aware via nil).
func TestEffectiveAgents_PartialOverrideMerges(t *testing.T) {
	var builtin AgentSpec
	for _, b := range BuiltinAgents() {
		if b.Name == "claude" {
			builtin = b
		}
	}
	if builtin.Name == "" || len(builtin.WaitingMarkers) == 0 || builtin.Launch == "" {
		t.Fatalf("built-in claude spec not found or incomplete: %+v", builtin)
	}

	cfg := Config{Agents: []AgentSpec{{Name: "claude", Glyph: "☯"}}}
	specs := cfg.EffectiveAgents()
	var got AgentSpec
	for _, s := range specs {
		if s.Name == "claude" {
			got = s
		}
	}
	if got.Glyph != "☯" {
		t.Errorf("Glyph = %q; want the override ☯", got.Glyph)
	}
	if len(got.WaitingMarkers) != len(builtin.WaitingMarkers) ||
		got.Launch != builtin.Launch || got.Command != builtin.Command {
		t.Errorf("glyph-only override erased built-in fields: %+v", got)
	}

	cfg = Config{Agents: []AgentSpec{{Name: "claude", WaitingMarkers: []string{}}}}
	for _, s := range cfg.EffectiveAgents() {
		if s.Name == "claude" && len(s.WaitingMarkers) != 0 {
			t.Errorf("explicit empty list must clear the built-in's markers: %+v", s.WaitingMarkers)
		}
	}
}

// TestLoad_RejectsMalformedAgentCommand pins the line-protocol guard:
// command must be one executable token — whitespace, shell metacharacters,
// and option-like values fail loading with the agent named; launch must be
// a single line so it cannot split the tab-separated record.
func TestLoad_RejectsMalformedAgentCommand(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"clean", "[[agent]]\nname = \"x\"\ncommand = \"my-agent\"\nlaunch = \"my-agent --go\"\n", true},
		{"path", "[[agent]]\nname = \"x\"\ncommand = \"/opt/bin/agent\"\nlaunch = \"a\"\n", true},
		{"space", "[[agent]]\nname = \"x\"\ncommand = \"rm -rf\"\nlaunch = \"a\"\n", false},
		{"semicolon", "[[agent]]\nname = \"x\"\ncommand = \"a;b\"\nlaunch = \"a\"\n", false},
		{"tab", "[[agent]]\nname = \"x\"\ncommand = \"a\\tb\"\nlaunch = \"a\"\n", false},
		{"quote", "[[agent]]\nname = \"x\"\ncommand = \"a'b\"\nlaunch = \"a\"\n", false},
		{"dash", "[[agent]]\nname = \"x\"\ncommand = \"-rf\"\nlaunch = \"a\"\n", false},
		{"newline-launch", "[[agent]]\nname = \"x\"\ncommand = \"a\"\nlaunch = \"a\\nb\"\n", false},
		{"wrapper-launch", "[[agent]]\nname = \"x\"\ncommand = \"agent\"\nlaunch = \"env FOO=1 sh -c 'agent --x'\"\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "sidebar.toml")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if tc.ok && err != nil {
				t.Errorf("Load = %v; want ok", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("Load accepted a malformed agent spec")
			}
		})
	}
}
