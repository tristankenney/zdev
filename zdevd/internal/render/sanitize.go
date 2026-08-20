// internal/render/sanitize.go
//
// SanitizeLine is the single trust-boundary scrubber for untrusted text that
// would otherwise reach the operator's terminal verbatim: agent hook payloads
// (wait summaries, death reasons) and MCP-set titles (park thoughts, anchor
// titles). Raw agent/MCP text can carry ESC/CSI/OSC/CR/BEL control bytes; once
// those reach the sidebar they escape the cell model — an OSC 52 sequence
// writes the operator's clipboard, CR/SGR forge or hide rows. Stripping the
// control bytes at ingestion means every downstream consumer (render, the
// persisted zdevd-state.json, the phone push) inherits an already-safe string.
package render

import (
	"strings"
	"unicode"
)

// SanitizeLine returns s with every Unicode control character removed. This
// neutralizes the whole terminal-injection class: ESC (so no CSI/SGR/OSC
// sequence can form — CSI and OSC both require a leading ESC), BEL (the OSC 52
// terminator), CR/LF/VT/FF (row forging), and NUL. Control runes are dropped
// rather than replaced so no phantom width is introduced into the sidebar's
// cell accounting.
//
// Newline is itself a control rune (unicode.IsControl('\n') == true), so this
// is also the single-line guard for fields that must never wrap: an embedded
// newline is stripped along with the rest. All the values sanitized at the hub
// boundary (WaitSummary, DeadReason, park/anchor titles) are single-line by
// contract, so dropping newlines here is correct, not lossy.
//
// Benign text (printable ASCII, ordinary Unicode letters/marks/punctuation,
// spaces) passes through untouched. The common case — a string with no control
// runes — allocates nothing.
func SanitizeLine(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
