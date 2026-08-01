package render

import (
	"regexp"
	"unicode/utf8"
)

// sgrRE matches ANSI escape sequences for visible-width measurement.
// Production twin of the test helpers' ansiRE — needed by the rose-pine
// right-aligned status column, which pads between the name and the chip
// cluster and must measure COLUMNS, not bytes (every color token is
// multibyte, every frame glyph is multibyte).
var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// visWidth returns the on-screen column count of s: escapes stripped,
// runes counted. All glyphs this renderer emits are single-width.
func visWidth(s string) int {
	return utf8.RuneCountInString(sgrRE.ReplaceAllString(s, ""))
}

// Truncate14 returns s truncated to 13 runes + Ellipsis ("…", U+2026)
// when s exceeds 14 runes. Returns s unchanged at exactly 14 runes or
// fewer.
//
// Bash baseline lines 525-526, 540-541 — branch names and shell-cmd
// labels are truncated at 14 chars with `…` appended. Per RESEARCH
// "Don't Hand-Roll" (line 474), bash uses byte length for ASCII branch
// names; rune count is the closest matching primitive in Go and matches
// for any pure-ASCII input. For multi-byte branch names (rare —
// `feature-ñoño` style) rune count is the more sensible behavior.
func Truncate14(s string) string {
	return truncateRunes(s, 14)
}

// truncateRunes returns s truncated to cap-1 runes + Ellipsis when s
// exceeds cap runes. Returns s unchanged when rune count <= cap.
// Used by Truncate14 (cap=14) and the current-row branch cap (cap=24).
func truncateRunes(s string, cap int) string {
	if utf8.RuneCountInString(s) <= cap {
		return s
	}
	runes := []rune(s)
	return string(runes[:cap-1]) + Ellipsis
}
