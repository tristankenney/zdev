// Package render — the Bubble Tea body split.
//
// Render/RenderWithRows compose a frame with an embedded terminal harness:
// CursorHome at the top, a per-line ClearLineEnd ("\x1b[K") immediately
// before every newline, and a trailing ClearToEnd ("\x1b[J"). That harness
// is exactly right for the classic render loop (cmd/zdev-sidebar writes the
// bytes straight to the pane, owns no cursor state of its own, and relies on
// the escapes to wipe stale content).
//
// Bubble Tea's renderer is a different harness: it tracks cursor position
// and line-diffing itself, so feeding it CursorHome/ClearLineEnd/ClearToEnd
// would fight its own escape sequences and corrupt the diff. Body returns
// the SAME content with that harness stripped, for tea's View() to hand to
// the tea runtime unwrapped.
//
// Body is deliberately NOT a parallel rendering path. It calls
// RenderWithRows — the exact function the goldens pin — and mechanically
// strips the three harness pieces from the result. Every render*Row helper
// in frame.go, triage.go, review_gauge.go, and daemon_health.go writes
// ClearLineEnd as the last thing before each line's '\n' (verified by
// inspection — there is no code path that emits ClearLineEnd anywhere else
// in a line), so removing every occurrence of the ClearLineEnd byte
// sequence is lossless: it deletes exactly the harness, never content.
// CursorHome is always the frame's first bytes and ClearToEnd always its
// last (see RenderWithRows), so a plain prefix/suffix trim handles those.
//
// This means Body can never drift from Render — there is only one frame
// composer, and Render/RenderWithRows stay byte-for-byte what the goldens
// already pin.
package render

import (
	"bytes"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// Body returns the frame content for one snapshot WITHOUT the terminal
// harness (CursorHome, per-line ClearLineEnd, trailing ClearToEnd) alongside
// the same RowRef click-map RenderWithRows produces. cmd/zdev-sidebar's tea
// model uses this as the tea.Model's View() body; the harness is Bubble
// Tea's job, not this package's.
func Body(snap *proto.Snapshot, width int, animator *Animator, nowFn func() int64) ([]byte, []RowRef) {
	frame, rows := RenderWithRows(snap, width, animator, nowFn)
	return stripHarness(frame), rows
}

// stripHarness removes the three harness pieces described in the package
// doc. Safe to call on any RenderWithRows/Render output.
func stripHarness(frame []byte) []byte {
	frame = bytes.TrimPrefix(frame, []byte(CursorHome))
	frame = bytes.TrimSuffix(frame, []byte(ClearToEnd))
	return bytes.ReplaceAll(frame, []byte(ClearLineEnd), nil)
}
