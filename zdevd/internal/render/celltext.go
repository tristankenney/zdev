// internal/render/celltext.go
//
// The display-text seam (calm pass lane C / QA plan T1): every truncation
// or width decision over USER-SUPPLIED text — wait summaries, held titles,
// loop goals, anchor titles — routes through these, which measure terminal
// CELLS via grapheme clusters and never slice mid-rune. The renderer's own
// glyph vocabulary is single-width by construction (width.go), but agent-
// and user-authored strings are arbitrary UTF-8: a byte slice can emit
// invalid UTF-8 into the frame, and a rune count mismeasures CJK, combining
// marks, and emoji sequences.
//
// Implemented on charmbracelet/x/ansi (already in the dependency graph),
// which is ANSI-aware: embedded escapes cost zero cells and are never cut
// in half.
package render

import (
	"github.com/charmbracelet/x/ansi"
)

// CellWidth returns the terminal-cell width of s: grapheme-cluster aware,
// ANSI-escape blind.
func CellWidth(s string) int {
	return ansi.StringWidth(s)
}

// CellTruncate cuts s to at most cells terminal cells, appending tail
// (usually Ellipsis or "...") when anything was cut. tail's own width is
// budgeted inside cells, mirroring truncateRunes' contract. cells <= 0
// returns the empty string.
func CellTruncate(s string, cells int, tail string) string {
	if cells <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= cells {
		return s
	}
	return ansi.Truncate(s, cells, tail)
}
