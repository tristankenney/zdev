// Package render owns ANSI escape constants and frame-construction helpers
// for the zdev-sidebar renderer. Constants are ported verbatim from the bash
// baseline at ~/.local/bin/zdev-sidebar-render lines 23-78 so Phase 3 parity
// work has a single source of truth. Phase 1 stub uses CursorHide,
// RestoreOnExit, Bold, Reset, ClearLineEnd, CursorHome, ClearToEnd; the rest
// land now to avoid Phase 3 churn.
package render

// Cursor control sequences.
const (
	CursorHide    = "\x1b[?25l"
	CursorShow    = "\x1b[?25h"
	CursorHome    = "\x1b[H"
	ClearToEnd    = "\x1b[J"
	ClearLineEnd  = "\x1b[K"
	RestoreOnExit = CursorShow + CursorHome + ClearToEnd
)

// SGR (Select Graphic Rendition) sequences ported verbatim from
// ~/.local/bin/zdev-sidebar-render lines 70-78.
const (
	Reset    = "\x1b[0m"
	Bold     = "\x1b[1m"
	Dim      = "\x1b[90m"
	Cyan     = "\x1b[36m"
	Yellow   = "\x1b[33m"
	Green    = "\x1b[32m"
	RedPulse = "\x1b[1;91m"
	// Icy follows the Orange precedent below (260511-h2): the previous
	// \x1b[96m was the THEME-MAPPED bright-cyan slot — terminal palettes
	// remap it freely (dogfood 2026-06-06: rendered orange on the
	// operator's theme, directly contradicting the legend's "cyan").
	// xterm-256 SGR 117 is a real sky cyan no 16-color theme touches.
	Icy = "\x1b[38;5;117m"
	// 260511-h2: Orange un-aliased from Yellow. Now xterm-256 SGR 208 — a real
	// orange clearly distinguishable from the finished-agent yellow marker.
	// Previously both Yellow and Orange were \x1b[33m, collapsing the
	// pending-PR / wait-warn / dirty-count tiers visually into the
	// finished-agent tier. Fixes the H2 audit item documented in DESIGN.md's
	// Tier-Distinct Rule.
	Orange = "\x1b[38;5;208m"

	// SGR dim attribute (restorable). D4-02 wraps the entire body during
	// outage in SGRDim...SGRUndim; the existing Dim constant is fg-grey-90
	// and not restorable in the same way.
	SGRDim   = "\x1b[2m"
	SGRUndim = "\x1b[22m"

	// RedBorder — urgent-row left-border accent (260511-nxy). Foreground xterm-256
	// red 196; matches MoodRed for visual coherence with the urgent mood block.
	// Replaces BgUrgent/FgOnUrgent (bg fill removed due to SGR-state bleed across rows).
	RedBorder = "\x1b[38;5;196m"
)
