package render

import (
	"fmt"

	"github.com/tristankenney/zdev/zdevd/internal/policy"
)

// Visual constants ported byte-for-byte from the bash baseline at
// ~/.local/bin/zdev-sidebar-render lines 27-78 + 116-122. The bash file
// IS the spec for VIS-* requirements; any drift here will fail Plan 07's
// golden-frame parity tests.
//
// Source-of-truth lines (relative to ~/.local/bin/zdev-sidebar-render):
//
//	PULSE / PULSE_HOLD                          → lines 27, 31
//	BREATH / BREATH_HOLD                        → lines 33-37
//	PROJECT_PALETTE                             → lines 39-40
//	CLAUDE_GLYPH / PI_GLYPH defaults            → lines 46-47
//	FRAME_SLEEP / IDLE_SLEEP cadence            → lines 48-49
//	GIT_REFRESH_TICKS / PR_REFRESH_TICKS        → lines 51-52
//	STALE_THRESHOLD / WAIT_* thresholds         → lines 116-122
//	PR celebration window                       → line 315
//	Em-dash / ellipsis glyphs                   → lines 622, 525-526
//	DEFAULT_BRANCHES_RE                         → line 56
//
// NOTE: BreathStates was replaced by BreathBrightness + BreathColorForProject
// in quick task 260511-mgy. Breath bytes are no longer byte-parity with the
// bash baseline lines 33-37 (INTENTIONAL — per-project hue replaces
// hardcoded cyan).

// PulseFrames is the 8-frame waiting-marker cycle. PulseHold=1 advances
// every renderer tick. Note element 3 and 4 are BOTH "●" — the peak
// holds for two frames per the bash baseline.
var PulseFrames = [8]string{"·", "∙", "•", "●", "●", "•", "∙", "·"}

// WorkFrames is the 4-frame working-marker spinner (dogfood 2026-06-06:
// a static ◎ doesn't read as "in motion" — an animated glyph is the
// convention for running work). The quarter-circle family keeps the
// marker in the same visual register as the rest of the circle glyphs
// (● ◎ ◆ ·). Advanced by Animator.WorkGlyph at workHold ticks/frame.
var WorkFrames = [4]string{"◐", "◓", "◑", "◒"}

// BreathBrightness is the 4-slot brightness-param cycle for the current-session
// breath bar. Each slot is an SGR brightness PARAM ONLY — no "\x1b[" prefix,
// no "m" suffix, no color code. The color is provided separately by
// BreathColorForProject, which combines brightness + per-project xterm hue
// into a single combined SGR sequence (e.g., "\x1b[1;38;5;39m").
//
// Slot semantics:
//
//	0 = "1"  — bold   (peak)
//	1 = ""   — normal (default brightness)
//	2 = "2"  — dim    (trough)
//	3 = ""   — normal (default brightness, return)
//
// BreathHold=30 renderer ticks per slot ≈ 8s full cycle at 15fps.
// The "single combined SGR" rule is preserved: do NOT emit "\x1b[1m\x1b[38;5;Nm"
// — always "\x1b[1;38;5;Nm".
var BreathBrightness = [4]string{"1", "", "2", ""}

// BreathColorForProject returns the combined SGR sequence for the breath bar
// of the named project at the given animation frame index. The returned string
// is ready to write directly to the output buffer.
//
// The hue is derived from paletteXtermCodes[PaletteIndex(name)] — deterministic
// per project name. The brightness is taken from BreathBrightness[frame%4].
//
// When brightness is non-empty the result is "\x1b[<bright>;38;5;<code>m";
// when brightness is empty (default brightness frames) the result is "\x1b[38;5;<code>m".
// Both forms are single combined SGR sequences, never two separate ESC sequences.
func BreathColorForProject(name string, frame int) string {
	code := paletteXtermCodes[PaletteIndex(name)]
	bright := BreathBrightness[frame%len(BreathBrightness)]
	if bright != "" {
		return fmt.Sprintf("\x1b[%s;38;5;%dm", bright, code)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", code)
}

// ProjectPalette is the 15-entry ANSI escape sequence palette indexed by
// PaletteIndex(name) — see color.go. Codes are xterm-256 foreground colors.
// The underlying xterm code numbers are in paletteXtermCodes (color.go).
var ProjectPalette = [15]string{
	"\x1b[38;5;39m", "\x1b[38;5;45m", "\x1b[38;5;51m",
	"\x1b[38;5;75m", "\x1b[38;5;81m", "\x1b[38;5;87m",
	"\x1b[38;5;105m", "\x1b[38;5;111m", "\x1b[38;5;141m",
	"\x1b[38;5;147m", "\x1b[38;5;177m", "\x1b[38;5;183m",
	"\x1b[38;5;207m", "\x1b[38;5;213m", "\x1b[38;5;219m",
}

const (
	// PulseHold is the number of renderer ticks per PULSE step. Bash sets
	// PULSE_HOLD=1 — pulse advances every tick.
	PulseHold = 1

	// BreathHold is the number of renderer ticks per BREATH state. Bash
	// sets BREATH_HOLD=30 — at 15fps this is a ~2s hold per state, ~8s
	// full cycle (the bash comment says "~4s full cycle at 30fps" but the
	// actual sleep loop runs at 15fps; CONTEXT D3-07 documents the
	// intentional cadence).
	BreathHold = 30

	// ClaudeGlyphDefault and PiGlyphDefault are env-overridable at
	// renderer startup via ZDEV_SIDEBAR_CLAUDE_GLYPH / ZDEV_SIDEBAR_PI_GLYPH.
	// 260512-cpa: codex slot replaced by pi.dev; glyph default π (Greek small
	// letter pi, U+03C0) — single-cell width like the prior ◉.
	ClaudeGlyphDefault = "✻"
	PiGlyphDefault     = "π"

	// FrameSleepMS is the renderer's animating cadence (~15fps).
	// Bash baseline FRAME_SLEEP=0.066s.
	FrameSleepMS = 66

	// IdleSleepMS is the renderer's idle cadence (~5fps).
	// Bash baseline IDLE_SLEEP=0.2s.
	IdleSleepMS = 200

	// InvisibleSleepMS is the renderer's effectively-paused cadence used
	// when its pane has no attached tmux client. The ticker still fires
	// (once per hour) so a snapshot-missed visibility transition can still
	// recover, but paint work is bounded regardless of how many sidebars
	// the user has open. In normal operation, visibility transitions arrive
	// as snapshot updates from the daemon and the ticker is Reset to the
	// appropriate cadence well before this fires.
	InvisibleSleepMS = 3_600_000 // 1 hour

	// StaleThresholdSec — alive-but-no-activity dim-out threshold.
	// VIS-12. Bash hardcodes 3600 (1 hour).
	StaleThresholdSec = 3600

	// WaitWarnSec — wait-age renders dim until this threshold.
	// DATA-09. Bash WAIT_WARN_SECONDS=60.
	WaitWarnSec = 60

	// WaitUrgentSec — wait-age renders red+`!` past this threshold.
	// DATA-09. Bash WAIT_URGENT_SECONDS=300.
	WaitUrgentSec = 300

	// PRCelebrationFrames is the number of renderer ticks the ✨ overlay
	// displays after a PR open-count drop. 60 ticks at 15fps ≈ 4s. The
	// hub stores an absolute unix-second deadline (state.celebrateUntil),
	// not a tick count — the renderer compares against time.Now().
	PRCelebrationFrames = 60

	// EmDash is the U+2014 horizontal em-dash. Was the empty-metadata
	// placeholder per VIS-14 but removed from renderer output in quick task
	// 260511-n4n (task 6): empty current-row metadata renders as a blank
	// ClearLineEnd line; non-current projects have no metadata row. Retained
	// as an exported constant for API stability — render code no longer emits it.
	EmDash = "—"

	// MoodRed, MoodGreen, MoodIdle are xterm-256 foreground colors for the
	// mood block (260511-n4n task 4 / PD-06). Orange reuses the existing
	// constant from ansi.go for the warn tier.
	MoodRed   = "\x1b[38;5;196m" // bright red (xterm 196)
	MoodGreen = "\x1b[38;5;46m"  // bright green (xterm 46)
	MoodIdle  = "\x1b[38;5;245m" // mid-grey (xterm 245)


	// Ellipsis is the U+2026 horizontal ellipsis, used as the truncation
	// suffix per VIS-11. Three bytes UTF-8 (0xE2 0x80 0xA6).
	Ellipsis = "…"

	// DefaultBranchesRE is preserved as a re-exported reference for
	// existing callers/tests; the canonical home for the default-branch
	// suppression rule is internal/policy (staff-review PR #4 — Arch CR #1).
	DefaultBranchesRE = policy.DefaultBranchesRE
)

// IsDefaultBranch re-exports the policy.IsDefaultBranch decision so render
// callers don't need to add an extra import for one yes/no call. The
// canonical implementation lives in internal/policy.
func IsDefaultBranch(branch string) bool { return policy.IsDefaultBranch(branch) }
