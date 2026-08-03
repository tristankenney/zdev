package main

import (
	"fmt"
	"strings"

	zone "github.com/lrstanley/bubblezone"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/render"
)

// The global bubblezone manager backs the popup's mouse support — same
// init()-not-main() rationale as cmd/zdev-round's: tests that call View()
// directly get a live manager too.
func init() { zone.NewGlobal() }

// gistMaxLen caps the title/gist column, matching cmd/zdev-round's.
const gistMaxLen = 50

// nameColWidth is the project-name column's fixed width — narrower than
// Round's (28) because the boundary popup's box border eats horizontal
// room the Round's borderless view doesn't have to budget for.
const nameColWidth = 20

// boxMinWidth/boxDefaultWidth bound the box's total border width in
// columns. A WindowSizeMsg narrower than boxMinWidth (a tiny terminal, or a
// test harness that never sends one) falls back to boxDefaultWidth rather
// than drawing a box with a negative fill count.
const boxMinWidth = 40
const boxDefaultWidth = 70

// View renders the boundary review: a bordered box (the brief's visual
// contract) with the held set first, a blank line, then the demand set,
// each row built from internal/render's EXPORTED tokens exactly like
// cmd/zdev-round's View — see that file's comment for why raw ANSI
// constants rather than lipgloss.Style are the right tool in a real
// attached terminal.
func (m *boundaryModel) View() string {
	if len(m.rows) == 0 {
		return m.viewEmpty()
	}

	w := m.boxWidth()
	heldCount := 0
	for _, r := range m.rows {
		if r.Section == sectionHeld {
			heldCount++
		}
	}
	title := buildTitle(m.snap, heldCount)

	var b strings.Builder
	b.WriteString(boxTop(title, w))
	b.WriteByte('\n')

	lastSection := ""
	for i, r := range m.rows {
		if r.Section != lastSection {
			if lastSection != "" {
				b.WriteString("\n") // blank separator between sections
			}
			header := "HELD WHILE YOU WORKED"
			if r.Section == sectionDemand {
				header = "DEMANDING NOW"
			}
			fmt.Fprintf(&b, "%s%s%s\n", render.Bold, header, render.Reset)
			lastSection = r.Section
		}

		cursor := "  "
		if i == m.cursor {
			cursor = render.Bold + render.Cyan + "▶ " + render.Reset
		}
		line := cursor + rowGlyphAndText(r)
		// Each row is a mouse zone keyed by its ZoneKey — hover moves the
		// cursor, click picks, right-click defers (handleMouse).
		b.WriteString(zone.Mark(r.ZoneKey, line))
		b.WriteByte('\n')
	}

	b.WriteString(boxBottom(w))
	b.WriteByte('\n')
	b.WriteString(m.viewFooter())
	// Scan registers every marked zone's final position and strips the
	// zero-width markers — MUST wrap the root view, nothing after it.
	return zone.Scan(b.String())
}

// boxWidth clamps m.width (set by WindowSizeMsg, or the model's construction
// default) to a sane floor so boxTop/boxBottom never compute a negative
// fill/repeat count.
func (m *boundaryModel) boxWidth() int {
	if m.width < boxMinWidth {
		return boxDefaultWidth
	}
	return m.width
}

// viewEmpty is the brief's "nothing held — fleet is quiet ✓" — shown when
// there is nothing held and nothing demanding. Any key exits (handleKey).
func (m *boundaryModel) viewEmpty() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%snothing held — fleet is quiet ✓%s\n", render.Bold, render.Reset)
	fmt.Fprintf(&b, "%s(any key to exit)%s\n", render.Dim, render.Reset)
	return b.String()
}

// viewFooter renders the spinner-while-polling + the brief's exact key
// legend text.
func (m *boundaryModel) viewFooter() string {
	var b strings.Builder
	if m.polling {
		fmt.Fprintf(&b, "%s%s%s ", render.Icy, render.WorkFrames[0], render.Reset)
	}
	fmt.Fprintf(&b, "%s↑/↓ move · enter pick · d defer · D drop · q later%s\n", render.Dim, render.Reset)
	return b.String()
}

// rowGlyphAndText renders one row's glyph + name column + age + gist,
// matching cmd/zdev-round's row shape (glyph, padded name, age, dim gist).
func rowGlyphAndText(r boundaryRow) string {
	var glyph string
	if r.Section == sectionHeld {
		glyph = heldGlyph(r.Kind)
	} else {
		glyph = demandGlyph(r.Att, r.Cheap)
	}
	name := r.Project
	if name == "" {
		name = "(listless)" // no ANSI here — keeps the %-*s width math correct
	}
	return fmt.Sprintf("%s %-*s %s%4s%s  %s%s%s",
		glyph, nameColWidth, name,
		render.Dim, formatBoundaryAge(r.AgeSec), render.Reset,
		render.Dim, truncateBoundaryGist(r.Title), render.Reset)
}

// heldGlyph picks the marker glyph + color for a held item by Kind, per the
// phase 3B brief's fixed vocabulary: "wait"=● (waiting hue — RedPulse, the
// same hue cmd/zdev-round uses for a waiting triage row), "parked"=+ (dim —
// a listless thought carries no urgency of its own), unknown Kind=· (dim,
// neutral) — the open-string rule: an unrecognized Kind must still render,
// never be dropped or crash the popup. There is no existing precedent for
// this mapping (zdev-show's `held` command prints Kind as plain dim text,
// no glyph) — this establishes it.
func heldGlyph(kind string) string {
	switch kind {
	case "wait":
		return render.RedPulse + "●" + render.Reset
	case "parked":
		return render.Dim + "+" + render.Reset
	default:
		return render.Dim + "·" + render.Reset
	}
}

// demandGlyph mirrors cmd/zdev-round's glyphFor exactly (same file:line
// precedent as isCheapWait/gistFor/waitOrActivityAge) so the "demanding
// now" section reads identically to the Round's queue for the same
// project attention — the sidebar's marker grammar, reused rather than
// reinvented.
func demandGlyph(att proto.Attention, cheap bool) string {
	switch {
	case att == proto.AttDead:
		return render.RedPulse + "✗" + render.Reset
	case att == proto.AttWaiting && cheap:
		return render.Orange + "⚡" + render.Reset
	case att == proto.AttWaiting:
		return render.RedPulse + "●" + render.Reset
	case att == proto.AttWorking:
		return render.Icy + "◐" + render.Reset
	default: // finished
		return render.Yellow + "◆" + render.Reset
	}
}

// truncateBoundaryGist caps the gist/title column exactly like
// cmd/zdev-round's truncateGist.
func truncateBoundaryGist(gist string) string {
	if len(gist) > gistMaxLen {
		return gist[:gistMaxLen-3] + "..."
	}
	return gist
}

// formatBoundaryAge mirrors cmd/zdev-round's formatRoundAge exactly (same
// second/minute/hour/day buckets) so the age column reads identically
// across every zdev popup.
func formatBoundaryAge(sec int64) string {
	switch {
	case sec <= 0:
		return "-"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	default:
		return fmt.Sprintf("%dd", sec/86400)
	}
}

// boxTop/boxBottom draw the hand-rolled rounded border cmd/zdev-park's
// View already established (lipgloss may be imported ONLY by
// internal/render/lipgloss.go — scripts/check-no-lipgloss-scatter.sh
// enforces this at build time), parameterized on width since the boundary
// popup is a resizable percentage-sized popup (config/zdev.tmux.conf:
// `-w 70% -h 60%`), unlike park's fixed 60x5.
func boxTop(title string, width int) string {
	label := " " + title + " "
	fill := width - 4 - len(label)
	if fill < 0 {
		fill = 0
	}
	return render.Dim + "╭─" + render.Reset +
		render.Bold + label + render.Reset +
		render.Dim + strings.Repeat("─", fill) + "─╮" + render.Reset
}

func boxBottom(width int) string {
	n := width - 2
	if n < 0 {
		n = 0
	}
	return render.Dim + "╰" + strings.Repeat("─", n) + "╯" + render.Reset
}
