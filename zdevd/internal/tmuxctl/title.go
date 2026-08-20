// internal/tmuxctl/title.go
//
// Pane-title classifier ported from the bash baseline at
// ~/.local/bin/zdev-sidebar-render lines 23-90 (PULSE/marker/glyph
// constants) and line 484 (status hierarchy comment). The Go classifier
// is byte-exact prefix matching on the leading marker glyph; the
// per-session aggregation (waiting > shell-running > finished > alive)
// lives in hub/state.go's deriveStatus.
package tmuxctl

import (
	"strings"
	"unicode/utf8"
)

// Status constants — exact strings used in proto.Snapshot.Projects[].Status.
const (
	StatusWaiting      = "waiting"
	StatusFinished     = "finished"
	StatusShellRunning = "shell-running"
	StatusAlive        = "alive"
	StatusAbsent       = "absent"
)

// Marker glyphs ported verbatim from the bash baseline at
// ~/.local/bin/zdev-sidebar-render lines 23-90 (PULSE/marker/glyph
// constants). The glyphs are multi-byte UTF-8; HasPrefix is byte-exact.
//
// Legacy format (old Claude/Codex CLI):
//   - ● (U+25CF BLACK CIRCLE) + space = waiting
//   - ◆ (U+25C6 BLACK DIAMOND) + space = finished
//   - ◎ (U+25CE BULLSEYE) + space = shell running
//
// New Claude Code CLI (v2.1+) format:
//   - ✳ (U+2733 EIGHT SPOKED ASTERISK) + space = waiting for user input
//   - Braille pattern chars (U+2800–U+28FF) + space = working/generating
const (
	MarkerWaiting      = "● " // U+25CF BLACK CIRCLE  + space (legacy)
	MarkerFinished     = "◆ " // U+25C6 BLACK DIAMOND + space (legacy)
	MarkerShellRunning = "◎ " // U+25CE BULLSEYE      + space (legacy)
	MarkerWaitingNew   = "✳ " // U+2733 EIGHT SPOKED ASTERISK + space (Claude Code v2.1+)
)

// brailleRangeStart / End define the Unicode Braille Patterns block (U+2800–U+28FF).
// Claude Code v2.1+ uses characters from this block as spinner frames in pane titles
// while the model is generating (e.g. "⠂ Implementing feature X").
const (
	brailleRangeStart = 0x2800
	brailleRangeEnd   = 0x28FF
)

// spinner glyph ranges: the Braille Patterns block (U+2800–U+28FF, the
// classic Claude Code generating spinner) and the circle-quadrant set
// ◐◓◑◒ (U+25D0–U+25D3), which newer Claude Code builds title with while
// working — observed live 2026-08-18, when three visibly-generating
// sessions all derived bare "alive" and the sidebar showed a quiet fleet.
// isBrailleSpinnerTitle returns true when the title starts with a Braille
// Patterns character (U+2800–U+28FF) followed by a space. This is the
// "Claude is working/generating" state in Claude Code v2.1+.
func isBrailleSpinnerTitle(title string) bool {
	if len(title) < 4 { // 3 bytes for spinner char + 1 space minimum
		return false
	}
	r, size := utf8.DecodeRuneInString(title)
	if r == utf8.RuneError {
		return false
	}
	braille := r >= brailleRangeStart && r <= brailleRangeEnd
	quadrant := r >= 0x25D0 && r <= 0x25D3 // ◐ ◓ ◑ ◒
	if !braille && !quadrant {
		return false
	}
	// Check that the rune is followed by a space.
	return len(title) > size && title[size] == ' '
}

// DefaultShells is the set of pane_current_command values whose presence
// means "this is just a shell, no chip-worthy command running" (DATA-03).
// 260512-cpa: codex+codex-aarch64-a entries replaced by `pi` (pi.dev).
//
// claude/pi appear in the list because panes running those agents also match
// `pane_current_command == "claude"` etc. In the Go pipeline, agent
// attribution is handled separately by agents.Registry on the pane-title
// path; panes with title "shell" and cmd="claude" still should not show a
// ShellCmd chip, so agent-variant commands are suppressed here too.
var DefaultShells = []string{"bash", "zsh", "sh", "fish", "dash", "claude", "claude.exe"} // 260519-hww: pi removed while pi.dev integration is disabled (sl undo to restore)

// IsDefaultShell returns true when cmd is one of DefaultShells.
func IsDefaultShell(cmd string) bool {
	for _, s := range DefaultShells {
		if s == cmd {
			return true
		}
	}
	return false
}

// ClassifyPaneTitle returns the per-pane status from the title's leading
// marker glyph, per the bash baseline conventions:
//
// Legacy format:
//
//	● claude bench-test       -> "waiting"
//	◆ pi done                 -> "finished"
//	◎ npm run dev             -> "shell-running"
//	zsh / vim / anything else -> "alive"
//
// New Claude Code v2.1+ format:
//
//	✳ Claude Code             -> "alive" (idle prompt — Claude is open with no
//	                             active task; visually quiet · marker, no chip)
//	✳ <task description>      -> "waiting" (active task awaiting user permission)
//	⠂ <task description>      -> "shell-running" (working/generating)
//	⠐ <task description>      -> "shell-running" (any Braille spinner)
//	claude (bare)             -> "alive"   (idle shell running claude)
//
// Phase 2 simplifies the bash baseline by widening from `^● claude`
// (the bash version's strict pattern) to `^● `. This catches more
// pane-title formats while staying conservative — the agent-name suffix
// (claude, pi) will be honored by the per-agent chips in Phase 3
// (DATA-08).
//
// The literal "✳ Claude Code" form is split out from generic `✳ <task>`
// to fix a UX bug where every Claude session pulses "needs input" on
// daemon restart simply because every idle Claude pane has that title.
// Claude Code sets it on boot before any task runs, and replaces it with
// the active task description while awaiting permission — so the suffix
// IS the discriminator.
//
// Empty string returns StatusAlive (not StatusAbsent — absence is a
// session-level property, applied in hub/state.go's deriveStatus when
// len(panes) == 0).
//
// TRUST NOTE (M2b): a pane title is attacker-controlled — anything running in
// the pane can set it, so a hostile process can forge the "✳ <task>" waiting
// marker and make this classifier report StatusWaiting. This function stays a
// pure, correct byte-exact classifier by design; the anti-abuse defense is not
// here but in the hub, where notifications are gated by the per-session tier
// bitmap (pd.WaitNotifiedTiers): a given wait notifies at most once per
// escalation tier, never once per derivation pass. That bitmap already bounds
// a forged title to one wait's worth of tiered escalation — it cannot produce
// a notification storm. Fully distinguishing a forged title from a genuine
// agent wait needs an authenticated signal (the hook channel already carries
// one via NotifSeen; a pane-title nonce is the tracked follow-up) and cannot
// be decided from the title string alone.
func ClassifyPaneTitle(title string) string {
	switch {
	// New Claude Code v2.1+ format.
	case strings.HasPrefix(title, MarkerWaitingNew):
		// "✳ Claude Code" (literal) is the idle-prompt title Claude sets
		// before any task. Treat as alive (subtle · marker, no attention
		// pulse) so 19 idle Claude sessions don't all light up the sidebar
		// after a daemon restart. Other `✳ <task>` forms ARE real waits.
		if strings.TrimPrefix(title, MarkerWaitingNew) == "Claude Code" {
			return StatusAlive
		}
		return StatusWaiting
	case isBrailleSpinnerTitle(title):
		// Braille spinner prefix = Claude is working/generating.
		// Map to StatusShellRunning so the hub's deriveStatus hierarchy
		// (waiting > shell-running > finished > alive) correctly shows
		// "active" status when Claude is generating.
		return StatusShellRunning

	// Legacy format.
	case strings.HasPrefix(title, MarkerWaiting):
		return StatusWaiting
	case strings.HasPrefix(title, MarkerFinished):
		return StatusFinished
	case strings.HasPrefix(title, MarkerShellRunning):
		return StatusShellRunning
	default:
		return StatusAlive
	}
}
