package render

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// scrollNowMs returns the current wall-clock in unix milliseconds for
// sub-second marquee animation (260512-cmx). Indirected as a package-level
// var so tests can pin it deterministically without bringing time.Now into
// the production path of more code.
var scrollNowMs = func() int64 { return time.Now().UnixMilli() }

// RenderUnreachable returns the Phase 1 fallback frame the renderer prints
// when it cannot reach zdevd (Subscribe failure). The frame mirrors the
// 2-line shape of RenderStub so the pane geometry is unchanged: bold
// "zdev projects" header followed by a single body row "  · (zdevd
// unreachable: <reason>)". Reconnect logic is deferred to Phase 4 per
// CONTEXT D-12 — Phase 1 just keeps the pane open with the message visible
// until the user closes it.
func RenderUnreachable(reason string, width int) []byte {
	var buf bytes.Buffer

	buf.WriteString(CursorHome)
	buf.WriteString(Bold)
	buf.WriteString("  zdev projects")
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	buf.WriteString("  · (zdevd unreachable: ")
	buf.WriteString(reason)
	buf.WriteString(")")
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	buf.WriteString(ClearToEnd)
	return buf.Bytes()
}

// RenderStub returns the Phase 1 2-line skeleton bytes per CONTEXT D-11.
//
// The output mirrors the bash baseline's header byte sequence at
// ~/.local/bin/zdev-sidebar-render line 69 and the body_lines pattern at
// line 622 — Phase 3 expands the body with markers, chips, and animation,
// but the framing (cursor home → header → rows → clear-to-end) is stable.
//
// The width parameter is accepted for API stability with Phase 3 (truncation
// lives there); Phase 1 doesn't truncate the short "  · stub" row at any
// realistic terminal width.
//
// Output shape (bytes):
//
//	\x1b[H                         (CursorHome — repaint anchor)
//	\x1b[1m  zdev projects\x1b[0m  (Bold header)
//	\x1b[K\n                       (ClearLineEnd + LF)
//	  · {project name}             (single body row, 2-space indent)
//	\x1b[K\n                       (ClearLineEnd + LF)
//	\x1b[J                         (ClearToEnd — wipe leftover frame)
func RenderStub(snap *proto.Snapshot, width int) []byte {
	var buf bytes.Buffer

	// Cursor home before drawing — matches the bash full-screen-redraw
	// idiom at zdev-sidebar-render line 652 (`printf '\033[H%s\033[J'`).
	buf.WriteString(CursorHome)

	// Header line: "  zdev projects" in bold, line-clear suffix.
	// Mirrors zdev-sidebar-render line 69:
	//   HEADER=$'\033[1m  zdev projects\033[0m\033[K\n…'
	buf.WriteString(Bold)
	buf.WriteString("  zdev projects")
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Single body row: 2-space indent + alive marker + space + project
	// name. Mirrors the bash body_lines construction at line 622:
	//   body_lines+=("  ${marker} ${label}"$'\033[K')
	if len(snap.Projects) > 0 {
		buf.WriteString("  · ")
		buf.WriteString(snap.Projects[0].Name)
	} else {
		// Defensive: empty Projects emits an empty-marker line so the
		// overall frame still has the expected 2-line shape.
		buf.WriteString("  · ")
	}
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Clear-to-end clears any leftover content from previous (longer)
	// frames. Mirrors the bash render full-redraw at line 652.
	buf.WriteString(ClearToEnd)

	return buf.Bytes()
}

// Render is the Phase 3 full multi-row frame composer. Replaces
// RenderStub for production use; RenderStub remains available for
// Phase 1 stub-snapshot tests.
//
// nowFn is injected for testability — production callers pass
// time.Now().Unix; tests pass a fixed value.
//
// Frame structure:
//  1. CursorHome
//  2. Header: bold "  zdev projects <mood>" + ClearLineEnd + LF
//  3. Divider: "  ─────────────────" (17 U+2500) + ClearLineEnd + LF
//  4. For each project: 2 visual rows (marker + metadata, em-dash
//     placeholder when no chip applies; click-row math invariant
//     per Pitfall H).
//  5. Footer: "  N● N◎ N◆ N· N·" + ClearLineEnd + LF
//  6. ClearToEnd
//
// Source-of-truth: ~/.local/bin/zdev-sidebar-render lines 622-661.
func Render(snap *proto.Snapshot, width int, animator *Animator, nowFn func() int64) []byte {
	var buf bytes.Buffer
	buf.WriteString(CursorHome)

	// Header: bold "  zdev projects {mood}" + reset + clear + LF.
	// The trailing dim " [go]" build-tag was distilled out — it carried no
	// user signal, only renderer build identity (debug). For build identity
	// at runtime, use `zdevd diag` or `zdev-show --legend`.
	buf.WriteString(Bold)
	buf.WriteString("  zdev projects ")
	buf.WriteString(MoodFor(snap, nowFn))
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Divider: "  " + dim + 17xU+2500 + reset + clear + LF
	buf.WriteString("  ")
	buf.WriteString(Dim)
	buf.WriteString(strings.Repeat("─", 17))
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Per-project rows.
	var nWait, nRun, nDone, nAlive, nAbsent int
	for _, p := range snap.Projects {
		switch p.Status {
		case "waiting":
			nWait++
		case "shell-running":
			nRun++
		case "finished":
			nDone++
		case "absent":
			nAbsent++
		default:
			nAlive++
		}
		isCurrent := p.Name == snap.CurrentSession && snap.CurrentSession != ""
		urgent := isUrgent(&p, nowFn())
		// 260511-ohu change A: twoRows := isCurrent only (urgent dropped).
		// Non-current urgent projects now render as 1 compact row with the red ▌
		// prefix migrated into renderCompactRow.
		if isCurrent {
			renderProjectRow(&buf, &p, snap.CurrentSession, animator, nowFn, urgent)
			renderMetadataRow(&buf, &p, snap.CurrentSession, width, animator, nowFn, urgent)
		} else {
			renderCompactRow(&buf, &p, width, animator, nowFn, urgent)
		}
	}

	// Footer.
	buf.WriteString("  ")
	buf.WriteString(Dim)
	fmt.Fprintf(&buf, "%d● %d◎ %d◆ %d· %d·", nWait, nRun, nDone, nAlive, nAbsent)
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	buf.WriteString(ClearToEnd)
	return buf.Bytes()
}

// domainSep is the separator between sub-groups within a domain row.
// Dim " │ " keeps the bar visually subordinate to the chip colors it separates.
const domainSep = Dim + " │ " + Reset

// metadataPrefix returns the left-side prefix bytes for current-session
// metadata rows (marker row prefix + each populated domain row prefix
// use this verbatim). Branches:
//
//	urgent=true              → RedBorder + ▌ + Reset + 5 spaces
//	urgent=false + isCurrent → BreathColorForProject + ▌ + Reset + 5 spaces
//	default (defensive)      → 6 spaces
//
// isCurrent will always be true at the renderMetadataRow call site under the
// new dispatch (twoRows := isCurrent), so the default branch is dead in
// production — kept defensively to match marker-row symmetry.
func metadataPrefix(p *proto.Project, current string, animator *Animator, urgent bool) string {
	isCurrent := p.Name == current && current != ""
	var b bytes.Buffer
	switch {
	case urgent:
		b.WriteString(RedBorder)
		b.WriteString("▌")
		b.WriteString(Reset)
		b.WriteString("     ")
	case isCurrent:
		b.WriteString(BreathColorForProject(p.Name, animator.BreathFrame()))
		b.WriteString("▌")
		b.WriteString(Reset)
		b.WriteString("     ")
	default:
		b.WriteString("      ")
	}
	return b.String()
}

// renderDomainRow writes one domain-grouped metadata row to buf with the
// given full prefix string (computed by metadataPrefix) and Dim leading
// glyph. The row body is composed by `write` against a local inner buffer;
// if inner is empty after `write`, the entire row is suppressed (no prefix,
// no glyph, no newline).
//
// Row format (non-empty case):
//
//	prefix + Dim + glyph + " " + Reset + innerBody + ClearLineEnd + "\n"
//
// Leading-space trim on innerBody mirrors the original renderMetadataRow
// behavior — chip writers may emit a leading space via spaceIf when the
// first chip doesn't write anything.
func renderDomainRow(buf *bytes.Buffer, prefix string, glyph string, write func(inner *bytes.Buffer)) {
	var inner bytes.Buffer
	write(&inner)
	if inner.Len() == 0 {
		return
	}
	body := inner.Bytes()
	if body[0] == ' ' {
		body = body[1:]
	}
	buf.WriteString(prefix)
	buf.WriteString(Dim)
	buf.WriteString(glyph)
	buf.WriteString(" ")
	buf.WriteString(Reset)
	buf.Write(body)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// joinNonEmpty writes the non-empty members of subs to dst, separated by
// sep. Empty buffers are skipped entirely — there is never a sep written
// with nothing on one side.
func joinNonEmpty(dst *bytes.Buffer, subs []*bytes.Buffer, sep string) {
	first := true
	for _, s := range subs {
		if s.Len() == 0 {
			continue
		}
		if !first {
			dst.WriteString(sep)
		}
		dst.Write(s.Bytes())
		first = false
	}
}

// renderProjectRow composes the marker row for one project.
//
// Format: [prefix] + marker + " " + label + ClearLineEnd + LF
//
// Prefix dispatch (urgent wins over identity — 260511-nxy):
//
//	urgent=true          → {RedBorder}▌{Reset}" " (foreground-only red; no bg state to leak)
//	urgent=false+current → {BreathColorForProject}▌{Reset}" " (per-project breath bar, VIS-03)
//	otherwise            → "  " (2-space indent)
func renderProjectRow(buf *bytes.Buffer, p *proto.Project, current string, animator *Animator, nowFn func() int64, urgent bool) {
	isCurrent := p.Name == current && current != ""
	switch {
	case urgent:
		// Urgent left-border accent. Replaces the breath bar when current
		// (urgency wins over identity); replaces the "  " indent when non-current.
		// 260511-nxy: foreground-only red ▌ — no bg state to leak across rows.
		buf.WriteString(RedBorder)
		buf.WriteString("▌")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	case isCurrent:
		buf.WriteString(BreathColorForProject(p.Name, animator.BreathFrame()))
		buf.WriteString("▌")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	default:
		buf.WriteString("  ")
	}

	pForMarker := *p
	if isCurrent && pForMarker.Status == "waiting" {
		// Suppress the attention-drawing pulse when the user is present —
		// same rationale as zeroing agentClaude/agentPi in renderMetadataRow.
		pForMarker.Status = "alive"
	}
	glyph, color := MarkerFor(pForMarker, animator)
	// VIS-12 stale dim-out: alive + age >= StaleThreshold => Dim
	if p.Status == "alive" && p.LastActivityTS > 0 && nowFn()-p.LastActivityTS >= int64(StaleThresholdSec) {
		color = Dim
	}
	buf.WriteString(color)
	buf.WriteString(glyph)
	buf.WriteString(Reset)
	buf.WriteString(" ")

	if isCurrent {
		buf.WriteString(Bold)
		buf.WriteString(PaletteFor(p.Name))
	}
	buf.WriteString(p.Name)
	if isCurrent {
		buf.WriteString(Reset)
	}
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// renderMetadataRow composes up to 3 domain-grouped metadata rows for the
// current-session project (260511-ohu change B). Each domain row is written
// by renderDomainRow, which suppresses the row entirely when its inner buffer
// is empty — so a current project with no metadata produces 0 domain rows.
//
// Domain rows (each prefixed with metadataPrefix, then Dim leading glyph):
//
//	⎇  git:     branch+dirty | PR-or-celebrate | CI
//	▶  runtime: shell-cmd | ports
//	✻  agent:   wait-age only (agent chips suppressed for current session)
//
// Sub-groups within each row are joined by domainSep (" │ " Dim-wrapped) via
// joinNonEmpty on per-sub-group bytes.Buffers. Empty sub-groups are skipped
// (no double-separator artifacts).
//
// Prefix dispatch via metadataPrefix: urgent ▌+5sp / breath ▌+5sp / 6 spaces.
func renderMetadataRow(buf *bytes.Buffer, p *proto.Project, current string, width int, animator *Animator, nowFn func() int64, urgent bool) {
	prefix := metadataPrefix(p, current, animator, urgent)
	now := nowFn()

	// Git domain row: branch + dirty | PR-or-celebrate | CI
	renderDomainRow(buf, prefix, "⎇", func(inner *bytes.Buffer) {
		var subBranch, subPR, subCI bytes.Buffer

		// Sub-group 1: branch + dirty
		chipBranchWithCap(&subBranch, p.Branch, p.Ahead, p.Behind, 24)
		spaceIf(&subBranch)
		chipDirty(&subBranch, p.DirtyCount)

		// Sub-group 2: PR-or-celebrate (mutually exclusive)
		celebrating := chipCelebrate(&subPR, p.CelebrateUntil, now)
		if !celebrating {
			chipPRAggregate(&subPR, p.PROpen, p.PRFail, p.PRPend, false)
		}

		// Sub-group 3: CI — binary chip; failing-check names live on the
		// dedicated scrolling row (renderFailingChecksRow) below.
		chipCI(&subCI, p.CIStatus, p.CIConclusion)

		joinNonEmpty(inner, []*bytes.Buffer{&subBranch, &subPR, &subCI}, domainSep)
	})

	// CI-fails marquee row (260512-cgw, retimed in 260512-cmx): shows the
	// failing check-run names scrolling at ~5 runes/sec via wall-clock millis
	// when they overflow the panel width. Always uses the ✗ glyph (matches
	// chipCI's failure semantics). Suppressed when no failing checks reported
	// for this project.
	if len(p.FailingChecks) > 0 {
		// renderDomainRow consumes: len(prefix-visual) + 1 (glyph) + 1 (space).
		// metadataPrefix is always 6 visual columns (urgent ▌+5sp / breath
		// ▌+5sp / 6sp). Inner body budget = width - 8.
		bodyWidth := width - 8
		if bodyWidth < 1 {
			bodyWidth = 1
		}
		nowMs := scrollNowMs()
		renderDomainRow(buf, prefix, "✗", func(inner *bytes.Buffer) {
			renderFailingChecksRow(inner, p.FailingChecks, bodyWidth, nowMs)
		})
	}

	// Runtime domain row: shell-cmd | ports
	renderDomainRow(buf, prefix, "▶", func(inner *bytes.Buffer) {
		var subShell, subPorts bytes.Buffer

		// Sub-group 1: shell command
		chipShellCmd(&subShell, p.ShellCmd)

		// Sub-group 2: ports
		chipPorts(&subPorts, p.ListeningPorts)

		joinNonEmpty(inner, []*bytes.Buffer{&subShell, &subPorts}, domainSep)
	})

	// Agent domain row: wait-age only.
	// Agent chips (chipAgentClaude / chipAgentPi) are suppressed for the
	// current session — the user is present, so the unattended-agent indicator
	// is misleading. chipWaitAge retains its full 3-tier behavior including the
	// RedPulse "! " prefix for cross-threshold urgency.
	renderDomainRow(buf, prefix, "✻", func(inner *bytes.Buffer) {
		chipWaitAge(inner, p.WaitStartedTS, now)
	})
}

// renderCompactRow composes the SINGLE-line non-current layout:
//
//	[prefix] + marker-glyph + " " + name (truncated) + chipInlineAlerts + (wait-age if waiting) + ClearLineEnd + LF
//
// Prefix dispatch (260511-ohu change A: urgent non-current now reaches here):
//
//	urgent=true  → {RedBorder}▌{Reset}" " (urgent accent preserved on single compact line)
//	otherwise    → "  " (2-space indent)
//
// No branch, ports, shell-cmd, agent chips, or celebrate chip — those are
// scanning noise on a non-current row. Only attention-worthy signals surface.
// Per planner decision PD-02: name soft-cap at max(width-14, 10) runes.
func renderCompactRow(buf *bytes.Buffer, p *proto.Project, width int, animator *Animator, nowFn func() int64, urgent bool) {
	if urgent {
		buf.WriteString(RedBorder)
		buf.WriteString("▌")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	} else {
		buf.WriteString("  ")
	}

	// Marker (reuse MarkerFor with stale-dim override, same as renderProjectRow VIS-12).
	pForMarker := *p
	glyph, color := MarkerFor(pForMarker, animator)
	if p.Status == "alive" && p.LastActivityTS > 0 && nowFn()-p.LastActivityTS >= int64(StaleThresholdSec) {
		color = Dim
	}
	buf.WriteString(color)
	buf.WriteString(glyph)
	buf.WriteString(Reset)
	buf.WriteString(" ")

	// Name (truncated to width budget).
	nameCap := width - 14
	if nameCap < 10 {
		nameCap = 10
	}
	buf.WriteString(truncateRunes(p.Name, nameCap))

	// Inline alerts: PR/CI fail, PR pend, dirty count.
	chipInlineAlerts(buf, p)

	// Wait-age (compact form: no "! " prefix; chipWaitAge with "! " is for
	// current-session agent domain row only — compact rows use the tiered inline form).
	if p.Status == "waiting" && p.WaitStartedTS > 0 {
		now := nowFn()
		age := now - p.WaitStartedTS
		buf.WriteString(" ")
		if age >= int64(WaitUrgentSec) {
			buf.WriteString(Orange)
		} else if age >= int64(WaitWarnSec) {
			buf.WriteString(Orange)
		} else {
			buf.WriteString(Dim)
		}
		buf.WriteString(formatAge(age))
		buf.WriteString(Reset)
	}

	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// spaceIf writes a space to buf if buf is non-empty AND the last byte
// is not already a space. The chip composers conditionally write data;
// this helper inserts inter-chip separators only between non-empty chips.
func spaceIf(buf *bytes.Buffer) {
	if buf.Len() == 0 {
		return
	}
	last := buf.Bytes()[buf.Len()-1]
	if last == ' ' {
		return
	}
	buf.WriteByte(' ')
}
