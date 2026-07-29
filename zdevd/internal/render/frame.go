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
//  2. Mood divider: "  ─────────────────" (17 U+2500, fleet-mood color)
//     + ClearLineEnd + LF
//  3. For each project: rows (marker + metadata for current session;
//     click-row math invariant per Pitfall H).
//  4. Footer tally (one row, possibly blank — see FooterMode)
//  5. ClearToEnd
//
// Source-of-truth: ~/.local/bin/zdev-sidebar-render lines 622-661.
func Render(snap *proto.Snapshot, width int, animator *Animator, nowFn func() int64) []byte {
	var buf bytes.Buffer
	buf.WriteString(CursorHome)

	// Mood divider: the frame's first row. The "  zdev projects" header
	// text was dropped (dogfood: it added nothing — the pane border
	// already names the pane); the divider carries the fleet mood as its
	// COLOR (grey idle / green active / orange waiting / red urgent),
	// preserving the at-a-glance signal in zero extra rows.
	buf.WriteString("  ")
	buf.WriteString(MoodFor(snap, nowFn))
	buf.WriteString(strings.Repeat("─", 17))
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')

	// Triage section (phase4-v9): pinned ranked attention strip between
	// the divider and the stable project list. Renders ZERO rows when the
	// queue is empty, so the pre-triage row math (project section starts
	// at click-row 2, after the mood divider) is unchanged for quiet
	// sidebars; when non-empty it
	// adds min(len(Triage), triageSectionMax) entry rows plus one closing
	// divider row, shifting the project section down by exactly that.
	// The main list is never reordered — spatial memory of row positions
	// is preserved; the section is the ranking surface.
	renderTriageSection(&buf, snap, width, animator, nowFn)

	// Review gauge (phase4-v21, roadmap NOW#4): the S3 landing-readiness
	// gauge — the permanent occupant of the slot the demoted triage strip
	// vacated. Like the strip it renders ZERO rows when disabled
	// (ZDEV_SIDEBAR_REVIEW unset, the default) or when the gauge is empty,
	// so quiet/gauge-off sidebars are byte-identical to the pre-gauge layout;
	// when populated it adds up to reviewGaugeSectionMax repo rows plus one
	// closing divider, shifting the project section down by exactly that. The
	// main list is never reordered.
	renderReviewGauge(&buf, snap, width)

	// Team badge lookup (phase4-v16, slice 4): lead project row → its
	// team. Rendered inline on the lead's row so click-row math is
	// untouched; multiple teams led from one row concatenate.
	teamsByLead := make(map[string][]*proto.TeamGroup, len(snap.TeamGroups))
	for i := range snap.TeamGroups {
		g := &snap.TeamGroups[i]
		if g.LeadProject != "" {
			teamsByLead[g.LeadProject] = append(teamsByLead[g.LeadProject], g)
		}
	}

	// Cursor row-order contract (slice C): CursorRow indexes the FLATTENED
	// row list (proto.FlatRows) — projects plus, when TeamRows is on, their
	// nested member rows. projBase[i] is the flattened index of project i's
	// own row; its member rows occupy projBase[i]+1 .. projBase[i]+memberCount.
	// This accumulation MUST match proto.FlatRows' ordering exactly (project
	// then its teamsByLead members) so the hub cursor and this ▶ never drift.
	// Knob off ⇒ projBase[i] == i ⇒ behaviour identical to pre-slice-C.
	teamRows := teamRowsFor(snap)
	projBase := make([]int, len(snap.Projects))
	acc := 0
	for i := range snap.Projects {
		projBase[i] = acc
		acc++
		if teamRows {
			for _, g := range teamsByLead[snap.Projects[i].Name] {
				acc += len(g.Members)
			}
		}
	}
	cursorFlatRow := -1
	if snap.CursorActive {
		cursorFlatRow = snap.CursorRow
	}

	// Per-project rows.
	//
	// renderProject writes one project's row(s) and tallies it into the
	// footer buckets. Used by both the main loop (dim/off modes) and the
	// active/demoted sub-loops (fold mode).
	var nWait, nDead, nRun, nDone, nAlive, nAbsent int
	renderProject := func(i int) {
		p := snap.Projects[i]
		// Absent is a session-existence flag, not an Attention value;
		// detect via Status here since Attention has no "absent" case.
		if p.Status == "absent" {
			nAbsent++
		} else {
			switch projectAttention(&p) {
			case proto.AttWaiting:
				nWait++
			case proto.AttDead:
				// Counted separately (dogfood #4 redesign): "1 dead"
				// carries a different demand — relaunch — than a wait.
				nDead++
			case proto.AttWorking:
				nRun++
			case proto.AttFinished:
				nDone++
			default:
				nAlive++
			}
		}
		isCurrent := p.Name == snap.CurrentSession && snap.CurrentSession != ""
		urgent := isUrgent(&p, nowFn())
		isCursor := cursorFlatRow == projBase[i] && !isCurrent
		// 260511-ohu change A: twoRows := isCurrent only (urgent dropped).
		// Non-current urgent projects now render as 1 compact row with the red ▌
		// prefix migrated into renderCompactRow.
		if isCurrent {
			renderProjectRow(&buf, &p, snap.CurrentSession, animator, nowFn, urgent, teamsByLead[p.Name], teamRows)
			renderMetadataRow(&buf, &p, snap.CurrentSession, width, animator, nowFn, urgent)
		} else {
			renderCompactRow(&buf, &p, width, animator, nowFn, urgent, isCursor, teamsByLead[p.Name], teamRows)
		}
		// Nested member rows (Agent Teams slice B, ZDEV_TEAM_WINDOWS): each
		// teammate of a team led by this project renders on its own indented
		// row immediately after the lead's row(s). Knob off → no rows (the
		// bullets on the badge are the surface). Placed here so it runs under
		// both the fold and the flat layouts.
		if teamRows {
			renderMemberRows(&buf, teamsByLead[p.Name], width, projBase[i], cursorFlatRow)
		}
	}

	// Group headers (ZDEV_SIDEBAR_GROUP=prefix): a dim "─ name ──" line
	// before each run of projects sharing a first path segment. The key is
	// DERIVED from the name — initiative membership under the worktree
	// layout (initiatives/<name>/<repo>) is whatever directories exist, so
	// there is no registry to consult or rot. Projects are already sorted
	// lexicographically, so same-prefix runs are contiguous and header
	// emission is a single scan; row ORDER is untouched (spatial memory
	// contract). Headers are renderer-only visual lines — never navigation
	// rows — so proto.FlatRows, the hub cursor, and the wire are unaffected,
	// same as the fold divider and the daemon-health row.
	// groupKeys[i] is project i's effective group. One refinement over the
	// raw first-segment key: an initiative ROOT row — a bare name whose
	// slash-form members immediately follow it in sort order ("ai-at-pay"
	// before "ai-at-pay/pay-app") — adopts its own name as key, so the home
	// row renders UNDER its header instead of orphaned above it.
	var groupKeys []string
	if GroupMode == "prefix" {
		groupKeys = make([]string, len(snap.Projects))
		for i := range snap.Projects {
			k := groupKey(snap.Projects[i].Name)
			if k == "" && i+1 < len(snap.Projects) &&
				groupKey(snap.Projects[i+1].Name) == snap.Projects[i].Name {
				k = snap.Projects[i].Name
			}
			groupKeys[i] = k
		}
	}
	prevGroup := ""
	renderGrouped := func(i int) {
		if GroupMode == "prefix" {
			if g := groupKeys[i]; g != prevGroup {
				writeGroupHeader(&buf, g)
				prevGroup = g
			}
		}
		renderProject(i)
	}

	if DemoteMode == "fold" {
		// Fold mode: separate active and demoted projects. Active projects
		// render in their original positions (spatial memory preserved for
		// the active block). Demoted (idle > DemoteThresholdSec) projects
		// sink below a dim divider at the bottom of the list.
		// Demoted rows still receive stale-dim treatment (renderCompactRow /
		// renderProjectRow apply isStaleRow normally — only "off" disables it).
		var activeIdx, demotedIdx []int
		now := nowFn()
		for i := range snap.Projects {
			if isDemotedRow(&snap.Projects[i], now) {
				demotedIdx = append(demotedIdx, i)
			} else {
				activeIdx = append(activeIdx, i)
			}
		}
		for _, i := range activeIdx {
			renderGrouped(i)
		}
		if len(demotedIdx) > 0 {
			// Dim demote divider: same glyph family as the mood divider but
			// Dim-colored to signal "below the fold". One row always.
			buf.WriteString("  ")
			buf.WriteString(Dim)
			buf.WriteString(strings.Repeat("─", 17))
			buf.WriteString(Reset)
			buf.WriteString(ClearLineEnd)
			buf.WriteByte('\n')
			// Group tracking restarts below the fold: a group straddling
			// the divider re-states its header, so a demoted row is never
			// orphaned under a header that scrolled away with the actives.
			prevGroup = ""
			for _, i := range demotedIdx {
				renderGrouped(i)
			}
		}
	} else {
		for i := range snap.Projects {
			renderGrouped(i)
		}
	}

	// Daemon self-health row (zd-6e1): appears between the project list and
	// the footer ONLY when health thresholds are breached. Never on a healthy
	// fleet, so project row positions are unchanged in the common case.
	if daemonIsDegraded(snap, nowFn) {
		renderDaemonHealthRow(&buf, snap, nowFn)
	}

	renderFooter(&buf, nWait, nDead, nRun, nDone, nAlive, nAbsent)

	buf.WriteString(ClearToEnd)
	return buf.Bytes()
}

// FooterMode selects the footer tally style (dogfood #4: the glyph
// tally was unmemorable noise — "I don't remember what the glyphs
// signify"). cmd/zdev-sidebar sets this from ZDEV_SIDEBAR_FOOTER:
//
//	full    — worded counts of NON-ZERO decision-relevant buckets only
//	          ("2 waiting · 1 dead · 3 working · 1 done"); quiet fleets
//	          render a blank footer row. The default.
//	compact — the legacy glyph tally (N● N◎ N◆ N· N·), always present.
//	off     — blank footer row always.
//
// Every mode emits exactly ONE row (possibly empty) so the frame's
// line count — which tests and click-row math rely on — is invariant
// across modes and fleet states; an empty last row is visually
// indistinguishable from no row.
var FooterMode = "full"

// DemoteMode selects the inactive-session demotion style.
// cmd/zdev-sidebar sets this from ZDEV_SIDEBAR_DEMOTE:
//
//	dim  — stale rows dim in place (default; current VIS-12 behavior).
//	fold — stale sessions sink below a dim ─── divider at the bottom;
//	       the active block is never reordered (spatial memory preserved).
//	off  — no special treatment for inactive sessions (no dim, no fold).
//
// Kill criterion: if fold hides a session the operator forgets,
// default stays dim. fold requires explicit opt-in.
//
// Frame line-count math for fold mode (when N_demoted > 0):
//
//	rows = 1 (mood divider)
//	     + T (triage rows, 0 when quiet)
//	     + A (active project rows)
//	     + 1 (demote divider)    ← only present when N_demoted > 0
//	     + D (demoted project rows)
//	     + 1 (footer)
//
// Click-row offsets: active projects index from row 3+T as usual;
// demoted projects index from row 3+T+A+1 (past the demote divider).
var DemoteMode = "dim"

// DemoteThresholdSec is the inactivity duration (seconds) after which a
// project is eligible for fold/dim demotion. Defaults to StaleThresholdSec.
// cmd/zdev-sidebar overrides via ZDEV_SIDEBAR_DEMOTE_THRESHOLD.
var DemoteThresholdSec = DemoteThresholdSecDefault

// GroupMode selects sidebar grouping. cmd/zdev-sidebar sets this from
// ZDEV_SIDEBAR_GROUP:
//
//	off    — flat list (default; byte-identical to pre-knob output).
//	prefix — a dim "─ name ──" header line precedes each contiguous run
//	         of projects sharing a first path segment ("pay/pay-app" →
//	         "pay"; "ai-at-pay/pay-app" → "ai-at-pay"); unprefixed names
//	         group under "" and get a bare "──────" separator when they
//	         follow a named group, so a trailing "zdev" is never visually
//	         orphaned under someone else's header.
//
// This is the initiative-grouping surface for the workspace layout
// ($ZDEV_WORKSPACE/projects/<repo> canonical checkouts;
// $ZDEV_WORKSPACE/initiatives/<name>/<repo> full clones per initiative):
// the path IS the membership — under the initiatives container the key is
// the initiative name (see groupKey) — so membership derives from the
// directories that exist and scope drift needs no config edit.
//
// Kill criterion: if a week of dogfood shows the headers never change
// which row the eye lands on (the pay/ prefix cluster already groups
// rows spatially), the headers are decoration — default stays off, and
// the knob is removed rather than nursed.
var GroupMode = "off"

// initiativesContainer is the workspace directory that holds initiative
// directories (ZDEV layout: $ZDEV_WORKSPACE/initiatives/<name>/<repo>).
// Projects under it group by the INITIATIVE name, not the container — a
// single "initiatives" group would erase exactly the distinction the
// grouping exists to draw.
const initiativesContainer = "initiatives"

// groupKey returns the grouping key for a project name under
// GroupMode=prefix: the first path segment for slash-form names, "" for
// bare names — except under the initiatives container, where the key is
// the second segment (the initiative), and the initiative home row
// ("initiatives/<name>") keys as its own name so it groups with its
// members. Pure.
func groupKey(name string) string {
	i := strings.IndexByte(name, '/')
	if i <= 0 {
		return ""
	}
	if name[:i] != initiativesContainer {
		return name[:i]
	}
	rest := name[i+1:]
	if j := strings.IndexByte(rest, '/'); j > 0 {
		return rest[:j]
	}
	return rest
}

// writeGroupHeader emits the one-line group header: "  ─ name ──" with the
// name Bold against Dim rules, or a bare dim "  ──────" separator for the
// unprefixed group (name == ""). One row always, same glyph family as the
// mood/demote dividers.
func writeGroupHeader(buf *bytes.Buffer, name string) {
	buf.WriteString("  ")
	buf.WriteString(Dim)
	if name == "" {
		buf.WriteString(strings.Repeat("─", 6))
	} else {
		buf.WriteString("─ ")
		buf.WriteString(Reset)
		buf.WriteString(Bold)
		buf.WriteString(name)
		buf.WriteString(Reset)
		buf.WriteString(Dim)
		buf.WriteString(" ──")
	}
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// TeamRows selects how Agent Teams members render (Agent Teams slice B).
// cmd/zdev-sidebar sets it from ZDEV_TEAM_WINDOWS=1 — the same knob that
// drives the daemon's team-sweep (each tmux teammate lives in its own
// window) and lead de-aggregation, so the three move together:
//
//	false (default) — members render as colored bullets on the lead's
//	                  ⊛badge, exactly as phase4-v16..v18 did. Zero byte
//	                  change for non-team fleets and for teams when the
//	                  knob is off.
//	true            — the lead row keeps the ⊛<name> badge as the team
//	                  marker (no bullets), and each member renders as its
//	                  own 4-space-indented row beneath the lead with a
//	                  status glyph in the project-row language.
//
// Kill criterion: if nested rows cost more vertical space than they earn
// in legibility at fleet scale, default stays bullets.
var TeamRows = false

// teamRowsFor resolves the effective member-row mode for one snapshot: the
// DAEMON's flag on the wire wins (it is the row-order authority — CursorRow
// indexes the daemon's flattened list), with the package var as fallback
// only for snapshots from a pre-v20 source (impossible in practice given
// strict schema equality, but keeps goldens/tests that construct bare
// snapshots meaningful). Invariants review (slice C, F1): renderer env must
// never disagree with the daemon about row order.
func teamRowsFor(snap *proto.Snapshot) bool { return snap.TeamRows || TeamRows }

// renderFooter writes the footer tally per FooterMode. Counts use the
// bucket's own marker color (waiting orange, dead red, working icy,
// done yellow) so the words tie back to the rows; separators are dim.
func renderFooter(buf *bytes.Buffer, nWait, nDead, nRun, nDone, nAlive, nAbsent int) {
	switch FooterMode {
	case "off":
		// "off" mode keeps the row blank by contract.
		buf.WriteString(ClearLineEnd)
		buf.WriteByte('\n')
		return
	case "compact":
		buf.WriteString("  ")
		buf.WriteString(Dim)
		// Dead folds into the waiting slot here — the compact form is
		// the legacy 5-bucket shape, kept stable for muscle memory.
		fmt.Fprintf(buf, "%d● %d◎ %d◆ %d· %d·", nWait+nDead, nRun, nDone, nAlive, nAbsent)
		buf.WriteString(Reset)
		buf.WriteString(ClearLineEnd)
		buf.WriteByte('\n')
		return
	}
	// full: words, non-zero buckets only, decision-relevant first.
	type bucket struct {
		n     int
		word  string
		color string
	}
	buckets := []bucket{
		{nDead, "dead", RedPulse},
		{nWait, "waiting", Orange},
		{nRun, "working", Icy},
		{nDone, "done", Yellow},
	}
	wrote := false
	for _, b := range buckets {
		if b.n == 0 {
			continue
		}
		if !wrote {
			buf.WriteString("  ")
		} else {
			buf.WriteString(Dim)
			buf.WriteString(" · ")
			buf.WriteString(Reset)
		}
		buf.WriteString(b.color)
		fmt.Fprintf(buf, "%d %s", b.n, b.word)
		buf.WriteString(Reset)
		wrote = true
	}
	// All-zero (idle/absent only) emits the blank row: a quiet fleet
	// LOOKS quiet instead of enumerating its quietness, and the frame
	// keeps its invariant line count.
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
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
func renderProjectRow(buf *bytes.Buffer, p *proto.Project, current string, animator *Animator, nowFn func() int64, urgent bool, teamGroups []*proto.TeamGroup, teamRows bool) {
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
	if isCurrent && projectAttention(&pForMarker) == proto.AttWaiting {
		// Suppress the attention-drawing pulse when the user is present —
		// same rationale as zeroing agentClaude/agentPi in renderMetadataRow.
		pForMarker.Attention = proto.AttIdle
		pForMarker.Status = "alive"
	}
	glyph, color := MarkerFor(pForMarker, animator, nowFn())
	// VIS-12 stale dim-out: idle + age >= StaleThreshold => Dim.
	// Skipped in "off" mode where no special treatment applies.
	// Unmanaged rows are always dim (no projects-file entry).
	if (DemoteMode != "off" && isStaleRow(p, nowFn())) || p.Unmanaged {
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
	// Agent Teams badge (phase4-v16, slice 4) — same placement as the
	// compact row: after the name, before line-clear.
	chipTeamBadge(buf, teamGroups, teamRows)
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
	// Per-agent chips were dropped in the 260511-ohu domain-row refactor —
	// agent state surfaces through MarkerFor's attention-driven glyph
	// instead. chipWaitAge retains its full 3-tier behavior including the
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
//	cursor=true  → "▶ " (cursor selection indicator; zd-e6e — replaces the 2-space indent)
//	otherwise    → "  " (2-space indent)
//
// No branch, ports, shell-cmd, agent chips, or celebrate chip — those are
// scanning noise on a non-current row. Only attention-worthy signals surface.
// Per planner decision PD-02: name soft-cap at max(width-14, 10) runes.
func renderCompactRow(buf *bytes.Buffer, p *proto.Project, width int, animator *Animator, nowFn func() int64, urgent bool, isCursor bool, teamGroups []*proto.TeamGroup, teamRows bool) {
	switch {
	case urgent:
		buf.WriteString(RedBorder)
		buf.WriteString("▌")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	case isCursor:
		buf.WriteString("▶ ")
	default:
		buf.WriteString("  ")
	}

	// Marker (reuse MarkerFor with stale-dim override, same as renderProjectRow VIS-12).
	// Stale-dim skipped in "off" mode.
	pForMarker := *p
	glyph, color := MarkerFor(pForMarker, animator, nowFn())
	stale := DemoteMode != "off" && isStaleRow(p, nowFn())
	if stale || p.Unmanaged {
		color = Dim
	}
	buf.WriteString(color)
	buf.WriteString(glyph)
	buf.WriteString(Reset)
	buf.WriteString(" ")

	// Name (truncated to width budget). Stale, absent, and unmanaged rows
	// recede whole-row. Unmanaged sessions are dim by design (they're not
	// tracked projects — just adopted polecat/scratch sessions). The name
	// carries the dimming where the eye actually rests.
	nameCap := width - 14
	if nameCap < 10 {
		nameCap = 10
	}
	if stale || p.Status == "absent" || p.Unmanaged {
		buf.WriteString(Dim)
		buf.WriteString(truncateRunes(p.Name, nameCap))
		buf.WriteString(Reset)
	} else {
		buf.WriteString(truncateRunes(p.Name, nameCap))
	}

	// Inline alerts: PR/CI fail, PR pend, dirty count.
	chipInlineAlerts(buf, p)

	// Agent Teams badge (phase4-v16, slice 4): the lead's row carries
	// "⊛name" + a colored bullet per teammate — for in-process teams
	// this is the members' ONLY surface.
	chipTeamBadge(buf, teamGroups, teamRows)

	// Wait-age (compact form: no "! " prefix; chipWaitAge with "! " is for
	// current-session agent domain row only — compact rows use the tiered inline form).
	if projectAttention(p) == proto.AttWaiting && p.WaitStartedTS > 0 {
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

// renderMemberRows writes one indented row per teammate of every team led by
// the just-rendered project (Agent Teams slice B, ZDEV_TEAM_WINDOWS). Layout:
//
//	indent + glyph-color + glyph + Reset + " " + name-color + name + Reset
//
// The indent is 4 columns: "    " normally, "  ▶ " when this member row is the
// cursor-selected row (slice C) — the ▶ sits at column 2 so it reads as nested
// under the lead while the glyph column stays aligned with the unselected
// rows. The status glyph is the project-row language (memberGlyph); the name
// renders in its team color (no color for unknown colors), truncated to the
// width budget.
//
// baseIdx is the flattened row index of the LEAD's project row (projBase[i]);
// the first member therefore sits at baseIdx+1. cursorFlatRow is the selected
// flattened row index, or -1 when the cursor is inactive. The accumulation
// across groups/members MUST match proto.FlatRows so a selected member here is
// exactly the row the hub's cursor resolved. No rows when groups is empty.
func renderMemberRows(buf *bytes.Buffer, groups []*proto.TeamGroup, width, baseIdx, cursorFlatRow int) {
	// Name budget: width minus the 4-col indent, the glyph, and a space.
	nameCap := width - 6
	if nameCap < 10 {
		nameCap = 10
	}
	flat := baseIdx
	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, m := range g.Members {
			flat++ // member rows occupy baseIdx+1, baseIdx+2, …
			glyph, color := memberGlyph(m.Status)
			if flat == cursorFlatRow {
				buf.WriteString("  ▶ ")
			} else {
				buf.WriteString("    ")
			}
			buf.WriteString(color)
			buf.WriteString(glyph)
			buf.WriteString(Reset)
			buf.WriteString(" ")
			nameColor, ok := teamMemberColors[m.Color]
			if ok {
				buf.WriteString(nameColor)
			}
			buf.WriteString(truncateRunes(m.Name, nameCap))
			if ok {
				buf.WriteString(Reset)
			}
			buf.WriteString(ClearLineEnd)
			buf.WriteByte('\n')
		}
	}
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
