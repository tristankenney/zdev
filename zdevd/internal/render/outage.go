// Package render — outage rendering helper.
//
// ARCH-09 / D4-02 / D4-04: when the daemon connection drops, the renderer
// stays on its last frame for 500ms (grace), then paints exactly one dim
// overlay with a banner. At t=30s the banner escalates from "↻ reconnecting..."
// to "⚠ daemon offline" (one additional repaint). Animation is frozen
// throughout (D4-05) — these are the only two paints during outage.
//
// PaintOutage is the single helper for both substates. The caller decides
// when to call (and which banner string to pass).
package render

import (
	"bytes"
	"io"
)

// PaintOutage writes a dim-attribute-wrapped frame: a banner row
// ("  ↻ reconnecting..." or "  ⚠ daemon offline") above the last-known body.
// The body MUST be the bytes of the most recent successful Render output —
// PaintOutage does not re-render from a snapshot.
//
// Pre/postconditions:
//   - Cursor is left at end of frame (no explicit home/show).
//   - Caller must NOT have any in-flight ANSI state (Reset on disconnect).
//
// Visual layout (D4-02): banner adds one row above the standard frame.
// OPS-05 row-math invariant is explicitly relaxed during outage (CONTEXT D4-02).
func PaintOutage(w io.Writer, lastFrame []byte, banner string) error {
	var buf bytes.Buffer
	buf.WriteString(CursorHome)

	// Banner row.
	buf.WriteString(SGRDim)
	buf.WriteString("  ")
	buf.WriteString(banner)
	buf.WriteString(SGRUndim)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Body (last known frame, dimmed).
	buf.WriteString(SGRDim)
	buf.Write(lastFrame)
	buf.WriteString(SGRUndim)

	// Clear residual content from previous (non-outage) frame if shorter.
	buf.WriteString(ClearToEnd)

	_, err := w.Write(buf.Bytes())
	return err
}
