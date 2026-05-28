package render

import "unicode/utf8"

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
