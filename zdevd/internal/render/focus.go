// internal/render/focus.go
//
// The focus loop's sidebar half (phase 3C, docs/design/command-centre.md):
// the anchor row, the "┊ holding N" counter, and damped rendering while
// anchored. Knob: ZDEV_SIDEBAR_FOCUS (cmd/zdev-sidebar wires it into
// FocusEnabled below). Default off ⇒ byte-identical frames — the loop's
// "must win by being picked, never by being default" rule, same posture as
// every other sidebar knob (ZDEV_SIDEBAR_TRIAGE, ZDEV_SIDEBAR_REVIEW, …).
//
// Lands in the SHARED Render path (frame.go's RenderWithOpts) so both the
// classic and tea engines get it for free, same th* color seam, same RowRef
// click-map discipline as every other renderer-only section.
//
// Anchor/Held/InFocus/FreeUntil are already on the wire (phase4-v24) —
// nothing sets them yet except the daemon phases building in parallel
// (this file only reads them; see proto.go's phase4-v24 comment block).
package render

import (
	"bytes"
	"fmt"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// FocusEnabled gates the anchor row, the holding counter, and damped
// rendering. cmd/zdev-sidebar sets this from ZDEV_SIDEBAR_FOCUS=1.
var FocusEnabled = false

// renderAnchorRow writes the anchor row — "▶ now  <title> · <elapsed>" — and,
// only when the airlock is actually catching something, the "┊ holding N"
// counter beneath it, as the FIRST line(s) of the frame (above the mood
// divider — cheap re-entry after any micro-distraction). Writes NOTHING when
// the knob is off or the snapshot carries no anchor, so an unanchored /
// knob-off sidebar is byte-identical to today.
//
// lineOf reports the line the anchor row lands on, for the caller's RowRef
// bookkeeping — the same closure frame.go's `claim` uses. Returns a RowRef
// targeting the anchor's project when one is set (the design's "enter
// switches to its session" — a click here does the same); ok is false when
// there is nothing to claim (no anchor, or a Project-less pick like a phone
// call). The holding counter is never clickable — the boundary popup is its
// consumer, next phase.
func renderAnchorRow(buf *bytes.Buffer, snap *proto.Snapshot, width int, nowFn func() int64, lineOf func() int) (ref RowRef, ok bool) {
	if !FocusEnabled || snap.Anchor == nil {
		return RowRef{}, false
	}
	anchor := snap.Anchor
	y := lineOf()

	// "▶ now" carries the working-hue family (thAnchor) — the anchor IS the
	// one thing actively being worked, the same semantic register as the
	// working spinner's color, but its own token so a future retint of
	// "working in general" doesn't silently retint the one-of-a-kind anchor
	// row too.
	buf.WriteString(thAnchor())
	buf.WriteString("▶ now")
	buf.WriteString(Reset)
	buf.WriteString("  ")

	now := nowFn()
	var elapsed int64
	if anchor.SinceTS > 0 && now > anchor.SinceTS {
		elapsed = now - anchor.SinceTS
	}
	age := formatAge(elapsed)

	// Truncate the title to what's left after the fixed-width prefix
	// ("▶ now  " = 7 visible columns) and suffix (" · " + age) are spent —
	// a title long enough to blow the pane must lose its tail, never the
	// elapsed time (the tether's whole point is cheap re-entry: "what was I
	// doing, and for how long").
	const prefixCols = 7
	suffixCols := 3 + len([]rune(age)) // " " + "· " + age
	titleCap := width - prefixCols - suffixCols
	if titleCap < 1 {
		titleCap = 1
	}
	buf.WriteString(Bold)
	buf.WriteString(truncateRunes(anchor.Title, titleCap))
	buf.WriteString(Reset)
	buf.WriteString(" ")
	buf.WriteString(thDim())
	buf.WriteString("· ")
	buf.WriteString(age)
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Holding counter: only when the airlock is actually catching
	// something — its entire job is proof the airlock works, so an empty
	// held set draws nothing rather than a permanent "holding 0".
	if len(snap.Held) > 0 {
		// Hue-coded (calibration 2026-08-03): a held WAIT is a
		// person-shaped item — the counter says so in the waiting hue
		// ("┊ holding 3 · ●2") instead of hiding it in a flat dim number.
		waits := 0
		for i := range snap.Held {
			if snap.Held[i].Kind == "wait" {
				waits++
			}
		}
		buf.WriteString("  ")
		buf.WriteString(thDim())
		fmt.Fprintf(buf, "┊ holding %d", len(snap.Held))
		if waits > 0 {
			buf.WriteString(" · ")
			buf.WriteString(Reset)
			buf.WriteString(thWaiting(0))
			fmt.Fprintf(buf, "●%d", waits)
		}
		buf.WriteString(Reset)
		buf.WriteString(ClearLineEnd)
		buf.WriteByte('\n')
	}

	if anchor.Project == "" {
		return RowRef{}, false
	}
	return RowRef{Y: y, Name: anchor.Project}, true
}

// focusReceded reports whether project p should render RECEDED under damped
// mode: true while anchored (damped, computed by the caller as
// FocusEnabled && snap.Anchor != nil) except for the anchor's own project
// and the FIRES list — a dead agent, or a wait that has crossed the urgent
// tier. urgent is passed in rather than recomputed: frame.go already calls
// isUrgent once per row for the ▌ left-border accent, and this predicate
// must agree with that exact same call (pure function, same inputs).
func focusReceded(damped bool, anchorProject string, p *proto.Project, urgent bool) bool {
	if !damped {
		return false
	}
	if anchorProject != "" && p.Name == anchorProject {
		return false
	}
	return !(urgent || projectAttention(p) == proto.AttDead)
}

// dampMarker overrides a receded row's attention glyph. Recalibrated
// 2026-08-03 ("I do like multi tasking"): damping kills MOTION, never
// information. For a fleet operator the peripheral fleet state IS the work
// queue — hopping to service a wait is the job, so a wait must stay
// legible at a glance even while anchored. What changes per attention:
//
//	waiting  → frozen at the pulse PEAK ("●"), full waiting hue — visible,
//	           motionless. (The first cut froze it to the resting "·" in
//	           dim, which erased the information along with the motion.)
//	working  → frozen spinner frame, its normal hue — "busy, fine".
//	finished → its normal glyph and hue, static as always.
//	idle/absent → dim; genuinely quiet rows are the only ones that recede.
//
// Dead/urgent rows never reach here — they pierce fully animated.
func dampMarker(att proto.Attention, glyph string) (newGlyph, color string) {
	switch att {
	case proto.AttWaiting:
		return "●", thWaiting(0)
	case proto.AttWorking:
		return WorkFrames[0], thWorking()
	case proto.AttFinished:
		return glyph, thDone()
	default:
		return glyph, thDim()
	}
}
