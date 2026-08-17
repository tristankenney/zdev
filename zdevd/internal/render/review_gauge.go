// internal/render/review_gauge.go
//
// Sidebar review-gauge section — the renderer-side surface of
// Snapshot.ReviewGauge (phase4-v21, roadmap NOW#4). The daemon's
// computeReviewGauge is the single source of truth for classification,
// repo grouping, and longest-rotting-first ordering; this file only draws
// the top of that gauge. Each entry shows the dominant-bucket glyph, the
// resolved repo, the non-zero bucket counts, and the longest-rotting age.
// classic keeps the original text-only shape:
//
//	◆ zitcha/agora 2 ready 31m
//	✗ zitcha/backend 1 fix
//	⌁ solo/tool 1 rot 12m
//	─────────────────
//
// rose-pine (ZDEV_SIDEBAR_THEME=rose-pine) instead draws a compact block
// bar per repo — the landing state at a glance, not just a number — plus a
// short text tail, e.g.:
//
//	◆ zitcha/agora       ██ clean 31m
//	⌁ solo/tool          █  1 rotting 12m
//	✗ zitcha/backend     ░  1 pending
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

// reviewGaugeBarCellsMax caps the rose-pine bar at this many glyphs even
// when a repo's contributing-row count is larger — "compact" (brief) means
// bounded width, not one glyph per row for a repo with a dozen worktrees.
const reviewGaugeBarCellsMax = 6

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

	rosePine := ThemeMode == "rose-pine"

	rows := 0
	for _, r := range snap.ReviewGauge.Repos {
		if rows == reviewGaugeSectionMax {
			break
		}

		// Lead glyph + color by dominant bucket: ready (landable now)
		// outranks needs-fix outranks will-rot. Colors route through the
		// theme seam — classic byte-shape unchanged (each th* falls back
		// to the constant this was authored with); rose-pine stops mixing
		// classic xterm glyphs into truecolor rows.
		var glyph, color string
		switch {
		case r.Ready > 0:
			glyph, color = "◆", thChipAccent(Green)
		case r.NeedsFix > 0:
			glyph, color = "✗", thChipAccent(Orange)
		default:
			glyph, color = "⌁", thChipAccent(Yellow)
		}

		buf.WriteString("  ")
		buf.WriteString(color)
		buf.WriteString(glyph)
		buf.WriteString(Reset)
		buf.WriteString(" ")

		if rosePine {
			writeReviewGaugeRosePineRow(buf, r, width)
		} else {
			// Name budget leaves room for the glyph slot, the count
			// segments, and the age — same 2-space indent + glyph grid as
			// a compact project row.
			nameCap := width - 22
			if nameCap < 8 {
				nameCap = 8
			}
			buf.WriteString(truncateRunes(r.Repo, nameCap))

			// Non-zero bucket counts only, each in its bucket color. Ready is
			// the headline (green); fix orange; rot dim-yellow.
			writeCount(buf, r.Ready, "ready", thChipAccent(Green))
			writeCount(buf, r.NeedsFix, "fix", thChipAccent(Orange))
			writeCount(buf, r.WillRot, "rot", thChipAccent(Yellow))

			// Longest-rotting age (the repo's OldestSec) in dim — the "how
			// long has the readiest thing waited" signal that drives the
			// ordering.
			if r.OldestSec > 0 {
				buf.WriteString(" ")
				buf.WriteString(thDim())
				buf.WriteString(formatAge(r.OldestSec))
				buf.WriteString(Reset)
			}
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
	buf.WriteString(thDim())
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

// ---- rose-pine: the bar ----
//
// The row-count contract (one line per repo, click-row math depends on it)
// is unaffected by everything below: writeReviewGaugeRosePineRow only ever
// appends to the CURRENT line — it never writes a newline. The cascade
// below only trades off which pieces of the SAME line survive at narrow
// widths; the caller's ClearLineEnd + '\n' framing stays exactly where it
// was for the classic branch.

// writeReviewGaugeRosePineRow writes the repo name (padded to a fixed
// column so every row's bar starts in the same place), the block bar, the
// text tail, and the age — shrinking gracefully so the row never exceeds
// width. All sizing decisions (reviewGaugeLayout) happen on PLAIN text
// before any ANSI is applied, so truncation never risks cutting an escape
// sequence in half; color/bold is layered on only after the layout is
// final.
func writeReviewGaugeRosePineRow(buf *bytes.Buffer, r proto.ReviewRepo, width int) {
	nameCap, barCells, tail, ageStr := reviewGaugeLayout(width, r)

	name := truncateRunes(r.Repo, nameCap)
	buf.WriteString(StyleGaugeName(nameCap).Render(name))

	if barCells > 0 {
		buf.WriteString(" ")
		buf.WriteString(reviewGaugeBar(r, barCells))
	}
	if tail != "" {
		buf.WriteString(" ")
		buf.WriteString(reviewGaugeRenderTail(tail))
	}
	if ageStr != "" {
		buf.WriteString(" ")
		buf.WriteString(thDim())
		buf.WriteString(ageStr)
		buf.WriteString(Reset)
	}
}

// reviewGaugeLayout decides how much of the rose-pine row's optional
// content (bar cells, tail text, age) survives at the given pane width.
// Everything here operates on PLAIN strings/counts — no ANSI in sight —
// which is what lets the shrink loop just compare rune counts against
// `width` without a visWidth pass.
//
// Priority, most-disposable first: age, then bar cells one at a time, then
// the tail (dropped whole rather than mid-word truncated — "1 rott…" reads
// worse than no tail at all), then the name floor (matches the classic
// floor of 8). This keeps the two pieces of information every row commits
// to showing — the dominant glyph and (as much as fits of) the repo name —
// last to go.
func reviewGaugeLayout(width int, r proto.ReviewRepo) (nameCap, barCells int, tail, ageStr string) {
	nameCap = width - 22
	if nameCap < 8 {
		nameCap = 8
	}
	if nameCap > 20 {
		// Don't let the name column hog the row on wide panes — the gauge
		// stays a compact glance, not a second project list with the name
		// column stretched to match.
		nameCap = 20
	}

	total := r.Ready + r.NeedsFix + r.WillRot
	barCells = total
	if barCells > reviewGaugeBarCellsMax {
		barCells = reviewGaugeBarCellsMax
	}

	tail = reviewGaugeTailText(r)

	if r.OldestSec > 0 {
		ageStr = formatAge(r.OldestSec)
	}

	fixedWidth := func() int {
		// prefix "  " + glyph + " " is accounted by the caller before this
		// function is reached; this budgets only what THIS function
		// controls: the padded name plus the optional trailing pieces.
		n := nameCap
		if barCells > 0 {
			n += 1 + barCells
		}
		if tail != "" {
			n += 1 + len([]rune(tail))
		}
		if ageStr != "" {
			n += 1 + len([]rune(ageStr))
		}
		return n
	}

	// Fixed prefix already written by the caller: "  " + glyph + " " = 4.
	budget := width - 4

	for fixedWidth() > budget {
		switch {
		case ageStr != "":
			ageStr = ""
		case barCells > 0:
			barCells--
		case tail != "":
			tail = ""
		case nameCap > 3:
			nameCap--
		default:
			// Nothing left to shrink — a pathologically narrow pane. Accept
			// the overflow rather than loop forever; ClearLineEnd still
			// clamps what the terminal keeps on screen.
			return
		}
	}
	return
}

// reviewGaugeTailText returns the plain-text (uncolored, unbolded) tail for
// a repo, using ONLY fields already on Snapshot.ReviewGauge — Ready,
// NeedsFix, WillRot. Precedence mirrors the repo's sort key (longest-
// rotting first): a repo that will rot says so first, since that is the
// time-sensitive case; a repo blocked on a fix is "pending" (something else
// has to happen before it's actionable); a repo with nothing but ready rows
// is "clean" — no debt, just land it.
func reviewGaugeTailText(r proto.ReviewRepo) string {
	switch {
	case r.WillRot > 0:
		return strconv.Itoa(r.WillRot) + " rotting"
	case r.NeedsFix > 0:
		return strconv.Itoa(r.NeedsFix) + " pending"
	case r.Ready > 0:
		return "clean"
	default:
		return ""
	}
}

// reviewGaugeRenderTail applies the ONE bold accent the tail gets — the
// leading count, via the pinned renderer's StyleBold (its first production
// call site: bold is a profile-independent SGR attribute, so it composes
// safely with the ANSI256-pinned Renderer without the truecolor-downsample
// risk that would come from routing bar/text COLOR through lipgloss; see
// reviewGaugeBar's doc for that trade-off). "clean" has no count to bold.
func reviewGaugeRenderTail(tail string) string {
	if tail == "clean" {
		return tail
	}
	if i := strings.IndexByte(tail, ' '); i > 0 {
		return StyleBold().Render(tail[:i]) + tail[i:]
	}
	return tail
}

// reviewGaugeBar renders the block bar: one solid █ per Ready/WillRot unit
// in the theme's positive/negative hue, one hatched ░ per NeedsFix unit in
// muted — pending-on-CI reads differently from actively rotting even at a
// glance. Colors reuse thChipAccent's existing Green/RedPulse/Dim mapping
// (rpPine/rpLove/rpMuted) rather than inventing new rose-pine tokens.
//
// Deliberately NOT routed through a lipgloss Style: the pinned Renderer is
// fixed to termenv.ANSI256 (lipgloss.go's whole reason for existing), and a
// truecolor hex Foreground under that profile gets silently downsampled to
// its nearest xterm-256 match (confirmed empirically: Foreground(Color(
// "#3e8fb0")) renders "\x1b[38;5;67m", not the exact rpPine bytes) — exactly
// the precision loss theme_rosepine.go's rpRGB tokens exist to avoid
// everywhere else in this theme. Raw concatenation of thChipAccent's token
// keeps the bar on the same truecolor-exact footing as the rest of
// rose-pine; lipgloss earns its keep elsewhere in this file (StyleGaugeName,
// StyleBold) where its job is layout/attribute, not color.
//
// cells caps the total glyph count; when a repo's total exceeds it, counts
// are apportioned proportionally (integer floor, non-zero buckets always
// keep at least one glyph, remainder handed to the numerically largest
// bucket) so the bar always sums to exactly `cells`.
func reviewGaugeBar(r proto.ReviewRepo, cells int) string {
	if cells <= 0 {
		return ""
	}
	total := r.Ready + r.NeedsFix + r.WillRot
	if total <= 0 {
		return ""
	}

	ready, rot, fix := r.Ready, r.WillRot, r.NeedsFix
	if total > cells {
		ready = reviewGaugeShare(r.Ready, total, cells)
		rot = reviewGaugeShare(r.WillRot, total, cells)
		fix = reviewGaugeShare(r.NeedsFix, total, cells)

		for ready+rot+fix > cells {
			switch reviewGaugeLargestIdx(ready, rot, fix) {
			case 0:
				ready--
			case 1:
				rot--
			default:
				fix--
			}
		}
		for ready+rot+fix < cells {
			// Grow the numerically largest ORIGINAL bucket (not the
			// already-shrunk share) so the biggest real bucket also reads
			// biggest on the bar.
			switch reviewGaugeLargestIdx(r.Ready, r.WillRot, r.NeedsFix) {
			case 0:
				ready++
			case 1:
				rot++
			default:
				fix++
			}
		}
	}

	var b strings.Builder
	reviewGaugeWriteSegment(&b, ready, "█", thChipAccent(Green))
	reviewGaugeWriteSegment(&b, rot, "█", thChipAccent(RedPulse))
	reviewGaugeWriteSegment(&b, fix, "░", thChipAccent(Dim))
	return b.String()
}

// reviewGaugeShare apportions one bucket's cell count by simple floor
// division, keeping any non-zero bucket visible (minimum 1 cell) so a
// small-but-real count never vanishes from the bar entirely.
func reviewGaugeShare(count, total, cells int) int {
	if count <= 0 {
		return 0
	}
	n := count * cells / total
	if n == 0 {
		n = 1
	}
	return n
}

// reviewGaugeLargestIdx returns the index (0=ready, 1=rot, 2=fix) of the
// largest of the three, breaking ties toward the earlier (higher-priority)
// bucket.
func reviewGaugeLargestIdx(ready, rot, fix int) int {
	if ready >= rot && ready >= fix {
		return 0
	}
	if rot >= fix {
		return 1
	}
	return 2
}

// reviewGaugeWriteSegment appends n copies of glyph in color when n > 0;
// no-op otherwise (mirrors writeCount's shape for the classic branch).
func reviewGaugeWriteSegment(b *strings.Builder, n int, glyph, color string) {
	if n <= 0 {
		return
	}
	b.WriteString(color)
	for i := 0; i < n; i++ {
		b.WriteString(glyph)
	}
	b.WriteString(Reset)
}
