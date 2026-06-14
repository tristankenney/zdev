// internal/render/review_gauge.go
//
// Sidebar review-gauge section — the renderer-side surface of
// Snapshot.ReviewGauge (phase4-v21, roadmap NOW#4). The daemon's
// computeReviewGauge is the single source of truth for classification,
// repo grouping, and longest-rotting-first ordering; this file only draws
// the top of that gauge. Each entry shows the dominant-bucket glyph, the
// resolved repo, the non-zero bucket counts, and the longest-rotting age:
//
//	◆ zitcha/agora 2 ready 31m
//	✗ zitcha/backend 1 fix
//	⌁ solo/tool 1 rot 12m
//	─────────────────
//
// This is the permanent occupant of the slot the demoted triage strip
// vacated (ROADMAP 3c → NOW#4). It clears the bar the strip failed: it
// shows information NOT already in the project list — review-bandwidth
// across a fleet of worktrees, the "what can I land right now" question no
// per-row marker answers. Decoupled from the flaky `finished` glyph; the
// daemon classifies from PR/CI/dirty state.
package render

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// reviewGaugeSectionMax caps the gauge at the top-N longest-rotting repos so
// it stays a "what to land next" glance rather than a second project list —
// the precise failure (a second list, not a ranking) that demoted the strip.
const reviewGaugeSectionMax = 3

// ReviewGaugeEnabled gates the gauge. Default OFF per the knob convention
// (ZDEV_SIDEBAR_* — current behavior is the default): cmd/zdev-sidebar sets
// this from ZDEV_SIDEBAR_REVIEW=1. When false, renderReviewGauge writes
// nothing, so the sidebar is byte-identical to today.
var ReviewGaugeEnabled = false

// renderReviewGauge writes the review-gauge strip to buf. Writes NOTHING (zero
// rows) when disabled or when the gauge is empty (nil/no repos) — quiet
// sidebars and gauge-off sidebars are byte-identical to the pre-gauge layout.
// Returns the number of rows written so callers can account for click-row
// offsets (mirrors renderTriageSection).
func renderReviewGauge(buf *bytes.Buffer, snap *proto.Snapshot, width int) int {
	if !ReviewGaugeEnabled || snap.ReviewGauge == nil || len(snap.ReviewGauge.Repos) == 0 {
		return 0
	}

	rows := 0
	for _, r := range snap.ReviewGauge.Repos {
		if rows == reviewGaugeSectionMax {
			break
		}

		// Lead glyph + color by dominant bucket: ready (green ◆ — landable
		// now) outranks needs-fix (orange ✗) outranks will-rot (yellow ⌁).
		var glyph, color string
		switch {
		case r.Ready > 0:
			glyph, color = "◆", Green
		case r.NeedsFix > 0:
			glyph, color = "✗", Orange
		default:
			glyph, color = "⌁", Yellow
		}

		// Name budget leaves room for the glyph slot, the count segments, and
		// the age — same 2-space indent + glyph grid as a compact project row.
		nameCap := width - 22
		if nameCap < 8 {
			nameCap = 8
		}

		buf.WriteString("  ")
		buf.WriteString(color)
		buf.WriteString(glyph)
		buf.WriteString(Reset)
		buf.WriteString(" ")
		buf.WriteString(truncateRunes(r.Repo, nameCap))

		// Non-zero bucket counts only, each in its bucket color. Ready is the
		// headline (green); fix orange; rot dim-yellow.
		writeCount(buf, r.Ready, "ready", Green)
		writeCount(buf, r.NeedsFix, "fix", Orange)
		writeCount(buf, r.WillRot, "rot", Yellow)

		// Longest-rotting age (the repo's OldestSec) in dim — the "how long
		// has the readiest thing waited" signal that drives the ordering.
		if r.OldestSec > 0 {
			buf.WriteString(" ")
			buf.WriteString(Dim)
			buf.WriteString(formatAge(r.OldestSec))
			buf.WriteString(Reset)
		}

		buf.WriteString(ClearLineEnd)
		buf.WriteByte('\n')
		rows++
	}
	if rows == 0 {
		return 0
	}

	// Closing divider — same shape as the header/triage dividers so the gauge
	// reads as its own boxed region above the stable project list.
	buf.WriteString("  ")
	buf.WriteString(Dim)
	buf.WriteString(strings.Repeat("─", 17))
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
	return rows + 1
}

// writeCount appends " N label" in color when n > 0; no-op otherwise.
func writeCount(buf *bytes.Buffer, n int, label, color string) {
	if n <= 0 {
		return
	}
	buf.WriteString(" ")
	buf.WriteString(color)
	buf.WriteString(strconv.Itoa(n))
	buf.WriteString(" ")
	buf.WriteString(label)
	buf.WriteString(Reset)
}
