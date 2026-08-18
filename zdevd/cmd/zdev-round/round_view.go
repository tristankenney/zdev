package main

import (
	"fmt"
	"strings"

	zone "github.com/lrstanley/bubblezone"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/render"
)

// The global bubblezone manager backs the popup's mouse support: each queue
// row is Marked with its project name, Scan at the View root records where
// every row actually landed and strips the zero-width markers, and Update's
// MouseMsg handler asks "which row is under the pointer" instead of doing
// coordinate math. init() rather than main() so tests that call View()
// directly get a live manager too.
func init() { zone.NewGlobal() }

// gistMaxLen caps the gist column exactly like zdev-show's triage view, so
// the Round's queue reads identically to `zdev-show triage` for the same
// row.
const gistMaxLen = 60

// nameColWidth mirrors zdev-show's %-24s but a little wider — the popup has
// real room (bin/zdev-triage-popup opens it at -w 80% -h 70%) and project
// names run longer than session-name truncation implies.
const nameColWidth = 28

// View renders the Round popup. Styling is built entirely from
// internal/render's EXPORTED tokens (the plain ANSI constants ansi.go/
// theme.go already export — Dim, Bold, Yellow, RedPulse, Icy, Orange,
// Reset) rather than the lipgloss Style* constructors: this is a real
// attached terminal (unlike cmd/zdev-sidebar's tty-detection-averse
// WithInput(nil) renderer), so the pinned-renderer workaround those
// constructors exist for doesn't apply here, and zdev-show already
// established this exact pattern (locally mirrored copies of the same
// codes) for CLI-shaped output. Reusing render's tokens directly instead of
// re-mirroring them is strictly better: no drift between what the sidebar
// legend calls "waiting"/"dead"/"working"/"done" and what this popup shows.
func (m *roundModel) View() string {
	if len(m.rows) == 0 {
		return m.viewEmpty()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sRound%s %s— %d in queue%s\n\n",
		render.Bold, render.Reset, render.Dim, len(m.rows), render.Reset)

	start, end := m.offset, m.offset+m.visN
	if m.visN == 0 || end > len(m.rows) {
		end = len(m.rows)
	}
	if start > 0 {
		fmt.Fprintf(&b, "%s      · %d above ·%s\n", render.Dim, start, render.Reset)
	}
	for i := start; i < end; i++ {
		r := m.rows[i]
		cursor := "  "
		if i == m.cursor {
			cursor = render.Bold + render.Cyan + "▶ " + render.Reset
		}
		line := fmt.Sprintf("%s%s %-*s %s%4s%s  %s%s%s",
			cursor, glyphFor(r), nameColWidth, r.Name,
			render.Dim, formatRoundAge(r.AgeSec), render.Reset,
			render.Dim, truncateGist(r.Gist), render.Reset)
		// Each row is a mouse zone keyed by its project name — hover moves
		// the cursor, click jumps, right-click defers (see handleMouse).
		b.WriteString(zone.Mark(r.Name, line))
		b.WriteByte('\n')
	}
	if end < len(m.rows) {
		fmt.Fprintf(&b, "%s      · %d more ·%s\n", render.Dim, len(m.rows)-end, render.Reset)
	}

	b.WriteString("\n")
	b.WriteString(m.viewFooter())
	// Scan registers every marked zone's final position and strips the
	// zero-width markers — MUST wrap the root view, nothing after it.
	return zone.Scan(b.String())
}

// viewEmpty is the brief's "friendly 'fleet is quiet' state" — shown when
// the queue has nothing left (either nothing needed attention this Round,
// or the operator burned it all down). Any key exits (handleKey).
func (m *roundModel) viewEmpty() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%sfleet is quiet ✓%s\n", render.Bold, render.Reset)
	fmt.Fprintf(&b, "%s(any key to exit)%s\n", render.Dim, render.Reset)
	return b.String()
}

// viewFooter renders the live tally + spinner-while-polling + the key
// legend GENERATED from the same bindings handleKey dispatches on
// (bubbles help/key — park's precedent; a hand-written legend had already
// drifted between this popup and boundary's for identical handlers). The
// spinner is bubbles/spinner over the sidebar's own work frames — it
// spins; a frozen ◐ reads as a hung process.
func (m *roundModel) viewFooter() string {
	var b strings.Builder
	if m.polling {
		fmt.Fprintf(&b, "%s%s%s ", render.Icy, m.spin.View(), render.Reset)
	}
	fmt.Fprintf(&b, "%d handled · %d deferred · %d left\n", m.handledN, m.deferredN, len(m.rows))
	b.WriteString(render.Dim)
	b.WriteString(m.help.ShortHelpView(m.keys.ShortHelp()))
	b.WriteString(render.Reset)
	b.WriteByte('\n')
	return b.String()
}

// glyphFor picks the marker glyph + color for one row, matching the sidebar
// legend exactly: waiting=● (red pulse; orange ⚡ for a cheap/permission
// wait), dead=✗ (red pulse), working=◐ (icy — included for legend parity
// even though Snapshot.Triage never actually ranks a working row), done
// (finished)=◆ (yellow).
func glyphFor(r roundRow) string {
	switch {
	case r.Att == proto.AttDead:
		return render.RedPulse + "✗" + render.Reset
	case r.Att == proto.AttWaiting && r.Cheap:
		return render.Orange + "⚡" + render.Reset
	case r.Att == proto.AttWaiting:
		return render.RedPulse + "●" + render.Reset
	case r.Att == proto.AttWorking:
		return render.Icy + "◐" + render.Reset
	default: // finished
		return render.Yellow + "◆" + render.Reset
	}
}

// truncateGist caps the gist column exactly like zdev-show's triage view.
// Cell-safe (calm lane C): gists are agent-authored UTF-8 — a byte slice
// can cut a rune in half and emit invalid bytes into the frame.
func truncateGist(gist string) string {
	return render.CellTruncate(gist, gistMaxLen, "...")
}

// formatRoundAge mirrors zdev-show's formatAge buckets exactly (seconds,
// minutes, hours, days) so the Round's age column reads identically to
// `zdev-show triage`'s.
func formatRoundAge(sec int64) string {
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
