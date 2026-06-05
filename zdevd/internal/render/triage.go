// internal/render/triage.go
//
// Sidebar triage section — the renderer-side surface of Snapshot.Triage
// (phase4-v9). The daemon's rankTriage is the single source of truth for
// ordering; this file only draws the top of that queue. Entries show the
// cost-class glyph, the project name, and the wait age, so the user can
// answer "what should I handle next" without scanning the full list:
//
//	⚡ zitcha/agora-b 40s
//	● zitcha/agora-a 14m
//	◆ zitcha/backend 31m
//	─────────────────
//
// Glyphs: ⚡ needs-permission (cheap y/n — ranked first), pulsing ●
// needs-decision, ◆ finished-for-review. The closing divider separates
// the strip from the stable alphabetical list below, which is NEVER
// reordered — the section carries the ranking so row positions keep
// their spatial memory.
package render

import (
	"bytes"
	"strings"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// triageSectionMax caps the strip at the top of the queue. Three entries
// keeps it a "next actions" block rather than a second project list; the
// full queue lives in `zdev triage`.
const triageSectionMax = 3

// renderTriageSection writes the triage strip to buf. Writes NOTHING
// (zero rows) when the queue is empty — quiet sidebars are byte-identical
// to the pre-triage layout. Returns the number of rows written so callers
// can account for click-row offsets.
func renderTriageSection(buf *bytes.Buffer, snap *proto.Snapshot, width int, animator *Animator, nowFn func() int64) int {
	if len(snap.Triage) == 0 {
		return 0
	}
	now := nowFn()

	// Index the wire rows once; triage names are canonical Projects[].Name.
	byName := make(map[string]*proto.Project, len(snap.Projects))
	for i := range snap.Projects {
		byName[snap.Projects[i].Name] = &snap.Projects[i]
	}

	rows := 0
	for _, name := range snap.Triage {
		if rows == triageSectionMax {
			break
		}
		if name == snap.CurrentSession && snap.CurrentSession != "" {
			// The strip answers "what needs me ELSEWHERE" — the session
			// this sidebar lives in is already in front of the user; its
			// waiting state shows on its own (current) row below.
			continue
		}
		p, ok := byName[name]
		if !ok {
			continue // queue/projects raced; skip rather than mislead
		}

		var glyph, color string
		switch {
		case p.Attention == proto.AttDead:
			glyph, color = "✗", RedPulse
		case p.Attention == proto.AttWaiting && p.WaitKind == proto.WaitKindPermission:
			glyph, color = "⚡", Orange
		case p.Attention == proto.AttWaiting:
			var ageSec int64
			if p.WaitStartedTS > 0 {
				ageSec = now - p.WaitStartedTS
			}
			glyph, color = animator.PulseGlyphAt(ageSec), RedPulse
		default: // finished
			glyph, color = "◆", Yellow
		}

		var age string
		switch {
		case p.WaitStartedTS > 0:
			age = formatAge(now - p.WaitStartedTS)
		case p.LastActivityTS > 0:
			age = formatAge(now - p.LastActivityTS)
		}

		// "  " + glyph + " " + name + " " + age — same 2-space indent and
		// glyph slot as a compact project row so the eye tracks one grid.
		// Name budget mirrors renderCompactRow's soft cap.
		nameCap := width - 14
		if nameCap < 10 {
			nameCap = 10
		}
		buf.WriteString("  ")
		buf.WriteString(color)
		buf.WriteString(glyph)
		buf.WriteString(Reset)
		buf.WriteString(" ")
		buf.WriteString(truncateRunes(p.Name, nameCap))
		if age != "" {
			buf.WriteString(" ")
			buf.WriteString(Dim)
			buf.WriteString(age)
			buf.WriteString(Reset)
		}
		buf.WriteString(ClearLineEnd)
		buf.WriteByte('\n')
		rows++
	}
	if rows == 0 {
		return 0 // every queue entry raced away — no divider for nothing
	}

	// Closing divider — same shape as the header divider so the strip
	// reads as its own boxed region.
	buf.WriteString("  ")
	buf.WriteString(Dim)
	buf.WriteString(strings.Repeat("─", 17))
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
	return rows + 1
}
