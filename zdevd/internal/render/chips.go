package render

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// projectAttention returns the project's Attention enum, falling back to
// a translation of the legacy Status string when Attention is empty
// (older daemon builds, untouched test fixtures). Centralises the
// fallback so MarkerFor / MoodFor / row chips read it the same way.
// Takes a pointer to avoid copying the proto.Project struct on hot paths
// (frame.go calls this once per project row per animation tick).
func projectAttention(p *proto.Project) proto.Attention {
	if p.Attention != "" {
		return p.Attention
	}
	switch p.Status {
	case "waiting":
		return proto.AttWaiting
	case "shell-running":
		return proto.AttWorking
	case "finished":
		return proto.AttFinished
	default:
		return proto.AttIdle
	}
}

// PaletteFor returns the ANSI escape sequence for the alive-marker hue
// of the given project name. It wraps PaletteIndex+ProjectPalette into a
// single call for use in MarkerFor and frame composition.
func PaletteFor(name string) string {
	return ProjectPalette[PaletteIndex(name)]
}

// isStaleRow reports whether the project renders as stale per VIS-12:
// idle with a known last-activity older than StaleThresholdSec. Single
// predicate for the marker dim-out and the row-recede treatment —
// dogfood feedback (2026-06-06): a palette `·` vs a dim `·` is not
// distinguishable at one cell, so staleness must also recede the name.
// In "off" mode callers skip this check entirely (DemoteMode=="off" guard
// lives at the call site — this predicate is mode-agnostic).
func isStaleRow(p *proto.Project, now int64) bool {
	return projectAttention(p) == proto.AttIdle &&
		p.LastActivityTS > 0 &&
		now-p.LastActivityTS >= int64(StaleThresholdSec)
}

// isDemotedRow reports whether p should be relocated to the fold section
// when DemoteMode=="fold". Uses DemoteThresholdSec (runtime-configurable)
// instead of the StaleThresholdSec constant so fold and dim thresholds can
// diverge independently.
func isDemotedRow(p *proto.Project, now int64) bool {
	return projectAttention(p) == proto.AttIdle &&
		p.LastActivityTS > 0 &&
		now-p.LastActivityTS >= int64(DemoteThresholdSec)
}

// MarkerFor returns the (glyph, ansiColor) pair for the given project's
// current Attention state, per VIS-01 / bash baseline lines 484-517.
//
//   - Waiting   → animator.PulseGlyphAt(age) + the age-paced wait ramp
//   - Working   → animator.WorkGlyph() spinner + thWorking()
//   - Finished  → "◆" + Yellow
//   - Idle, absent, or unknown → " " (blank) + Dim
//
// Color budget (calm pass, 2026-08-19): color is spent on STATE, not
// identity — idle used to carry PaletteFor(p.Name) unconditionally
// (decoration: the row's fixed position and its own name already say
// which project this is), which was the single largest source of a quiet
// fleet still reading as a rainbow of unrelated hues. Idle is now always
// Dim, full stop.
//
// Working briefly went the other way too — identity-hued instead of one
// shared color — and got reverted the same day (live feedback, 2026-08-20:
// "non-obvious what the different colours are for" / "don't need
// different colours for initiatives"). Two repos both just "working" in
// two unrelated hues doesn't communicate anything; it looks like it
// should mean something and doesn't, which is worse than one flat color.
// Working is back to thWorking() — semantic, matching waiting/dead/done,
// which all already say what they mean through ONE color each rather than
// per-instance identity. Waiting's existing age ramp (thWaiting) was never
// part of this back-and-forth — that color already IS real information
// (how stale the wait is), not decoration, so it never moved.
//
// Glyph budget (2026-08-19, same pass): once the idle dot lost its color it
// stopped carrying anything either — a live A/B compared a column of dim
// "·"s against a blank column feeding the same fixture, and the blank read
// as quieter with no loss of scanability (the fixed 2-space margin still
// lines every name up), while the real markers (spinner, pulse, ✗) stood
// out MORE against true blank than against a wash of identical grey dots.
// Idle/absent/unknown now render a blank space instead of "·" — same
// column width, so alignment is unchanged; only a real attention state
// puts ink in that column now.
//
// Falls back to the legacy Status string when Attention is empty — the
// daemon may be running a binary built before the Attention field was
// added, or a test fixture may not have populated it.
func MarkerFor(p proto.Project, animator *Animator, now int64) (glyph, color string) {
	att := p.Attention
	if att == "" {
		// Back-compat path. Step-1 commit fb2667b made Status a
		// projection of Attention, so they agree in production, but
		// keep this fallback until the field is universally set.
		switch p.Status {
		case "waiting":
			att = proto.AttWaiting
		case "shell-running":
			att = proto.AttWorking
		case "finished":
			att = proto.AttFinished
		case "alive":
			att = proto.AttIdle
		default:
			// "absent" or unknown — blank, Dim (glyph budget, 2026-08-19).
			return " ", thDim()
		}
	}
	switch att {
	case proto.AttWaiting:
		// Age-paced pulse (dogfood #3): calm blink for fresh waits,
		// accelerating through the warn and urgent tiers.
		var age int64
		if p.WaitStartedTS > 0 {
			age = now - p.WaitStartedTS
		}
		return animator.PulseGlyphAt(age), thWaiting(age)
	case proto.AttWorking:
		// Animated spinner (dogfood 2026-06-06): running work is the
		// convention for motion, not a static ring. The footer tally
		// keeps the static ◎ as the bucket's label. thWorkingBreath —
		// semantic, not identity-hued (tried and reverted 2026-08-20, see
		// the doc comment above) — adds a shared brightness breath so
		// every working row in the fleet pulses together in the SAME
		// phase (delight, 2026-08-20): motion in intensity as well as
		// spinner rotation, without differentiating by project.
		return animator.WorkGlyph(), thWorkingBreath(animator.BreathFrame())
	case proto.AttFinished:
		return "◆", thDone()
	case proto.AttDead:
		// Hook-confirmed unclean exit (NOW#3) — static, no pulse: a dead
		// agent isn't asking, it's gone. RedPulse color carries the
		// urgency; the glyph carries the difference.
		return "✗", thDead()
	case proto.AttIdle:
		// Blank, not "·" (glyph budget, 2026-08-19) — see the doc comment
		// above.
		return " ", thDim()
	default:
		return " ", thDim()
	}
}

// teamMemberColor maps an Agent Teams member color name (the config.json
// "color" field — blue/green/... assigned by Claude Code at join time) to
// a themed foreground. Classic keeps the original xterm-256 codes byte for
// byte; rose-pine maps each name onto the nearest identity token so a team
// badge stops mixing palettes (calm pass lane A). Unknown colors render
// dim — fail-soft, the chip still counts the member.
func teamMemberColor(name string) (string, bool) {
	if ThemeMode == "rose-pine" {
		m := map[string]rpRGB{
			"blue": rpPine, "green": rpFoam, "yellow": rpGold,
			"red": rpLove, "purple": rpIris, "orange": rpRose,
			"pink": rpRose, "cyan": rpFoam,
		}
		if c, ok := m[name]; ok {
			return c.fg(), true
		}
		return "", false
	}
	m := map[string]string{
		"blue":   "\x1b[38;5;75m",
		"green":  "\x1b[38;5;114m",
		"yellow": "\x1b[38;5;221m",
		"red":    "\x1b[38;5;203m",
		"purple": "\x1b[38;5;135m",
		"orange": "\x1b[38;5;215m",
		"pink":   "\x1b[38;5;212m",
		"cyan":   "\x1b[38;5;80m",
	}
	c, ok := m[name]
	return c, ok
}

// chipTeamBadge composes the Agent Teams badge for a lead's row
// (phase4-v16, MVP slice 4): dim "⊛ <name>" + one colored bullet per
// teammate. In-process teammates are exactly as real as tmux ones here —
// the badge is the ONLY surface they have (no pane, no hooks). Name
// truncated to 10 runes; a 4-member team costs ~17 cells total.
func chipTeamBadge(buf *bytes.Buffer, groups []*proto.TeamGroup, teamRows bool) {
	for _, g := range groups {
		chipOneTeamBadge(buf, g, teamRows)
	}
}

func chipOneTeamBadge(buf *bytes.Buffer, g *proto.TeamGroup, teamRows bool) {
	if g == nil {
		return
	}
	buf.WriteString(" ")
	buf.WriteString(thDim())
	buf.WriteString("⊛")
	buf.WriteString(truncateRunes(g.Name, 10))
	buf.WriteString(Reset)
	// When member rows render (ZDEV_TEAM_WINDOWS), the ⊛<name> badge is the
	// team MARKER only — per-member state moves to the nested rows, so the
	// bullets are suppressed here to avoid duplicating the signal.
	if teamRows {
		return
	}
	for _, m := range g.Members {
		c, ok := teamMemberColor(m.Color)
		if !ok {
			c = thDim()
		}
		if m.Status == "waiting" {
			// Blocked on input outranks the member's identity color —
			// same hue the row markers use for waiting.
			c = thChipAccent(RedPulse)
		}
		buf.WriteString(c)
		if m.Status == "idle" {
			// Hollow bullet: available, awaiting tasking (Tier 2a).
			buf.WriteString("◦")
		} else {
			buf.WriteString("•")
		}
		buf.WriteString(Reset)
	}
}

// memberGlyph maps a proto.TeamMember.Status string to the (glyph, color)
// pair used by the nested member rows (Agent Teams slice B). The glyphs are
// the static project-row language — not the lead row's animated spinner /
// pulse — because a teammate row is a status readout, not the focal pulse:
//
//	"working" → "✳" Icy
//	"waiting" → "●" RedPulse (blocked on input — the only attention-drawing one)
//	"done"    → "◆" Yellow
//	"idle" / "" / unknown → "·" Dim
func memberGlyph(status string) (glyph, color string) {
	switch status {
	case "working":
		return "✳", thWorking()
	case "waiting":
		return "●", thChipAccent(RedPulse)
	case "done":
		return "◆", thDone()
	default:
		// idle, "", and any unrecognised value recede to a dim dot.
		return "·", thDim()
	}
}

// MoodFor returns the fleet-mood ANSI color per VIS-04 / PD-06. Since
// the header row's removal (dogfood: "the zdev projects header doesn't
// add anything" — the pane border already names the pane), the DIVIDER
// carries the mood as its color; this returns the bare color code and
// the divider composes it.
//
// Tiers (highest priority first):
//
//	urgent = any wait-age >= WaitUrgentSec, any dead, OR count(waiting) >= 3 → MoodRed
//	warn   = count(waiting) >= 1                                            → Orange
//	happy  = count(finished) > 0 OR count(shell-running) > 0                → MoodGreen
//	idle   = otherwise                                                      → MoodIdle
func MoodFor(snap *proto.Snapshot, nowFn func() int64) string {
	now := nowFn()
	nWait, nDone, nRun := 0, 0, 0
	urgent := false
	for i := range snap.Projects {
		p := &snap.Projects[i]
		switch projectAttention(p) {
		case proto.AttWaiting:
			nWait++
			if p.WaitStartedTS > 0 && now-p.WaitStartedTS >= int64(WaitUrgentSec) {
				urgent = true
			}
		case proto.AttDead:
			// A dead agent is immediately urgent — nothing escalates it
			// later (NOW#3), so the mood block must carry it now.
			nWait++
			urgent = true
		case proto.AttFinished:
			nDone++
		case proto.AttWorking:
			nRun++
		}
	}
	switch {
	case urgent || nWait >= 3:
		return MoodRed
	case nWait >= 1:
		return Orange
	case nDone > 0 || nRun > 0:
		return MoodGreen
	default:
		return MoodIdle
	}
}

// chipBranchWithCap composes the branch + ahead/behind chip with a
// configurable rune cap for the branch name. The runeCap parameter
// allows current-session rows to display more of the branch (cap=24)
// while other callers use the 14-rune default.
//
// Suppresses output for default branches (IsDefaultBranch guard).
// Format: {Cyan}{branch}[ ↑ahead][↓behind]{Reset}
func chipBranchWithCap(buf *bytes.Buffer, branch string, ahead, behind int, runeCap int) {
	if branch == "" || IsDefaultBranch(branch) {
		return
	}
	buf.WriteString(thChipAccent(Cyan))
	buf.WriteString(truncateRunes(branch, runeCap))
	if ahead > 0 || behind > 0 {
		buf.WriteByte(' ')
		if ahead > 0 {
			buf.WriteString("↑")
			buf.WriteString(strconv.Itoa(ahead))
		}
		if behind > 0 {
			buf.WriteString("↓")
			buf.WriteString(strconv.Itoa(behind))
		}
	}
	buf.WriteString(Reset)
}

// chipBranch composes the branch + ahead/behind chip per DATA-01 /
// bash baseline lines 522-535. Uses the 14-rune default cap.
//
// Suppresses output for default branches (IsDefaultBranch guard).
// Format: {Cyan}{branch}[ ↑ahead][↓behind]{Reset}
func chipBranch(buf *bytes.Buffer, branch string, ahead, behind int) {
	chipBranchWithCap(buf, branch, ahead, behind, 14)
}

// chipDirty composes the dirty-count chip per DATA-02 / bash baseline
// line 536-538.
//
// Format: {Orange}+N{Reset}. Suppressed when count == 0.
func chipDirty(buf *bytes.Buffer, count int) {
	if count == 0 {
		return
	}
	buf.WriteString(thChipAccent(Orange))
	buf.WriteByte('+')
	buf.WriteString(strconv.Itoa(count))
	buf.WriteString(Reset)
}

// chipShellCmd composes the shell-command chip per DATA-03 / bash
// baseline lines 539-543.
//
// Format: {Icy}▶ {Truncate14(cmd)}{Reset}. Suppressed when cmd == "".
func chipShellCmd(buf *bytes.Buffer, cmd string) {
	if cmd == "" {
		return
	}
	// No glyph prefix: the runtime domain row's "$" already introduces
	// the command (calm lane B — the old "▶ " here doubled the row's
	// domain glyph).
	buf.WriteString(thChipAccent(Icy))
	buf.WriteString(Truncate14(cmd))
	buf.WriteString(Reset)
}

// chipPRAggregate composes the PR tri-state chip per DATA-04 / bash
// baseline lines 553-567. Tightened in 260511-n4n task 5: the ' PR'
// suffix and '/N' denominator are dropped for a more compact display.
// Since 260606 the counts arrive branch/stack-scoped from the gh probe
// (THIS workspace's PRs, not the whole repo's) — see parseGhJSON.
//
// Suppressed when open == 0 OR when celebrating == true (DATA-05
// celebration chip occupies the PR slot during the window).
//
// Format:
//   - open>0 + fail>0 → {Red}✗ Nfail{Reset}
//   - open>0 + pend>0 + fail==0 → {Orange}⊙ Npend{Reset}
//   - open>0 + fail==0 + pend==0 → {Green}✓ Nopen{Reset}
func chipPRAggregate(buf *bytes.Buffer, open, fail, pend int, celebrating bool) {
	if open == 0 || celebrating {
		return
	}
	if fail > 0 {
		fmt.Fprintf(buf, "%s✗ %d%s", thChipAccent("\x1b[31m"), fail, Reset)
	} else if pend > 0 {
		fmt.Fprintf(buf, "%s⊙ %d%s", thChipAccent(Orange), pend, Reset)
	} else {
		fmt.Fprintf(buf, "%s✓ %d%s", thChipAccent(Green), open, Reset)
	}
}

// chipCelebrate composes the PR-celebration chip per DATA-05 / bash
// baseline lines 544-552.
//
// Returns true when the celebration window is active (celebrateUntil >
// now) and appends the celebration chip to buf. Returns false (and writes
// nothing) when the window has expired.
//
// A landing is rare by definition (a PR just merged) — exactly the moment
// a real flourish is cheap, since it can't become noise if it only shows
// up once per landing (delight pass, 2026-08-20). The chip's own frozen
// glyph never earned that; frame (the breath cadence, ~4s per half-cycle)
// alternates it between two equal-width star glyphs across the ~4s
// window, a small twinkle in place of one static ✨. Equal width by
// design — both variants are 8 runes — so it never perturbs row layout.
func chipCelebrate(buf *bytes.Buffer, celebrateUntil int64, now int64, frame int) bool {
	if celebrateUntil <= now {
		return false
	}
	buf.WriteString(Bold)
	buf.WriteString(thChipAccent(Green))
	if frame%2 == 0 {
		buf.WriteString("✨ merged")
	} else {
		buf.WriteString("✦ merged")
	}
	buf.WriteString(Reset)
	return true
}

// chipPorts composes the listening-ports chip per DATA-06 / bash
// baseline lines 568-573.
//
// Renders at most 4 ports. Format: {Dim}:port1 :port2 ...{Reset}
func chipPorts(buf *bytes.Buffer, ports []int) {
	if len(ports) == 0 {
		return
	}
	max := 4
	if len(ports) < max {
		max = len(ports)
	}
	buf.WriteString(thDim())
	for i := 0; i < max; i++ {
		if i > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(ports[i]))
	}
	buf.WriteString(Reset)
}

// chipWaitAge composes the wait-age escalation chip per DATA-09 / bash
// baseline lines 600-611. Restored to 3-tier in 260511-nxy after the urgent
// tier was removed in 260511-n4n (bg-fill approach dropped due to SGR-state
// bleed; the "! " prefix + RedPulse are now the chip-level urgency signal again).
//
// Suppressed when waitStartedTS == 0. Tiers:
//
//	age < WaitWarnSec                      → {Dim}<age>{Reset}
//	WaitWarnSec <= age < WaitUrgentSec     → {Orange}<age>{Reset}
//	age >= WaitUrgentSec                   → {RedPulse}"! "<age>{Reset}
func chipWaitAge(buf *bytes.Buffer, waitStartedTS int64, now int64) {
	if waitStartedTS == 0 {
		return
	}
	age := now - waitStartedTS
	ageStr := formatAge(age)
	if ThemeMode == "rose-pine" {
		// One ramp for the marker and the age (thWaiting); the "! "
		// urgency prefix survives the theme — it is information, not
		// decoration.
		buf.WriteString(thWaiting(age))
		if age >= int64(WaitUrgentSec) {
			buf.WriteString("! ")
		}
		buf.WriteString(ageStr)
		buf.WriteString(Reset)
		return
	}
	switch {
	case age >= int64(WaitUrgentSec):
		buf.WriteString(RedPulse)
		buf.WriteString("! ")
		buf.WriteString(ageStr)
		buf.WriteString(Reset)
	case age >= int64(WaitWarnSec):
		buf.WriteString(thChipAccent(Orange))
		buf.WriteString(ageStr)
		buf.WriteString(Reset)
	default:
		buf.WriteString(thDim())
		buf.WriteString(ageStr)
		buf.WriteString(Reset)
	}
}

// isUrgent reports whether the project should render in urgent mode
// (red ▌ left-border accent + force-expanded to 2 rows). Pure function for
// testability. Returns false early when WaitAcknowledged is true — the user
// has already visited the session past the highest crossed wait-tier, so the
// red urgent decoration is suppressed regardless of age.
func isUrgent(p *proto.Project, now int64) bool {
	if p.WaitStartedTS == 0 || p.WaitAcknowledged {
		return false
	}
	return now-p.WaitStartedTS >= int64(WaitUrgentSec)
}

// isCIFailure reports whether the (status, conclusion) pair represents
// a failed CI run. Failure set matches chipCI's failure-conclusion
// branch: status=="completed" AND conclusion not in {"", "success",
// "cancelled", "skipped", "neutral"}.
func isCIFailure(status, conclusion string) bool {
	if status != "completed" {
		return false
	}
	switch conclusion {
	case "", "success", "cancelled", "skipped", "neutral":
		return false
	default:
		return true
	}
}

// isCIPending reports whether the CI is queued or in progress.
func isCIPending(status string) bool {
	return status == "queued" || status == "in_progress"
}

// chipInlineAlerts writes 0..5 compact alert tokens for the non-current
// 1-line layout (260511-n4n + 260511-r7x). Each token is prefixed with a
// leading space; empty when nothing is wrong. Order:
//
//	" ✗N"   if PRFail > 0     (Red, PR fail count)
//	" ✗ CI" if isCIFailure    (Red, CI failure — no count; tokens are
//	                           separated to avoid the "✗1 means what?"
//	                           ambiguity introduced by the original
//	                           260511-n4n union)
//	" ⚙ CI" if isCIPending    (Cyan, CI queued / in_progress —
//	                           260511-r7x change C)
//	" ⊙N"   if PRPend > 0     (Orange, PR pend count)
//	" +N"   if DirtyCount > 0 (Orange, dirty count)
//
// CI success / ambiguous outcomes are suppressed (absence = healthy,
// mirrors PR-success suppression on compact rows).
func chipInlineAlerts(buf *bytes.Buffer, p *proto.Project) {
	// PR failure — highest visual priority, has a count.
	if p.PRFail > 0 {
		buf.WriteString(" ")
		buf.WriteString(thChipAccent("\x1b[31m"))
		buf.WriteString("✗")
		buf.WriteString(strconv.Itoa(p.PRFail))
		buf.WriteString(Reset)
	}

	// CI failure — separate token (no count; CI is binary failing/healthy).
	if isCIFailure(p.CIStatus, p.CIConclusion) {
		buf.WriteString(" ")
		buf.WriteString(thChipAccent("\x1b[31m"))
		buf.WriteString("✗ CI")
		buf.WriteString(Reset)
	}

	// CI pending — Cyan (matches chipCI's queued/in_progress hue).
	if isCIPending(p.CIStatus) {
		buf.WriteString(" ")
		buf.WriteString(thChipAccent(Cyan))
		buf.WriteString("⚙ CI")
		buf.WriteString(Reset)
	}

	// PR pending — Orange.
	if p.PRPend > 0 {
		buf.WriteString(" ")
		buf.WriteString(thChipAccent(Orange))
		buf.WriteString("⊙")
		buf.WriteString(strconv.Itoa(p.PRPend))
		buf.WriteString(Reset)
	}

	// Dirty count — Orange.
	if p.DirtyCount > 0 {
		buf.WriteString(" ")
		buf.WriteString(thChipAccent(Orange))
		buf.WriteString("+")
		buf.WriteString(strconv.Itoa(p.DirtyCount))
		buf.WriteString(Reset)
	}
}

// chipCI composes the per-branch CI status chip (260509-gfz, suffix added 260510-k4p).
//
// Format mirrors chipPRAggregate: glyph + " CI" suffix so the chip is
// self-identifying and not confusable with chipPRAggregate's bare ✓/✗ glyphs
// that share the same row.
//
// Status mapping:
//
//	"queued" | "in_progress"    → {Cyan}⚙ CI{Reset}
//	"completed" + "success"     → {Green}✓ CI{Reset}
//	"completed" + non-success   → {Red}✗ CI{Reset} (failure, timed_out, action_required)
//	"completed" + ambiguous     → suppressed (cancelled, skipped, neutral, "")
//	"" or any other status       → suppressed
//
// Failure-conclusion set per task_context: anything not in {success, cancelled,
// skipped, neutral, ""} renders as failure — covers failure, timed_out,
// action_required, startup_failure, and any future GitHub conclusion value.
//
// 260512-cgw: failing-check names moved to a dedicated marquee-scroll row
// (renderFailingChecksRow) so they survive long lists without truncation. This
// chip stays binary on both current and non-current rows.
func chipCI(buf *bytes.Buffer, status, conclusion string) {
	switch {
	case isCIPending(status):
		buf.WriteString(thChipAccent(Cyan))
		buf.WriteString("⚙ CI")
		buf.WriteString(Reset)
	case status == "completed" && conclusion == "success":
		buf.WriteString(thChipAccent(Green))
		buf.WriteString("✓ CI")
		buf.WriteString(Reset)
	case isCIFailure(status, conclusion):
		buf.WriteString(thChipAccent("\x1b[31m")) // match chipPRAggregate's failure literal
		buf.WriteString("✗ CI")
		buf.WriteString(Reset)
	default:
		// unknown status OR "completed" with ambiguous conclusion ("", cancelled, skipped, neutral)
		// → suppressed (no output)
	}
}

// failingChecksGap is the visual gap rendered between cycle repetitions when
// the failing-checks list scrolls. Three spaces is enough to make the wrap
// visually clear without consuming meaningful display width.
const failingChecksGap = "   "

// failingChecksScrollMs is the per-rune step interval in milliseconds.
// 200ms = 5 runes/sec — readable at conversational reading speed without
// outpacing the renderer's 15fps animated cadence (≈67ms per frame, so a
// new step lands every ~3 frames).
const failingChecksScrollMs int64 = 200

// failingChecksScrollWindow returns the visible slice of the marquee at the
// given wall-clock timestamp `nowMs` (unix milliseconds). When the joined
// check list fits within `width` runes, the list is returned verbatim (no
// scrolling). Otherwise the cycle = joined + gap is concatenated to itself
// and a window of `width` runes starting at
// `offset = (nowMs / failingChecksScrollMs) % cycleLen` is returned.
// Pure function — caller wraps the result in color codes.
//
// 260512-cgw: pulled out into its own helper so the slicing math is unit
// testable independently of ANSI output.
// 260512-cmx: switched from unix-seconds to unix-milliseconds so the marquee
// can advance faster than 1 rune/sec.
func failingChecksScrollWindow(names []string, width int, nowMs int64) string {
	if len(names) == 0 || width <= 0 {
		return ""
	}
	joined := strings.Join(names, ", ")
	joinedRunes := []rune(joined)
	if len(joinedRunes) <= width {
		return joined
	}
	cycle := joined + failingChecksGap
	cycleRunes := []rune(cycle)
	cycleLen := int64(len(cycleRunes))
	step := nowMs / failingChecksScrollMs
	offset := int(((step % cycleLen) + cycleLen) % cycleLen)
	// Concat cycle+cycle so we can take a contiguous slice across the wrap.
	doubled := append(append([]rune{}, cycleRunes...), cycleRunes...)
	return string(doubled[offset : offset+width])
}

// renderFailingChecksRow writes the marquee-scrolled failing-check names to
// buf, wrapped in red. Returns true when output was written (caller controls
// the surrounding domain-row plumbing — prefix, glyph, ClearLineEnd, newline).
//
// width is the inner-body rune budget AFTER the leading prefix + glyph + space
// have been consumed; renderMetadataRow does the subtraction at the call site
// so this helper stays pure.
func renderFailingChecksRow(buf *bytes.Buffer, names []string, width int, nowMs int64) bool {
	if len(names) == 0 || width <= 0 {
		return false
	}
	window := failingChecksScrollWindow(names, width, nowMs)
	if window == "" {
		return false
	}
	buf.WriteString(thChipAccent("\x1b[31m")) // match chipCI / chipPRAggregate's failure red
	buf.WriteString(window)
	buf.WriteString(Reset)
	return true
}

// formatAge formats a duration in seconds as a human-readable string
// matching the bash baseline fmt_age function.
//
//	<60   → "<n>s"
//	<3600 → "<n>m"
//	<86400 → "<n>h"
//	else   → "<n>d"
func formatAge(seconds int64) string {
	switch {
	case seconds < 60:
		return strconv.FormatInt(seconds, 10) + "s"
	case seconds < 3600:
		return strconv.FormatInt(seconds/60, 10) + "m"
	case seconds < 86400:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	default:
		return strconv.FormatInt(seconds/86400, 10) + "d"
	}
}
