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
	frame, _ := RenderWithOpts(snap, width, animator, nowFn, RenderOpts{})
	return frame
}

// RenderOpts carries optional per-frame render behavior that must NOT
// perturb Render/RenderWithRows' pinned byte-for-byte contract (goldens
// exercise those two entry points at the zero value and must never move).
// Zero value is exactly today's behavior.
type RenderOpts struct {
	// Hover names the project whose row currently sits under the mouse
	// pointer (ZDEV_SIDEBAR_HOVER, tea engine only — classic never sets
	// this). Empty when no row is hovered, which is the default and the
	// only value classic or a hover-off tea sidebar ever passes.
	Hover string
}

// RowRef maps ONE rendered screen line to the switch target it displays.
//
// Y is the 0-based line index within the pane — directly comparable to
// tmux's #{mouse_y}, which is what makes click-to-switch a lookup instead
// of a second implementation of the frame's geometry. Name is the canonical
// slash-form project to switch to; WindowID is set only for Agent Teams
// member rows (select-window after the session switch), mirroring the
// cursor reply's contract.
//
// Only NAVIGABLE lines get an entry. A current project's metadata rows map
// to their project (they read as one row on screen), so clicking a branch
// or CI chip lands where the eye expects. Synthetic drawer headers map to
// their group's first member (the M-p header contract — for a folded
// drawer the header is the group's only line). Dividers and the footer
// are absent: a click there is a no-op rather than a guess. Rows folded
// out of the frame own no line and so no entry beyond their header.
//
// The renderer owns this map because it is the only component that knows
// where a line actually landed: the triage strip, the review gauge, group
// headers, the demote divider, and a current project's variable metadata
// rows all shift the geometry. Deriving it anywhere else would be a second
// source of truth that drifts the moment a section changes.
type RowRef struct {
	Y        int
	Name     string
	WindowID string
}

// RenderWithRows composes the frame and, alongside it, the screen-line →
// switch-target map (see RowRef). Callers that only paint use Render.
func RenderWithRows(snap *proto.Snapshot, width int, animator *Animator, nowFn func() int64) ([]byte, []RowRef) {
	return RenderWithOpts(snap, width, animator, nowFn, RenderOpts{})
}

// RenderWithOpts is RenderWithRows plus optional per-frame behavior that
// must never move the goldens (see RenderOpts). RenderWithRows and Render
// are zero-opt wrappers around this — there is exactly one frame composer,
// same invariant Body's package doc already relies on.
func RenderWithOpts(snap *proto.Snapshot, width int, animator *Animator, nowFn func() int64, opts RenderOpts) ([]byte, []RowRef) {
	var buf bytes.Buffer
	var rows []RowRef
	// lineOf reports the 0-based index of the line the NEXT write lands on:
	// every emitted line ends in a newline, so the count of newlines so far
	// IS that index. CursorHome and the SGR escapes carry none.
	lineOf := func() int { return bytes.Count(buf.Bytes(), []byte("\n")) }
	// claim maps every line from y0 up to (but excluding) the current line
	// to one target — a project plus its metadata rows in a single call.
	claim := func(y0 int, name, windowID string) {
		for y := y0; y < lineOf(); y++ {
			rows = append(rows, RowRef{Y: y, Name: name, WindowID: windowID})
		}
	}
	buf.WriteString(CursorHome)

	// Anchor row + holding counter (phase 3C, ZDEV_SIDEBAR_FOCUS,
	// docs/design/command-centre.md): the frame's FIRST line(s), above the
	// mood divider — cheap re-entry after any micro-distraction ("what was
	// I doing?"). Writes nothing when the knob is off or nothing is
	// anchored, so an unanchored/knob-off sidebar stays byte-identical to
	// today; the goldens (which carry no Anchor) prove it.
	if ref, ok := renderAnchorRow(&buf, snap, width, nowFn, lineOf); ok {
		rows = append(rows, ref)
	}

	// Mood divider: the frame's first row absent an anchor. The "  zdev projects" header
	// text was dropped (dogfood: it added nothing — the pane border
	// already names the pane); the divider carries the fleet mood as its
	// COLOR (grey idle / green active / orange waiting / red urgent),
	// preserving the at-a-glance signal in zero extra rows.
	//
	// Damped mode (phase 3C) deliberately does NOT dim this line: it is one
	// quiet row, and losing the fleet-mood signal entirely while anchored
	// would blind the operator to a building fire behind the airlock — the
	// one ambient read the design keeps ON during the loop, everything else
	// recedes around it.
	buf.WriteString("  ")
	buf.WriteString(thDivider(MoodFor(snap, nowFn), 8))
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
		// Collapsed rows (phase4-v22) own no flat row — skip them here
		// EXACTLY as proto.FlatRows does, or the cursor and the ▶ drift.
		if snap.Projects[i].Collapsed {
			continue
		}
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

	// Group metadata (ZDEV_SIDEBAR_GROUP=prefix), computed before the row
	// closures so renderProject can route home rows to the header renderer.
	// groupKeys[i] is project i's effective group; isHome[i] marks the row
	// that IS its group's header — a MARKED group's own directory row.
	// Home-ness is structural (proto.HomeSet over the snapshot's names), so
	// the hub, this renderer, and the switcher derive the identical set and
	// nothing rides the wire.
	var groupKeys []string
	var streamKeys []string
	var isStreamHome []bool
	var isHome []bool
	hasHome := map[string]bool{}       // group key → has a home row (marked group)
	collapsedN := map[string]int{}     // group key → hidden member count
	visibleMembers := map[string]int{} // group key → shown (non-home) member count
	// homes is computed whenever EITHER GroupMode=="prefix" needs it for
	// group-header dispatch OR InitiativeEnabled needs it to detect a
	// current-session home row even when GroupMode is "off" (initiative
	// metadata is deliberately independent of the visual grouping knob —
	// the brief's "operator is AT the initiative level" case is exactly
	// today's ungrouped isCurrent row on a bare initiative dir). nil in the
	// common case (both knobs off), which is safe: a nil map read always
	// returns false.
	var homes map[string]bool
	if GroupMode == "prefix" || InitiativeEnabled {
		names := make([]string, len(snap.Projects))
		for i := range snap.Projects {
			names[i] = snap.Projects[i].Name
		}
		homes = proto.HomeSet(names)
	}
	if GroupMode == "prefix" {
		names := make([]string, len(snap.Projects))
		for i := range snap.Projects {
			names[i] = snap.Projects[i].Name
		}
		streamHomes := proto.StreamHomeSet(names)
		groupKeys = make([]string, len(snap.Projects))
		streamKeys = make([]string, len(snap.Projects))
		isStreamHome = make([]bool, len(snap.Projects))
		isHome = make([]bool, len(snap.Projects))
		for i := range snap.Projects {
			p := &snap.Projects[i]
			groupKeys[i] = proto.EffectiveGroupKey(p.Name, homes)
			// Stream membership is structural, like home-ness: the middle
			// segment of a 3-segment name (workstreams, 2026-08-17), or —
			// for the stream's own folder row (STREAM HOME, 2026-08-18) —
			// its trailing segment. The daemon's RowSort clusters a
			// group's streams after its floor with each home heading its
			// members, so each stream is one contiguous run here.
			streamKeys[i] = proto.StreamKey(p.Name)
			if streamKeys[i] == "" && streamHomes[p.Name] {
				isStreamHome[i] = true
				streamKeys[i] = p.Name[strings.IndexByte(p.Name, '/')+1:]
			}
			isHome[i] = homes[p.Name]
			if isHome[i] {
				hasHome[groupKeys[i]] = true
			}
			if p.Collapsed {
				collapsedN[groupKeys[i]]++
			} else if !isHome[i] && groupKeys[i] != "" {
				visibleMembers[groupKeys[i]]++
			}
		}
	}
	// No damped mode. Phase 3C shipped one — while anchored, every row but
	// the anchor's project and the fires lost its hue and its motion — and
	// it was torn out on 2026-08-06. It inverted the sidebar's whole job.
	// A fleet supervisor exists to DRAW the operator to what is live; the
	// damping muffled exactly that signal, and the failure was self-masking
	// because the anchor's own project pierced it, so the one row the
	// operator checked always looked right while the rest of the fleet was
	// libelled as quiet. Reported as "pip is currently working, but showing
	// a stalled spinner": a frozen spinner frame reads as a hung process,
	// because a quarter-circle only ever appears mid-rotation. Focus is
	// something you build by making the relevant row LOUDER (the anchor row
	// and the holding counter do that), never by making the rest dimmer.
	//
	// lastInGroup[i]: row i is its group's last VISIBLE member — its gutter
	// closes the frame (╰). Computed on snapshot order; the opt-in fold
	// mode re-partitions rendering and may place the closer mid-block,
	// which is accepted (fold already re-states headers loosely).
	var lastInGroup []bool
	if GroupMode == "prefix" {
		lastInGroup = make([]bool, len(snap.Projects))
		for i := range snap.Projects {
			if snap.Projects[i].Collapsed || groupKeys[i] == "" || isHome[i] {
				continue
			}
			last := true
			for j := i + 1; j < len(snap.Projects); j++ {
				if snap.Projects[j].Collapsed {
					continue
				}
				last = groupKeys[j] != groupKeys[i]
				break
			}
			lastInGroup[i] = last
		}
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
		// Collapsed rows are tallied (the footer reports fleet truth) but
		// never drawn — their group's home row carries the rollup.
		if p.Collapsed {
			return
		}
		rowY := lineOf()
		isCurrent := p.Name == snap.CurrentSession && snap.CurrentSession != ""
		urgent := isUrgent(&p, nowFn())
		isCursor := cursorFlatRow == projBase[i] && !isCurrent
		// Hover highlight (ZDEV_SIDEBAR_HOVER, tea engine only): opts.Hover
		// is always "" outside a hover-enabled tea sidebar, so this is a
		// no-op everywhere else. A stale name (hovering something no longer
		// in the snapshot) simply never matches any p.Name — no special
		// case needed.
		hovered := opts.Hover != "" && p.Name == opts.Hover
		grouped := GroupMode == "prefix" && groupKeys[i] != ""
		home := grouped && isHome[i]
		// 260511-ohu change A: twoRows := isCurrent only (urgent dropped).
		// Non-current urgent projects now render as 1 compact row with the red ▌
		// prefix migrated into renderCompactRow.
		switch {
		case home:
			// The home row IS the group header. When it's also the current
			// session, the metadata row still follows — the current-project
			// two-row contract (and projBase math) is glyph-agnostic. The
			// metadata rows hang inside the frame on the group's gutter.
			renderHomeRow(&buf, &p, width, animator, nowFn, isCursor, isCurrent, hovered,
				collapsedN[groupKeys[i]],
				collapsedN[groupKeys[i]] > 0 && visibleMembers[groupKeys[i]] == 0)
			if isCurrent {
				renderMetadataRow(&buf, &p, snap.CurrentSession, width-3, animator, nowFn, urgent,
					groupGutter(groupKeys[i], true, "│",
						rowMargin(&p, animator, true, false, hovered)),
					snap, true)
			}
		case isCurrent && grouped:
			// Current member row: keep the frame — gutter first, then the
			// ▌ marker in the columns compact rows spend on their indent.
			// The frame closer still lands here when this row is last; its
			// metadata rows then hang on blanks below the closed corner.
			// Stream members stay on the same rail, indented two cells
			// deeper — their metadata rows indent with them.
			g := "│"
			mg := "│"
			if lastInGroup[i] {
				g, mg = "╰", " "
			}
			if streamKeys[i] != "" && !isStreamHome[i] {
				renderProjectRow(&buf, &p, snap.CurrentSession, animator, nowFn, urgent, hovered, teamsByLead[p.Name], teamRows,
					groupGutter(groupKeys[i], true, g,
						rowMargin(&p, animator, true, false, hovered))+"  ")
				renderMetadataRow(&buf, &p, snap.CurrentSession, width-5, animator, nowFn, urgent,
					groupGutter(groupKeys[i], true, mg,
						rowMargin(&p, animator, true, false, hovered))+"  ",
					snap, false)
				break
			}
			renderProjectRow(&buf, &p, snap.CurrentSession, animator, nowFn, urgent, hovered, teamsByLead[p.Name], teamRows,
				groupGutter(groupKeys[i], true, g,
					rowMargin(&p, animator, true, false, hovered)))
			renderMetadataRow(&buf, &p, snap.CurrentSession, width-3, animator, nowFn, urgent,
				groupGutter(groupKeys[i], true, mg,
					rowMargin(&p, animator, true, false, hovered)),
				snap, false)
		case isCurrent:
			renderProjectRow(&buf, &p, snap.CurrentSession, animator, nowFn, urgent, hovered, teamsByLead[p.Name], teamRows, "")
			renderMetadataRow(&buf, &p, snap.CurrentSession, width, animator, nowFn, urgent, "", snap, homes[p.Name])
		case grouped:
			// Member row: a │ gutter hangs under the group's header —
			// hued (PaletteFor, matching the header name) for initiative
			// groups, Dim for synthetic ones (projects/) — so belonging
			// reads as a frame, not just an indent. Width shrinks with
			// the gutter so truncation still respects the pane edge.
			// The last visible member closes the frame. Stream members sit
			// two cells deeper on the SAME rail, under their subtle label
			// line — structure by whitespace, one hue per group (stream-rows
			// calm pass, 2026-08-17 rev 2, option B).
			g := "│"
			if lastInGroup[i] {
				g = "╰"
			}
			if streamKeys[i] != "" && !isStreamHome[i] {
				renderCompactRow(&buf, &p, width-5, animator, nowFn, urgent, isCursor, hovered, teamsByLead[p.Name], teamRows,
					groupGutter(groupKeys[i], true, g,
						rowMargin(&p, animator, false, isCursor, hovered))+"  ")
				break
			}
			renderCompactRow(&buf, &p, width-3, animator, nowFn, urgent, isCursor, hovered, teamsByLead[p.Name], teamRows,
				groupGutter(groupKeys[i], true, g,
					rowMargin(&p, animator, false, isCursor, hovered)))
		default:
			renderCompactRow(&buf, &p, width, animator, nowFn, urgent, isCursor, hovered, teamsByLead[p.Name], teamRows, "")
		}
		// Every line just written belongs to this project — the compact or
		// project row plus, for the current session, its metadata rows.
		claim(rowY, p.Name, "")
		// Nested member rows (Agent Teams slice B, ZDEV_TEAM_WINDOWS): each
		// teammate of a team led by this project renders on its own indented
		// row immediately after the lead's row(s). Knob off → no rows (the
		// bullets on the badge are the surface). Placed here so it runs under
		// both the fold and the flat layouts.
		if teamRows {
			memberY := lineOf()
			renderMemberRows(&buf, teamsByLead[p.Name], width, projBase[i], cursorFlatRow)
			// renderMemberRows emits exactly one line per member in this
			// order, so walking the same groups assigns each its own line.
			// A member row switches to the LEAD's session, then selects the
			// member's window — the cursor reply's contract.
			for _, g := range teamsByLead[p.Name] {
				if g == nil {
					continue
				}
				for _, m := range g.Members {
					rows = append(rows, RowRef{Y: memberY, Name: p.Name, WindowID: m.WindowID})
					memberY++
				}
			}
		}
	}

	// Group headers (ZDEV_SIDEBAR_GROUP=prefix). A group whose first row is
	// a HOME row needs no synthetic header — renderHomeRow draws that row
	// AS the header. Groups without a home (projects/, legacy prefixes) get
	// the synthetic "─ name ──…" line; the transition into the ungrouped
	// block gets the bare separator so a trailing single never reads as a
	// member of the previous group. Headers are renderer-only visual lines
	// — never navigation rows — so proto.FlatRows, the hub cursor, and the
	// wire are unaffected, same as the fold divider and the daemon-health
	// row. (Row ORDER, by contrast, is the daemon's: proto.GroupSort under
	// the same knob.)
	prevGroup := ""
	prevStream := ""
	// floorVisible tracks whether the CURRENT group has painted at least one
	// visible floor row (a non-home, non-stream member) — the breathing gap
	// only separates the floor block from the streams when there IS a floor
	// block to separate from. Without this, a group whose floor is entirely
	// quiet-and-collapsed (found live 2026-08-19: marketplace with both
	// pay-app and pay-toggles folded, only its working streams piercing) got
	// a stray rail-only line sitting directly under the header with nothing
	// above it — the gap's own purpose, orphaned. RowSort's floor-before-
	// streams guarantee means every floor row for a group is visited before
	// its first stream row, so a simple running flag suffices.
	floorVisible := false
	renderGrouped := func(i int) {
		if GroupMode == "prefix" {
			if g := groupKeys[i]; g != prevGroup {
				// Synthetic header for unmarked groups only; a marked
				// group's home row IS its header, and the transition to
				// ungrouped singles gets nothing at all — alpha order
				// interleaves them and the tree mirrors the disk.
				if g != "" && !isHome[i] {
					headerY := lineOf()
					writeGroupHeader(&buf, g, width, collapsedN[g],
						collapsedN[g] > 0 && visibleMembers[g] == 0)
					// The synthetic header IS clickable: it targets the
					// group's first project, mirroring the M-p header
					// contract. Essential for a FOLDED drawer — the
					// header is then the group's only line, and an inert
					// one made every click on "▸ projects ·N" a silent
					// no-op (live report 2026-08-02). Project i is by
					// construction that first project.
					rows = append(rows, RowRef{Y: headerY, Name: snap.Projects[i].Name})
				}
				prevGroup = g
				prevStream = ""
				floorVisible = false
			}
			// Stream labels (workstreams; calm pass 2026-08-17 rev 2): the
			// first VISIBLE row of each stream gets a subtle label line on
			// the initiative's rail, and the first stream of the group gets
			// a rail-only breathing line above it, IF the floor painted a
			// visible row to separate FROM — whitespace has to have two
			// sides to mean anything. Labels follow the drawer-header
			// contract — renderer-only, never navigation rows, clickable
			// via the stream's first repo; the breathing line is inert like
			// the dividers. Collapsed rows are skipped so a folded
			// initiative emits neither: the home's rollup is its only trace.
			if !snap.Projects[i].Collapsed {
				if streamKeys[i] == "" && !isHome[i] {
					floorVisible = true
				}
				if s := streamKeys[i]; s != prevStream {
					if s != "" {
						if prevStream == "" && floorVisible {
							writeStreamGap(&buf, groupKeys[i])
						}
						// The stream's HOME row (its folder — a real,
						// navigable place since 2026-08-18) opens the run
						// itself; the synthetic label only stands in when
						// no home row is visible (older layouts, or a
						// collapsed home over a pierced member).
						if !isStreamHome[i] {
							headerY := lineOf()
							writeStreamLabel(&buf, groupKeys[i], s)
							rows = append(rows, RowRef{Y: headerY, Name: snap.Projects[i].Name})
						}
					}
					prevStream = s
				}
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
			buf.WriteString(thDim())
			buf.WriteString(strings.Repeat("─", 8))
			buf.WriteString(Reset)
			buf.WriteString(ClearLineEnd)
			buf.WriteByte('\n')
			// Group tracking restarts below the fold: a group straddling
			// the divider re-states its header, so a demoted row is never
			// orphaned under a header that scrolled away with the actives.
			// Stream tracking restarts with it, for the same reason.
			prevGroup = ""
			prevStream = ""
			floorVisible = false
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
	return buf.Bytes(), rows
}

// FooterMode selects the footer tally style (dogfood #4: the glyph
// tally was unmemorable noise — "I don't remember what the glyphs
// signify"). cmd/zdev-sidebar sets this from ZDEV_SIDEBAR_FOOTER:
//
//	full — worded counts of NON-ZERO decision-relevant buckets only
//	       ("2 waiting · 1 dead · 3 working · 1 done"); quiet fleets
//	       render a blank footer row. The default.
//	off  — blank footer row always.
//
// The legacy "compact" glyph tally was deleted (calm lane B,
// 2026-08-18): full had already won the dogfood, and compact's ◎ was
// the only glyph in the whole vocabulary with a single orphan site.
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
// Kill criterion — VERDICT (2026-08-18, calm lane D): survived. Weeks of
// dogfood ran grouped (ZDEV_SIDEBAR_GROUP=collapse); the group frames are
// where the eye lands, the initiative/stream structure lives in them, and
// the flat list is the mode nobody chose. Default flips to "prefix";
// "off" remains the escape hatch.
var GroupMode = "prefix"

// displayName returns the row text for a project name under
// GroupMode=prefix: the portion after the group-key segment — the header
// carries the context the prefix used to, so repeating it on every row is
// pure noise ("initiatives/ai-at-pay/pay-app" renders as "pay-app" under
// the "ai-at-pay" header; "projects/pay-app" as "pay-app" under
// "projects"). Structural parse, mirroring proto.GroupKey — never a
// substring search, which would misfire on initiative names that happen
// to occur inside "initiatives". Stream members strip the stream segment
// too ("marketplace/backend/pay-app" renders as "pay-app" — the stream's
// ╭ header carries that context, so the prefix would repeat it). Identity
// (CurrentSession comparison, animator keys, switch targets) always uses
// the full name; this is display only. The SWITCHER deliberately keeps
// full paths — fzf matches on display text, and three identical "pay-app"
// rows can't be targeted.
func displayName(name string) string {
	if GroupMode != "prefix" {
		return name
	}
	if i := strings.IndexByte(name, '/'); i > 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '/'); i > 0 {
		name = name[i+1:]
	}
	return name
}

// writeGroupHeader emits the one-line SYNTHETIC group header for groups
// without a home row (e.g. projects/): "  ─ name ────…" filled toward
// width, or a bare dim "  ──────" separator for the unprefixed block
// (name == ""). Groups WITH a home row get renderHomeRow instead — the
// home project row IS the header there.
func writeGroupHeader(buf *bytes.Buffer, name string, width int, collapsedN int, folded bool) {
	buf.WriteString("  ")
	{
		// A group is a group: this header renders exactly like an
		// initiative's home row — same ╭/▸ glyph, same identity hue from
		// PaletteFor, same Bold. No trailing dash fill.
		//
		// This deliberately REVERSES the earlier dim treatment. Dimming
		// made sense while group-ness was INFERRED ("a root dir that
		// happens to lack .git"), where a drawer really was accidental
		// scaffolding. With the explicit .zdev marker a drawer is a
		// declared thing, equal in standing to an initiative — the
		// difference between them is semantic (metadata, journal, the
		// initiative skill), not hierarchy, so it must not read as a
		// visual demotion.
		//
		// A collapsed drawer shows its rollup here — this line is its only
		// remaining trace. No spinner: working rows pierce per-row, so a
		// folded row is by definition quiet.
		hue := thPalette(name)
		buf.WriteString(hue)
		if folded {
			buf.WriteString("▸ ")
		} else {
			buf.WriteString("╭ ")
		}
		buf.WriteString(Bold)
		buf.WriteString(hue)
		buf.WriteString(name)
		buf.WriteString(Reset)
		if collapsedN > 0 {
			buf.WriteString(" ")
			buf.WriteString(thDim())
			fmt.Fprintf(buf, "·%d", collapsedN)
		}
	}
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// writeStreamLabel emits the one-line label that opens a workstream's run
// inside its initiative: the initiative's rail, a two-space indent, the
// stream's name in the subtle tone. A place label, not a competing frame —
// hue stays the group's alone, and the repos beneath indent two cells
// deeper than floor members. Renderer-only visual line (never a navigation
// row), same contract as writeGroupHeader; the caller maps it to the
// stream's first repo so clicks land somewhere real.
func writeStreamLabel(buf *bytes.Buffer, groupKey, stream string) {
	buf.WriteString("  ")
	buf.WriteString(thPalette(groupKey))
	buf.WriteString("│")
	buf.WriteString(Reset)
	buf.WriteString("  ")
	buf.WriteString(thSubtle())
	buf.WriteString(stream)
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// writeStreamGap emits the rail-only breathing line between a group's
// floor members and its first stream — whitespace is the structural
// medium of the calm pass. Inert like the dividers: no RowRef.
func writeStreamGap(buf *bytes.Buffer, groupKey string) {
	buf.WriteString("  ")
	buf.WriteString(thPalette(groupKey))
	buf.WriteString("│")
	buf.WriteString(Reset)
	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}

// renderHomeRow draws an initiative's HOME project (initiatives/<name>,
// the directory holding INITIATIVE.md and notes/) as its group's header:
// attention glyph (a dim ─ when idle, so a quiet initiative reads as a
// pure section rule), Bold initiative name, dim dash fill. One row — it
// replaces both the synthetic header and the home's compact row, so the
// group costs no extra line and the home stays a real, navigable FlatRow
// whose agent attention lights the header.
//
// hovered (ZDEV_SIDEBAR_HOVER, tea engine only) overrides the name's
// identity-hue coloring with thHover() — foreground-only, so it layers
// safely regardless of isCurrent/isCursor, which own the MARGIN column
// (▌/▶) and are untouched here.
func renderHomeRow(buf *bytes.Buffer, p *proto.Project, width int, animator *Animator, nowFn func() int64, isCursor, isCurrent, hovered bool, collapsedN int, folded bool) {
	switch {
	case isCurrent:
		// Same breath-pulsing ▌ the current project row carries — without
		// it, switching INTO an initiative home left the sidebar's
		// selection invisible (found live, 2026-07-30).
		buf.WriteString(thBreath(p.Name, animator.BreathFrame()))
		buf.WriteString("▌")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	case isCursor:
		buf.WriteString("▶ ")
	case hovered:
		// Same precedence and rationale as rowMargin's hover case.
		buf.WriteString(thHover())
		buf.WriteString("›")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	default:
		buf.WriteString("  ")
	}
	// Idle/absent homes open the group frame with a ╭─ corner (the │
	// gutter on member rows hangs off it); any real attention state
	// replaces the corner with its marker — attention outranks
	// decoration. The split keys on ATTENTION, not the glyph — the
	// waiting pulse's off-phase frame is itself "·".
	name := p.Name
	att := projectAttention(p)
	if att == "" || att == proto.AttIdle || p.Status == "absent" {
		buf.WriteString(thPalette(name))
		// ╭ promises a frame below it; a fully folded group has none, so
		// it opens with the disclosure triangle instead.
		if folded {
			buf.WriteString("▸")
		} else {
			buf.WriteString("╭")
		}
	} else {
		glyph, color := MarkerFor(*p, animator, nowFn())
		buf.WriteString(color)
		buf.WriteString(glyph)
	}
	buf.WriteString(Reset)
	buf.WriteString(" ")
	// The initiative's name carries its stable PaletteFor hue — the same
	// per-name color identity project names already use — so each
	// initiative is recognizable by color before it is read. No trailing
	// dash fill and no glyph+dash combo: live dogfood showed both read
	// as clutter (a pane half-full of ragged rules; "◐─ name" noise) —
	// the corner/marker, hue, and Bold carry "this is a header" alone.
	switch {
	case hovered:
		buf.WriteString(thHover())
	default:
		buf.WriteString(Bold)
		buf.WriteString(thPalette(name))
	}
	buf.WriteString(name)
	buf.WriteString(Reset)
	// Rollup (phase4-v22): a collapsed group folds its member rows into a
	// dim count on the header — "·N" reads as "N rows live under here".
	if collapsedN > 0 {
		buf.WriteString(" ")
		buf.WriteString(thDim())
		fmt.Fprintf(buf, "·%d", collapsedN)
		buf.WriteString(Reset)
	}
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
	}
	// full: words, non-zero buckets only, decision-relevant first.
	type bucket struct {
		n     int
		word  string
		color string
	}
	buckets := []bucket{
		{nDead, "dead", thChipAccent(RedPulse)},
		{nWait, "waiting", thChipAccent(Orange)},
		{nRun, "working", thChipAccent(Icy)},
		{nDone, "done", thChipAccent(Yellow)},
	}
	wrote := false
	for _, b := range buckets {
		if b.n == 0 {
			continue
		}
		if !wrote {
			buf.WriteString("  ")
		} else {
			buf.WriteString(thDim())
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
// domainSep is a function, not a const: the separator's hue must follow
// ThemeMode, which is set at startup AFTER package init would have frozen
// a const/var into classic Dim (the theme-on-expanded-project bug,
// 2026-08-02 — every other row themed, the metadata row's │ stayed grey).
func domainSep() string { return thDim() + " │ " + Reset }

// metadataPrefix returns the left-side prefix bytes for current-session
// metadata rows (marker row prefix + each populated domain row prefix
// use this verbatim). Branches:
//
//	gutter != ""             → 6 spaces (rowMargin already placed the ▌)
//	urgent=true              → RedBorder + ▌ + Reset + 5 spaces
//	urgent=false + isCurrent → BreathColorForProject + ▌ + Reset + 5 spaces
//	default (defensive)      → 6 spaces
//
// isCurrent will always be true at the renderMetadataRow call site under the
// new dispatch (twoRows := isCurrent), so the default branch is dead in
// production — kept defensively to match marker-row symmetry.
func metadataPrefix(p *proto.Project, current string, animator *Animator, urgent bool, gutterPlaced bool) string {
	isCurrent := p.Name == current && current != ""
	var b bytes.Buffer
	switch {
	case gutterPlaced:
		// Grouped current row: the marker rides the gutter at column 0, so
		// the metadata row spends the full indent to stay aligned with the
		// project row above it.
		b.WriteString("      ")
	case isCurrent:
		b.WriteString(thBreath(p.Name, animator.BreathFrame()))
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
	buf.WriteString(thDim())
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
//
// hovered (ZDEV_SIDEBAR_HOVER, tea engine only) overrides the NAME's
// coloring with thHover() — the margin/prefix dispatch above (urgent/
// current/gutter) is untouched, so a hovered row that is also current or
// urgent keeps its ▌ and just gains the name treatment.
func renderProjectRow(buf *bytes.Buffer, p *proto.Project, current string, animator *Animator, nowFn func() int64, urgent, hovered bool, teamGroups []*proto.TeamGroup, teamRows bool, gutter string) {
	// gutter: the grouped sidebar's frame prefix ("  │" in the group's
	// color) so a current row no longer punches a hole through its frame
	// ("the indentation is kinda off" — live review 2026-07-30). Empty
	// outside groups; the ▌ marker then occupies the columns the compact
	// rows spend on their own indent, keeping the glyph column aligned.
	buf.WriteString(gutter)
	isCurrent := p.Name == current && current != ""
	switch {
	case gutter != "":
		// Grouped: the caller composed the gutter from rowMargin, so the
		// marker already sits at column 0 ahead of the frame glyph. Spend
		// the indent so the content column matches unmarked member rows.
		buf.WriteString("  ")
	case isCurrent:
		buf.WriteString(thBreath(p.Name, animator.BreathFrame()))
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
		color = thDim()
	}
	// Urgency lives in the row body (redesign 2026-08-09): a red !
	// marker plus the name in the urgent hue. The margin stays pure
	// location — see rowMargin's doc comment.
	if urgent {
		glyph, color = "!", thUrgentBar()
	}
	buf.WriteString(color)
	if urgent {
		buf.WriteString(Bold)
	}
	buf.WriteString(glyph)
	buf.WriteString(Reset)
	buf.WriteString(" ")

	switch {
	case hovered:
		buf.WriteString(thHover())
	case urgent:
		buf.WriteString(Bold)
		buf.WriteString(thUrgentBar())
	case isCurrent:
		buf.WriteString(Bold)
		buf.WriteString(thPalette(p.Name))
	}
	buf.WriteString(displayName(p.Name))
	if hovered || isCurrent || urgent {
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
//
// snap + isHome (ZDEV_SIDEBAR_INITIATIVE, phase4-v23): when isHome is true
// (this row IS its group's initiative home — proto.HomeSet), two more domain
// rows follow the usual three: the Intent sentence and a member rollup +
// bd-ready count. isHome is always false for a grouped MEMBER's current row
// (renderProject's `home` case already claims any row where isHome is true,
// so control never reaches the isCurrent-member branch with isHome set).
func renderMetadataRow(buf *bytes.Buffer, p *proto.Project, current string, width int, animator *Animator, nowFn func() int64, urgent bool, gutter string, snap *proto.Snapshot, isHome bool) {
	prefix := gutter + metadataPrefix(p, current, animator, urgent, gutter != "")
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

		joinNonEmpty(inner, []*bytes.Buffer{&subBranch, &subPR, &subCI}, domainSep())
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

	// Runtime domain row: shell-cmd | ports. "$" not "▶" — the glyph
	// grammar keeps ▶ for the selection family (cursor, popup cursor);
	// a running command reads as what it is: "$ npm test".
	renderDomainRow(buf, prefix, "$", func(inner *bytes.Buffer) {
		var subShell, subPorts bytes.Buffer

		// Sub-group 1: shell command
		chipShellCmd(&subShell, p.ShellCmd)

		// Sub-group 2: ports
		chipPorts(&subPorts, p.ListeningPorts)

		joinNonEmpty(inner, []*bytes.Buffer{&subShell, &subPorts}, domainSep())
	})

	// Agent domain row: wait-age only.
	// Per-agent chips were dropped in the 260511-ohu domain-row refactor —
	// agent state surfaces through MarkerFor's attention-driven glyph
	// instead. chipWaitAge retains its full 3-tier behavior including the
	// RedPulse "! " prefix for cross-threshold urgency.
	renderDomainRow(buf, prefix, "✻", func(inner *bytes.Buffer) {
		chipWaitAge(inner, p.WaitStartedTS, now)
	})

	// Initiative metadata (ZDEV_SIDEBAR_INITIATIVE=1): see renderInitiativeRows.
	// Knob off or not-a-home ⇒ no rows, no bytes — byte-identical to the
	// pre-feature frame.
	if InitiativeEnabled && isHome {
		renderInitiativeRows(buf, snap, p, prefix, width)
	}
}

// renderCompactRow composes the SINGLE-line non-current layout:
//
//	[prefix] + marker-glyph + " " + name (truncated) + chipInlineAlerts + (wait-age if waiting) + ClearLineEnd + LF
//
// Prefix dispatch (260511-ohu change A: urgent non-current now reaches here):
//
//	gutter != "" → the gutter itself (rowMargin + frame glyph) then "  "
//	urgent=true  → {RedBorder}▌{Reset}" " (urgent accent preserved on single compact line)
//	cursor=true  → "▶ " (cursor selection indicator; zd-e6e — replaces the 2-space indent)
//	otherwise    → "  " (2-space indent)
//
// No branch, ports, shell-cmd, agent chips, or celebrate chip — those are
// scanning noise on a non-current row. Only attention-worthy signals surface.
// Per planner decision PD-02: name soft-cap at max(width-14, 10) runes.
//
// hovered (ZDEV_SIDEBAR_HOVER, tea engine only) overrides the NAME's
// coloring with thHover(), taking priority over the stale/absent/unmanaged
// dim treatment — the live pointer signal wins. The prefix dispatch above
// (gutter/urgent/cursor) is untouched, so the ▶ cursor marker survives a
// hover exactly as the spec requires.
func renderCompactRow(buf *bytes.Buffer, p *proto.Project, width int, animator *Animator, nowFn func() int64, urgent, isCursor, hovered bool, teamGroups []*proto.TeamGroup, teamRows bool, gutter string) {
	buf.WriteString(gutter)
	rowStart := buf.Len()
	switch {
	case gutter != "":
		// Grouped: rowMargin already placed the marker at column 0 ahead of
		// the frame glyph — spend the indent to hold the content column.
		buf.WriteString("  ")
	case isCursor:
		buf.WriteString("▶ ")
	case hovered:
		// Same precedence and rationale as rowMargin's hover case.
		buf.WriteString(thHover())
		buf.WriteString("›")
		buf.WriteString(Reset)
		buf.WriteString(" ")
	default:
		// Urgency deliberately absent (redesign 2026-08-09): the margin
		// means location; urgent rows announce in the row body below.
		buf.WriteString("  ")
	}

	// Marker (reuse MarkerFor with stale-dim override, same as renderProjectRow VIS-12).
	// Stale-dim skipped in "off" mode.
	pForMarker := *p
	glyph, color := MarkerFor(pForMarker, animator, nowFn())
	stale := DemoteMode != "off" && isStaleRow(p, nowFn())
	if stale || p.Unmanaged {
		color = thDim()
	}
	// Urgency lives in the row body (redesign 2026-08-09) — see
	// renderProjectRow; same ! marker + urgent-hue name here.
	if urgent {
		glyph, color = "!", thUrgentBar()
	}
	buf.WriteString(color)
	if urgent {
		buf.WriteString(Bold)
	}
	buf.WriteString(glyph)
	buf.WriteString(Reset)
	buf.WriteString(" ")

	// Name (truncated to width budget). Stale, absent, and unmanaged rows
	// recede whole-row. Unmanaged sessions are dim by design
	// (they're not tracked projects — just adopted polecat/scratch
	// sessions). The name carries the dimming where the eye actually rests.
	nameCap := width - 14
	if nameCap < 10 {
		nameCap = 10
	}
	switch {
	case hovered:
		buf.WriteString(thHover())
		buf.WriteString(truncateRunes(displayName(p.Name), nameCap))
		buf.WriteString(Reset)
	case urgent:
		buf.WriteString(Bold)
		buf.WriteString(thUrgentBar())
		buf.WriteString(truncateRunes(displayName(p.Name), nameCap))
		buf.WriteString(Reset)
	case stale || p.Status == "absent" || p.Unmanaged:
		buf.WriteString(thDim())
		buf.WriteString(truncateRunes(displayName(p.Name), nameCap))
		buf.WriteString(Reset)
	default:
		buf.WriteString(truncateRunes(displayName(p.Name), nameCap))
	}

	// The status cluster: inline alerts (PR/CI fail, PR pend, dirty), the
	// Agent Teams badge, and the wait-age. Composed into its own buffer so
	// rose-pine can RIGHT-ALIGN it: classic appends it inline after the
	// name exactly as before (byte-identical), rose-pine pads the gap so
	// the cluster lands flush with the pane edge — names ragged-right,
	// status in a scannable column.
	var tail bytes.Buffer
	chipInlineAlerts(&tail, p)

	// Agent Teams badge (phase4-v16, slice 4): the lead's row carries
	// "⊛name" + a colored bullet per teammate — for in-process teams
	// this is the members' ONLY surface.
	chipTeamBadge(&tail, teamGroups, teamRows)

	// Wait-age (compact form: no "! " prefix; chipWaitAge with "! " is for
	// current-session agent domain row only — compact rows use the tiered
	// inline form).
	if projectAttention(p) == proto.AttWaiting && p.WaitStartedTS > 0 {
		now := nowFn()
		age := now - p.WaitStartedTS
		tail.WriteString(" ")
		if ThemeMode == "rose-pine" {
			tail.WriteString(thWaiting(age))
		} else if age >= int64(WaitWarnSec) {
			tail.WriteString(Orange)
		} else {
			tail.WriteString(Dim)
		}
		tail.WriteString(formatAge(age))
		tail.WriteString(Reset)
	}

	if ThemeMode == "rose-pine" && tail.Len() > 0 {
		// Right-align: pad between the name and the cluster. The chip
		// writers each lead with " ", so trim ONE space (the pad supplies
		// the gap) and measure what this row has consumed so far — the
		// caller's gutter was written before rowStart, and width was
		// already reduced for it, so rowStart-relative math is exact.
		cluster := bytes.TrimPrefix(tail.Bytes(), []byte(" "))
		pad := width - visWidth(buf.String()[rowStart:]) - visWidth(string(cluster))
		if pad < 1 {
			pad = 1 // never glue the cluster to a long name; overflow beats collision
		}
		for i := 0; i < pad; i++ {
			buf.WriteByte(' ')
		}
		buf.Write(cluster)
	} else {
		buf.Write(tail.Bytes())
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
			nameColor, ok := teamMemberColor(m.Color)
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

// groupGutter composes the 3-column frame prefix for rows inside a group:
// the row's left margin then the frame glyph (│ run, ╰ closer, or a blank
// continuation under a closed corner), hued for initiatives and Dim for
// containers. margin is rowMargin's 2-column output — the marker sits
// BEFORE the frame, never after it.
func groupGutter(key string, hued bool, glyph, margin string) string {
	// hued is now always true in practice: every group carries its identity
	// hue since the .zdev marker made group-ness explicit (see
	// writeGroupHeader). The parameter stays so a caller can still render a
	// frame without identity — nothing does today.
	color := thDim()
	if hued {
		color = thPalette(key)
	}
	return margin + color + glyph + Reset
}

// rowMargin composes the 2-column left margin every row opens with: the
// presence/selection marker, or blanks.
//
//	urgent   → RedBorder ▌   (urgency outranks identity)
//	current  → breath-pulsed ▌
//	cursor   → ▶
//	default  → two spaces
//
// It always lands at COLUMN 0, whether or not the row belongs to a group —
// inside a group the frame gutter follows it. Before this the marker sat
// after the gutter, so it read as one fused glyph pair ("│▌", "│▶") and
// its column jumped between 0 and 3 as the cursor crossed a group boundary
// (live review 2026-07-31). Content columns are unchanged: the gutter's
// consumers spend a constant two spaces where they used to draw the marker.
// Urgency does NOT live here (redesign 2026-08-09: the margin used to
// carry a red ▌ for urgent rows — the same glyph as the current-session
// breath bar, same column, distinguishable only by hue and motion, and
// live dogfood found the two hard to tell apart. One column, one meaning:
// the margin is LOCATION — ▌ you are here, ▶ cursor, › pointer. Urgency
// is the agent's state and renders in the row body: a red ! marker and
// the name in the urgent hue — no other row has a red name, so the forms
// can never collide).
func rowMargin(p *proto.Project, animator *Animator, isCurrent, isCursor, hovered bool) string {
	switch {
	case isCurrent:
		return thBreath(p.Name, animator.BreathFrame()) + "▌" + Reset + " "
	case isCursor:
		return "▶ "
	case hovered:
		// LAST in precedence, and its own glyph: a brightened name alone
		// was too subtle to answer "what will this click hit?" (live
		// review, 2026-08-02). "›" is deliberately lighter than the
		// cursor's "▶" — the pointer is transient, the cursor and the
		// current session are state. A hovered row that is ALSO current
		// or cursor keeps that stronger marker; the name treatment still
		// tells you the pointer is there.
		return thHover() + "›" + Reset + " "
	default:
		return "  "
	}
}
