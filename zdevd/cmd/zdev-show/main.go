// Command zdev-show prints the captured wait context for a given project,
// lists all waiting projects when called with no arguments, or prints a
// colored legend of every glyph the sidebar uses.
//
// Usage:
//
//	zdev-show                  # list every project currently in "waiting" status
//	zdev-show example/agora     # show wait context for a project (slash-form)
//	zdev-show example-agora     # same, dash-form accepted
//	zdev-show --legend         # print the sidebar glyph legend (no daemon dial)
//	zdev-show -l               # alias for --legend
//	zdev-show agents           # print the agent registry one-per-line (no daemon dial)
//	zdev-show next             # print the ranked queue, bare names (for scripts)
//	zdev-show triage           # print the ranked attention queue, annotated
//	zdev-show triage --tsv     # machine variant for fzf (name\tdisplay)
//	zdev-show triage --json    # structured queue (phone Shortcuts, widgets)
//	zdev-show list --json      # every project row as structured JSON
//	zdev-show teams            # live team members as TSV for the M-p switcher
//	zdev-show review           # human-readable landing-readiness queue
//	zdev-show review --json    # structured review gauge
//	zdev-show time             # today's commitments, InFocus, FreeUntil, calendar health
//	zdev-show time --json      # structured time-spine surface
//	zdev-show held             # the focus loop's held set (M-. parks land here)
//	zdev-show held --json      # structured held set
//
// zdev-show dials the daemon's unix socket, reads one snapshot, and exits.
// It never subscribes to the stream — the connection is closed immediately
// after the snapshot is received. Schema mismatch and dial errors go to
// stderr with exit code 1; "no context" cases exit 0. --legend and `agents`
// never dial — both read local config only so bin/zdev can use them when
// the daemon hasn't been launched yet.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/config"
	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/platform"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// ANSI codes mirror internal/render/ansi.go so the legend renders in the
// SAME colors the sidebar actually uses — the legend IS the source of truth
// for "what does this glyph mean", not a description that drifts.
const (
	dim      = "\x1b[90m"
	reset    = "\x1b[0m"
	bold     = "\x1b[1m"
	cyan     = "\x1b[36m"
	icy      = "\x1b[96m"
	yellow   = "\x1b[33m"
	orange   = "\x1b[38;5;208m" // 260511-h2: real orange, distinct from yellow
	green    = "\x1b[32m"
	red      = "\x1b[31m"
	redPulse = "\x1b[1;91m"
	// Mood block hues — mirror internal/render/theme.go MoodRed/Green/Idle.
	moodRed   = "\x1b[38;5;196m"
	moodGreen = "\x1b[38;5;46m"
	moodIdle  = "\x1b[38;5;245m"
)

func main() {
	os.Exit(run())
}

func run() int {
	// --legend / -l short-circuits before any socket dial — works even when
	// the daemon is down, so it's always discoverable.
	if len(os.Args) >= 2 && (os.Args[1] == "--legend" || os.Args[1] == "-l") {
		fmt.Print(formatLegend())
		return 0
	}

	// `agents` is the bin/zdev integration point: it lists the configured
	// agent registry one line per agent (binary<TAB>launch) so a shell
	// script can iterate and pick the first whose binary resolves on PATH.
	// No daemon dial — sidebar.toml is the source of truth and bin/zdev
	// may run before the daemon is alive.
	if len(os.Args) >= 2 && os.Args[1] == "agents" {
		out, err := formatAgents()
		if err != nil {
			fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
			return 1
		}
		fmt.Print(out)
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	snap, conn, err := socket.Subscribe(ctx, defaultSocketPath(), "", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
		return 1
	}
	// Close immediately — we are one-shot, not a streaming subscriber.
	_ = conn.Close()

	if snap.Schema != proto.SchemaVersion {
		fmt.Fprintf(os.Stderr, "zdev-show: schema mismatch: got %q, want %q (rebuild zdev-show)\n",
			snap.Schema, proto.SchemaVersion)
		return 1
	}

	if len(os.Args) < 2 {
		fmt.Print(formatList(snap))
		return 0
	}

	// `next` is the script-consumption mode behind `zdev next`: print the
	// full daemon-ranked triage queue, one bare project name per line, in
	// rank order. Emitting the whole queue (not just the head) lets the
	// consumer apply caller-side filters the daemon can't know — bin/zdev
	// skips the session the caller is already in. An empty queue prints
	// nothing and exits 0 — the consumer tests for empty output, matching
	// the "no context cases exit 0" convention above.
	if os.Args[1] == "next" {
		for _, name := range nextNames(snap) {
			fmt.Println(name)
		}
		return 0
	}

	// `triage` is the human-readable queue: one annotated line per entry
	// in daemon rank order (the same ordering zdev next consumes).
	// `triage --tsv` is the machine variant for fzf consumption:
	// <name>\t<colored display> per line, no rank numbers (the picker
	// shows position), empty output for an empty queue.
	// `triage --json` (S1) is the structured variant — the substrate for
	// phone-side Shortcuts/widgets and any consumer that shouldn't parse
	// ANSI: a JSON array in rank order, [] for an empty queue.
	if os.Args[1] == "triage" {
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "--tsv":
			fmt.Print(formatTriageTSV(snap, time.Now().Unix()))
		case len(os.Args) >= 3 && os.Args[2] == "--json":
			out, err := formatTriageJSON(snap, time.Now().Unix())
			if err != nil {
				fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
				return 1
			}
			fmt.Println(out)
		default:
			fmt.Print(formatTriage(snap, time.Now().Unix()))
		}
		return 0
	}

	// `review` (S3) is the human-readable landing-readiness queue: one line
	// per repo in daemon rank order (longest-rotting-first), with the
	// READY / NEEDS-FIX / WILL-ROT bucket counts and the repo's oldest age.
	// `review --tsv` is the fzf/machine variant (<repo>\t<colored display>);
	// `review --json` is the structured gauge — the kill-criterion observable
	// a dogfood week reads: `[]` means the gauge never populated (the bet that
	// review-bandwidth is the bottleneck is falsified), non-empty means it did.
	if os.Args[1] == "review" {
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "--tsv":
			fmt.Print(formatReviewTSV(snap))
		case len(os.Args) >= 3 && os.Args[2] == "--json":
			out, err := formatReviewJSON(snap)
			if err != nil {
				fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
				return 1
			}
			fmt.Println(out)
		default:
			fmt.Print(formatReview(snap))
		}
		return 0
	}

	// `held` (phase 1 focus loop, docs/design/command-centre.md) is the
	// debug surface over the airlock's held set: one line per item in
	// chronological order (id, kind, age, title). `held --json` is the
	// structured variant — `{"anchor": ..., "held": [...]}` (phase 3E
	// added the anchor field so a single call — bin/zdev-notify's
	// SessionStart context injection — can read both the tether and the
	// catch without a second daemon dial; "held": [] for an empty held
	// set, "anchor": null when unanchored, same "parseable absence" style
	// as `review --json`'s empty gauge).
	if os.Args[1] == "held" {
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "--json":
			out, err := formatHeldJSON(snap)
			if err != nil {
				fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
				return 1
			}
			fmt.Println(out)
		default:
			fmt.Print(formatHeld(snap, time.Now().Unix()))
		}
		return 0
	}

	// `teams` (M-p switcher): one tab-separated line per live agent-team
	// MEMBER, so the switcher can nest members under their lead project —
	// sourced from the SAME snapshot the sidebar renders (proto.FlatRows over
	// snap.TeamGroups), NOT from tmux `@zdev-team` window tags. That parity is
	// the point: in-process teammates own no tmux window, so a tag scrape
	// never sees them, but the sidebar (and now the switcher) does. Fields:
	//   <lead_project>\t<member_name>\t<status>\t<window_id>
	// window_id is empty for an in-process member — the switcher then just
	// switches to the lead's session (where that member lives) without a
	// select-window. Empty output (no live team) → the switcher shows a plain
	// project list.
	if os.Args[1] == "teams" {
		fmt.Print(formatTeamsTSV(snap))
		return 0
	}

	// `list --json` (S1): every project row as structured JSON — the
	// whole-fleet counterpart to triage --json.
	if os.Args[1] == "list" && len(os.Args) >= 3 && os.Args[2] == "--json" {
		out, err := formatListJSON(snap, time.Now().Unix())
		if err != nil {
			fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
			return 1
		}
		fmt.Println(out)
		return 0
	}

	// `time` (phase 2, docs/design/command-centre.md) is the focus-loop time
	// spine's acceptance surface: today's commitments, InFocus, FreeUntil,
	// and calendar SOURCE HEALTH. Health doesn't live on the snapshot (see
	// diag.Reply's phase-2 comment) so this is the one zdev-show subcommand
	// that dials the daemon TWICE — the normal snapshot subscription above,
	// plus a diag round-trip. A diag failure degrades to "health unknown"
	// rather than failing the whole command — the commitments/InFocus/
	// FreeUntil data the operator asked for is still valid and worth
	// printing even if the health side-channel is unreachable.
	if os.Args[1] == "time" {
		diagCtx, diagCancel := context.WithTimeout(context.Background(), 2*time.Second)
		reply, diagErr := socket.DialDiag(diagCtx, defaultSocketPath())
		diagCancel()
		now := time.Now().Unix()
		if len(os.Args) >= 3 && os.Args[2] == "--json" {
			out, err := formatTimeJSON(snap, reply)
			if err != nil {
				fmt.Fprintf(os.Stderr, "zdev-show: %v\n", err)
				return 1
			}
			fmt.Println(out)
			return 0
		}
		fmt.Print(formatTime(snap, reply, now))
		if diagErr != nil {
			fmt.Fprintf(os.Stderr, "zdev-show: calendar health unavailable: %v\n", diagErr)
		}
		return 0
	}

	target := normalizeProjectName(os.Args[1])
	p := findProject(snap, target)
	if p == nil {
		fmt.Printf("(no waiting context for %s)\n", os.Args[1])
		return 0
	}
	fmt.Print(formatShow(p))
	return 0
}

// formatLegend renders a colored, annotated key for every glyph the sidebar
// uses. Grouped by section (markers, chips, agents, time, footer, header)
// to match how the eye scans a sidebar row top-to-bottom, left-to-right.
//
// The colors here MUST mirror internal/render/ansi.go and the glyph choices
// in internal/render/chips.go — keep them in sync if either changes.
func formatLegend() string {
	var b strings.Builder
	row := func(glyph, meaning string) {
		fmt.Fprintf(&b, "  %-32s  %s%s%s\n", glyph, dim, meaning, reset)
	}
	section := func(title string) {
		fmt.Fprintf(&b, "\n%s%s%s\n", bold, title, reset)
	}

	fmt.Fprintf(&b, "%s%szdev sidebar legend%s\n", bold, cyan, reset)
	fmt.Fprintf(&b, "%seach row: marker + project, then a metadata row of chips.%s\n", dim, reset)

	section("Row marker (left of project name)")
	row(redPulse+"●"+reset, "agent waiting (pulses faster as the wait ages)")
	row(icy+"◐◓◑◒"+reset, "working / shell-running (cyan spinner)")
	row(yellow+"◆"+reset, "agent finished")
	row(redPulse+"✗"+reset, "agent died (unclean exit — static, relaunch it)")
	row("·", "alive (palette ·, full-brightness name)")
	row(dim+"· name"+reset, "stale (>1h) or no session — whole row dims")

	section("Groups & initiatives (ZDEV_SIDEBAR_GROUP=prefix)")
	row("╭─ name ──", "initiative header — IS its home row (notes/INITIATIVE.md);")
	row("", "name + frame take the initiative's stable color")
	row(redPulse+"●"+reset+"─ name ──", "attention on the home replaces the corner — marker wins")
	row("│ · name", "member row (a clone) — gutter hangs from its header;")
	row("╰ · name", "the last member closes the frame")
	row("▸ name "+dim+"·N"+reset, "folded group (nobody in it, nothing demanding) —")
	row("", "N rows folded; attention or entering auto-expands. Containers")
	row("", "fold behind their dim header the same way")
	row(dim+"─ name ──"+reset, "container header (projects/ — a pool, not an initiative)")
	row(dim+"──────"+reset, "separator: rows below are ungrouped singles")
	row(dim+"rows"+reset, "grouped rows show the LEAF (pay-app); switcher keeps full paths")

	section("Triage queue (zdev triage / next / popup)")
	row(orange+"⚡"+reset, "cheap wait — y/n or numbered prompt, seconds to answer")
	row(redPulse+"✗"+reset, "dead — tops the queue")
	row(redPulse+"●"+reset+" / "+yellow+"◆"+reset, "decision wait / finished-for-review")

	section("Review gauge (ZDEV_SIDEBAR_REVIEW=1 / zdev review)")
	row(green+"◆"+reset, "repo has PR(s) ready to land — CI green + clean tree")
	row(orange+"✗"+reset, "repo has open PR(s) with failing checks (needs a fix)")
	row(yellow+"⌁"+reset, "repo has uncommitted work on a feature branch (will rot)")
	row(green+"2 ready"+reset+dim+" · "+reset+orange+"1 fix"+reset+dim+" · "+reset+yellow+"1 rot"+reset, "per-repo bucket counts (non-zero only)")
	row(dim+"31m"+reset, "longest-rotting age in the repo — the queue is ordered by it, oldest first")

	section("Branch chip")
	row("feature/foo… ", "current branch (truncated to fit)")
	row("+3", "commits ahead of base branch")

	section("PR chip (this workspace's branch/stack)")
	row(red+"✗ 3"+reset, "3 of your stack's PRs are failing")
	row(orange+"⊙ 2"+reset, "2 of your stack's PRs are pending checks")
	row(green+"✓ 2"+reset, "all of your stack's PRs green")
	row(bold+green+"✨ merged"+reset, "PR-close celebration (60-frame window)")

	section("CI chip (per-branch latest run)")
	row(cyan+"⚙ CI"+reset, "CI queued or in_progress")
	row(green+"✓ CI"+reset, "CI completed: success")
	row(red+"✗ CI"+reset, "CI completed: failure / timed_out / action_required")
	row(dim+"(no chip)"+reset, "cancelled / skipped / no runs yet")

	section("Agent chips (claude / pi)")
	row(redPulse+"✻●"+reset+" / "+redPulse+"π●"+reset, "agent waiting on input")
	row(yellow+"✻◆"+reset+" / "+yellow+"π◆"+reset, "agent just finished")
	row(dim+"(no chip)"+reset, "agent idle, or you've visited the session since the wait started")

	section("Agent Teams (lead-row badge + member rows)")
	row(dim+"⊛ team"+reset, "team led from this row; for in-process teams the badge is the members' only surface")
	row(cyan+"•"+reset, "teammate busy/working — one bullet per member, in the member's identity color")
	row(cyan+"◦"+reset, "teammate idle — available, awaiting tasking (Tier 2a)")
	row(redPulse+"•"+reset, "teammate waiting on input (red outranks the identity color)")
	row(dim+"  • member-name"+reset, "member row nested under its lead (ZDEV_TEAM_WINDOWS=1: each tmux teammate gets its own window)")

	section("Time chips (wait age + activity age)")
	row(dim+"42s"+reset, "wait age <60s (just started)")
	row(orange+"2m"+reset, "wait age 60s-5min (warn tier)")
	row(redPulse+"! 7m"+reset, "wait age ≥5min (urgent — also fires macOS notifications)")
	row(dim+"1d"+reset, "last-activity age (only shown when ≥30s and not waiting)")

	section("Mood divider (top row)")
	row(moodIdle+"─────"+reset+" / "+moodGreen+"─────"+reset+" / "+orange+"─────"+reset+" / "+moodRed+"─────"+reset,
		"fleet mood by color — idle / finished or running / waiting / urgent (dead, ≥3 waits, or ≥5min)")

	section("Footer tally (non-zero buckets only)")
	row(redPulse+"1 dead"+reset+dim+" · "+reset+orange+"2 waiting"+reset+dim+" · "+reset+icy+"3 working"+reset,
		"what demands you; blank when the fleet is quiet")
	row("", "ZDEV_SIDEBAR_FOOTER=compact restores the glyph tally; =off hides it")

	section("Visual cues")
	row(cyan+"▌"+reset+" (left edge)", "breath bar — marks THIS sidebar's own session")
	row(bold+cyan+"project-name"+reset, "current session, bold cyan")
	row(dim+"dimmed row"+reset, "stale (>1h activity) or daemon outage")
	row("", "ZDEV_SIDEBAR_DEMOTE=fold sinks stale sessions below a dim ─── divider; =off disables stale treatment")

	fmt.Fprintf(&b, "\n%sSee also: zdev-show <project> for the captured wait-on-input prompt.%s\n", dim, reset)
	return b.String()
}

// normalizeProjectName converts slash-form ("example/agora") to dash-form
// ("example-agora") so both input styles resolve to the same project row.
func normalizeProjectName(s string) string {
	return proto.SessionKey(s)
}

// findProject returns the *proto.Project whose normalized name equals target,
// or nil if no project matches.
func findProject(snap *proto.Snapshot, target string) *proto.Project {
	for i := range snap.Projects {
		if normalizeProjectName(snap.Projects[i].Name) == target {
			return &snap.Projects[i]
		}
	}
	return nil
}

// formatShow renders the dim header, the hook-sourced WaitSummary (S1,
// when present — the agent's own last line, bolded as the headline), and
// the verbatim WaitContext for a project. If both are empty it returns
// the no-context fallback message.
func formatShow(p *proto.Project) string {
	if p.WaitContext == "" && p.WaitSummary == "" {
		return fmt.Sprintf("(no waiting context for %s)\n", p.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s─── %s ──%s\n", dim, p.Name, reset)
	if p.WaitSummary != "" {
		fmt.Fprintf(&b, "%s%s%s\n", bold, p.WaitSummary, reset)
	}
	if p.WaitContext != "" {
		b.WriteString(p.WaitContext)
		if !strings.HasSuffix(p.WaitContext, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// formatList walks all projects and prints one preview line for each project
// whose Status is "waiting". Returns "(no projects currently waiting)" when
// no waiting projects exist.
// nextNames returns the triage queue filtered to PROJECT names only — the
// `next` consumer contract. bin/zdev resolves each line to a tmux session
// and, on a has-session miss, falls through to the start-and-switch path,
// which would CREATE a junk session for a non-project label. Team-member
// triage labels ("lead/member", Agent Teams slice C) are therefore excluded
// here; members surface in the sidebar rows and `triage` views, and are
// reached via the cursor's M-Enter window path instead.
func nextNames(snap *proto.Snapshot) []string {
	projects := make(map[string]struct{}, len(snap.Projects))
	for i := range snap.Projects {
		projects[snap.Projects[i].Name] = struct{}{}
	}
	var out []string
	for _, name := range snap.Triage {
		if _, ok := projects[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

func formatList(snap *proto.Snapshot) string {
	var b strings.Builder
	for i := range snap.Projects {
		p := &snap.Projects[i]
		if p.Status != "waiting" {
			continue
		}
		preview := firstNonEmptyLine(p.WaitContext)
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		if preview == "" {
			preview = "(no captured context)"
		}
		fmt.Fprintf(&b, "%s%s%s  %s\n", dim, p.Name, reset, preview)
	}
	if b.Len() == 0 {
		b.WriteString("(no projects currently waiting)\n")
	}
	return b.String()
}

// formatTriage renders the daemon-ranked attention queue, one line per
// entry: rank, cost-class glyph, project name, wait age (or activity age
// for finished rows), and the first line of the captured wait context.
//
//  1. ⚡ example/agora-b    40s   Allow Bash(rm -rf …)?
//  2. ● example/agora-a    14m   Which approach should I take for…
//  3. ◆ example/backend    31m   (finished — review)
//
// Glyphs: ⚡ needs-permission (cheap — ranked first), ● needs-decision,
// ◆ finished. The ordering comes verbatim from Snapshot.Triage; this
// function never re-ranks.
func formatTriage(snap *proto.Snapshot, now int64) string {
	if len(snap.Triage) == 0 {
		return "(nothing needs attention)\n"
	}
	var b strings.Builder
	i := 0
	for _, name := range snap.Triage {
		p := findProject(snap, normalizeProjectName(name))
		if p == nil {
			continue // queue/projects raced; skip rather than mislead
		}
		glyph, age, gist := triageEntry(p, now)
		i++
		fmt.Fprintf(&b, "%d. %s %-24s %s%4s%s  %s%s%s\n",
			i, glyph, p.Name, dim, age, reset, dim, gist, reset)
	}
	return b.String()
}

// formatTriageTSV is the machine variant behind `zdev-show triage --tsv`:
// one queue entry per line as <name>\t<colored display>. Consumed by
// bin/zdev-triage-popup, which feeds field 2+ to fzf (--ansi) and acts on
// field 1. Empty queue → empty output (the consumer short-circuits).
func formatTriageTSV(snap *proto.Snapshot, now int64) string {
	var b strings.Builder
	for _, name := range snap.Triage {
		p := findProject(snap, normalizeProjectName(name))
		if p == nil {
			continue
		}
		glyph, age, gist := triageEntry(p, now)
		fmt.Fprintf(&b, "%s\t%s %-24s %s%4s%s  %s%s%s\n",
			p.Name, glyph, p.Name, dim, age, reset, dim, gist, reset)
	}
	return b.String()
}

// triageEntry computes the shared display pieces for one queue entry:
// glyph (⚡ cheap-to-answer / ● needs-thought / ◆ finished), wait age
// (activity age for finished rows), and the gist — the agent's own
// hook-sourced WaitSummary when present (S1), falling back to the first
// line of the scraped wait context.
//
// ⚡ means "seconds of your time, answer me first": a permission-class
// wait OR a structured prompt the AnswerCost classifier recognizes
// (numbered options, y/n). One glyph, one meaning, both fleets — in a
// bypass-permissions fleet the permission class never occurs but the
// classifier still lights it up.
func triageEntry(p *proto.Project, now int64) (glyph, age, gist string) {
	switch {
	case p.Attention == proto.AttDead:
		glyph = redPulse + "✗" + reset
	case p.Attention == proto.AttWaiting && isCheapWait(p):
		glyph = orange + "⚡" + reset
	case p.Attention == proto.AttWaiting:
		glyph = redPulse + "●" + reset
	default: // finished
		glyph = yellow + "◆" + reset
	}
	switch {
	case p.WaitStartedTS > 0:
		age = formatAge(now - p.WaitStartedTS)
	case p.LastActivityTS > 0:
		age = formatAge(now - p.LastActivityTS)
	default:
		age = "-"
	}
	gist = p.WaitSummary
	if gist == "" {
		gist = firstNonEmptyLine(p.WaitContext)
	}
	if len(gist) > 60 {
		gist = gist[:57] + "..."
	}
	if gist == "" {
		switch p.Attention {
		case proto.AttFinished:
			gist = "(finished — review)"
		case proto.AttDead:
			gist = "(agent exited — relaunch)"
		default:
			gist = "(no captured context)"
		}
	}
	return glyph, age, gist
}

// isCheapWait is the ⚡ predicate: permission-class or classifier-cheap.
func isCheapWait(p *proto.Project) bool {
	return p.WaitKind == proto.WaitKindPermission ||
		hub.AnswerCost(p.WaitContext) == hub.AnswerCostCheap
}

// triageJSONEntry is the structured form of one queue entry for
// `triage --json` / `list --json`. Field set is deliberately small and
// stable — it's a consumer contract, not a dump of proto.Project.
type triageJSONEntry struct {
	Name         string `json:"name"`
	Attention    string `json:"attention"`             // waiting | finished | working | "" (idle)
	Status       string `json:"status"`                // includes "absent" (list mode)
	WaitKind     string `json:"wait_kind,omitempty"`   // permission | decision | ""
	AnswerCost   string `json:"answer_cost,omitempty"` // "cheap" | "" (unknown)
	WaitAgeSec   int64  `json:"wait_age_sec,omitempty"`
	Acknowledged bool   `json:"acknowledged,omitempty"`
	Summary      string `json:"summary,omitempty"` // WaitSummary, falling back to scraped first line
}

func jsonEntry(p *proto.Project, now int64) triageJSONEntry {
	e := triageJSONEntry{
		Name:         p.Name,
		Attention:    string(p.Attention),
		Status:       p.Status,
		WaitKind:     p.WaitKind,
		AnswerCost:   hub.AnswerCost(p.WaitContext),
		Acknowledged: p.WaitAcknowledged,
		Summary:      p.WaitSummary,
	}
	if e.Summary == "" {
		e.Summary = firstNonEmptyLine(p.WaitContext)
	}
	if p.WaitStartedTS > 0 {
		e.WaitAgeSec = now - p.WaitStartedTS
	}
	return e
}

// formatTriageJSON marshals the daemon-ranked queue in rank order.
func formatTriageJSON(snap *proto.Snapshot, now int64) (string, error) {
	entries := make([]triageJSONEntry, 0, len(snap.Triage))
	for _, name := range snap.Triage {
		p := findProject(snap, normalizeProjectName(name))
		if p == nil {
			continue
		}
		entries = append(entries, jsonEntry(p, now))
	}
	b, err := json.Marshal(entries)
	return string(b), err
}

// formatListJSON marshals every project row (snapshot order).
func formatListJSON(snap *proto.Snapshot, now int64) (string, error) {
	entries := make([]triageJSONEntry, 0, len(snap.Projects))
	for i := range snap.Projects {
		entries = append(entries, jsonEntry(&snap.Projects[i], now))
	}
	b, err := json.Marshal(entries)
	return string(b), err
}

// formatTeamsTSV emits one tab-separated line per live team member for the
// M-p switcher:
//
//	<lead_project>\t<member_name>\t<status>\t<window_id>
//
// It walks proto.FlatRows(snap, true) — the canonical navigation order the
// sidebar and hub cursor already share — and keeps only member rows. Ordering
// therefore matches the sidebar's nested rows exactly, and in-process members
// (WindowID == "") are included: the switcher jumps to the lead's session
// (SwitchTo) and only select-windows when a WindowID is present. Empty when no
// team is live.
func formatTeamsTSV(snap *proto.Snapshot) string {
	var b strings.Builder
	for _, r := range proto.FlatRows(snap, true) {
		if !r.IsMember() {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", r.SwitchTo, r.Member.Name, r.Member.Status, r.WindowID)
	}
	return b.String()
}

// formatReview renders the S3 landing-readiness queue, one line per repo in
// daemon rank order (longest-rotting-first):
//
//  1. ◆ zitcha/agora           31m  2 ready
//  2. ⌁ solo/tool              12m  1 rot
//  3. ✗ zitcha/backend          -   1 fix
//
// The ordering and grouping come verbatim from Snapshot.ReviewGauge; this
// function never re-ranks. Empty/nil gauge → the "nothing ready" line, which
// is itself the kill-criterion signal (the gauge never populated).
func formatReview(snap *proto.Snapshot) string {
	if snap.ReviewGauge == nil || len(snap.ReviewGauge.Repos) == 0 {
		return "(nothing ready to review)\n"
	}
	var b strings.Builder
	for i, r := range snap.ReviewGauge.Repos {
		fmt.Fprintf(&b, "%d. %s %-24s %s%5s%s  %s\n",
			i+1, reviewRepoGlyph(r), r.Repo, dim, reviewAge(r), reset, reviewCounts(r))
	}
	return b.String()
}

// formatReviewTSV is the machine variant behind `zdev-show review --tsv`: one
// repo per line as <repo>\t<colored display>, mirroring triage --tsv for an
// fzf picker (field 1 is the actionable repo, field 2+ the --ansi display).
// Empty gauge → empty output.
func formatReviewTSV(snap *proto.Snapshot) string {
	if snap.ReviewGauge == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range snap.ReviewGauge.Repos {
		fmt.Fprintf(&b, "%s\t%s %-24s %s%5s%s  %s\n",
			r.Repo, reviewRepoGlyph(r), r.Repo, dim, reviewAge(r), reset, reviewCounts(r))
	}
	return b.String()
}

// formatReviewJSON marshals the gauge's per-repo entries (proto.ReviewRepo, with
// its bucket counts and contributing rows) in rank order. Emits `[]` for an
// empty/nil gauge — the explicit, parseable kill-criterion observable.
func formatReviewJSON(snap *proto.Snapshot) (string, error) {
	repos := []proto.ReviewRepo{}
	if snap.ReviewGauge != nil {
		repos = snap.ReviewGauge.Repos
	}
	b, err := json.Marshal(repos)
	return string(b), err
}

// formatHeld renders the held set behind `zdev-show held`: an anchor header
// line (phase 3A — "anchored: <title> · <age>" or "unanchored"), then one
// line per held item in chronological order (id, kind, age, title). Nothing
// renders in the sidebar for phase 1 (docs/design/command-centre.md) — this
// IS the only way to inspect the airlock's catch until a later phase's
// boundary review.
func formatHeld(snap *proto.Snapshot, now int64) string {
	var b strings.Builder
	if snap.Anchor != nil {
		age := formatAge(now - snap.Anchor.SinceTS)
		fmt.Fprintf(&b, "%s%sanchored: %s · %s%s\n", bold, cyan, snap.Anchor.Title, age, reset)
	} else {
		fmt.Fprintf(&b, "%sunanchored%s\n", dim, reset)
	}
	if len(snap.Held) == 0 {
		b.WriteString("(nothing held)\n")
		return b.String()
	}
	for i, item := range snap.Held {
		age := "-"
		if item.SinceTS > 0 {
			age = formatAge(now - item.SinceTS)
		}
		fmt.Fprintf(&b, "%d. %s%-16s%s %s%4s%s  %s%s:%s %s\n",
			i+1, dim, item.ID, reset, dim, age, reset, dim, item.Kind, reset, item.Title)
	}
	return b.String()
}

// heldJSON is the `held --json` wire shape: the held set alongside the
// anchor (phase 3E, docs/design/command-centre.md — "hook-informed focus",
// mechanism 3 "sessions know the focus"). Added so bin/zdev-notify's
// SessionStart context injection can learn BOTH "what am I anchored to"
// and "what's held" from the one cheapest daemon call this command
// already made — before this, `held --json` carried Snapshot.Held alone
// and a caller needing the anchor too would have had to make a second,
// slower round trip (`zdev-show time --json`, which also drags in a
// separate diag dial for calendar health this feature has no use for).
type heldJSON struct {
	Anchor *proto.Anchor    `json:"anchor"`
	Held   []proto.HeldItem `json:"held"`
}

// formatHeldJSON marshals the anchor + Snapshot.Held (chronological order,
// verbatim) together. `"held":[]` for an empty held set; `"anchor":null`
// when unanchored.
func formatHeldJSON(snap *proto.Snapshot) (string, error) {
	held := snap.Held
	if held == nil {
		held = []proto.HeldItem{}
	}
	out := heldJSON{Anchor: snap.Anchor, Held: held}
	b, err := json.Marshal(out)
	return string(b), err
}

// reviewRepoGlyph picks the dominant-bucket glyph for a repo: ready (green ◆ —
// landable now) outranks needs-fix (orange ✗) outranks will-rot (yellow ⌁).
// Mirrors the sidebar gauge's glyph precedence (internal/render/review_gauge.go).
func reviewRepoGlyph(r proto.ReviewRepo) string {
	switch {
	case r.Ready > 0:
		return green + "◆" + reset
	case r.NeedsFix > 0:
		return orange + "✗" + reset
	default:
		return yellow + "⌁" + reset
	}
}

// reviewCounts renders the non-zero bucket counts, each in its bucket color,
// joined by a dim middot: "2 ready", "1 fix · 1 rot".
func reviewCounts(r proto.ReviewRepo) string {
	var parts []string
	if r.Ready > 0 {
		parts = append(parts, fmt.Sprintf("%s%d ready%s", green, r.Ready, reset))
	}
	if r.NeedsFix > 0 {
		parts = append(parts, fmt.Sprintf("%s%d fix%s", orange, r.NeedsFix, reset))
	}
	if r.WillRot > 0 {
		parts = append(parts, fmt.Sprintf("%s%d rot%s", yellow, r.WillRot, reset))
	}
	return strings.Join(parts, dim+" · "+reset)
}

// formatTime renders the focus-loop time spine (phase 2,
// docs/design/command-centre.md): the InFocus/FreeUntil headline, one line
// per today's commitment in chronological order (verbatim from
// snap.Commitments — this never re-derives or re-sorts), and the calendar
// source's health. Commitments come straight off the wire; InFocus/FreeUntil
// are the hub's derivations, not recomputed here — this command is a
// window onto the daemon's view, never a second opinion on it.
func formatTime(snap *proto.Snapshot, health *diag.Reply, now int64) string {
	var b strings.Builder
	switch {
	case snap.InFocus:
		// Phase 3A: InFocus is anchored OR inside-a-commitment (an OR the
		// hub's deriveInFocus computes, never recomputed here) — note WHICH
		// one it is so the debug surface doesn't just say "in focus" and
		// leave the operator guessing why.
		cause := "commitment"
		if snap.Anchor != nil {
			cause = "anchored"
		}
		fmt.Fprintf(&b, "%s%sIn focus%s %s(%s)%s\n", bold, cyan, reset, dim, cause, reset)
	case snap.FreeUntil > 0:
		until := time.Unix(snap.FreeUntil, 0).Local()
		fmt.Fprintf(&b, "Free until %s%s%s %s(in %s)%s\n",
			bold, until.Format("15:04"), reset, dim, formatAge(snap.FreeUntil-now), reset)
	default:
		fmt.Fprintf(&b, "%sFree — nothing else today%s\n", dim, reset)
	}

	if len(snap.Commitments) == 0 {
		b.WriteString("(no commitments today)\n")
	} else {
		for i, c := range snap.Commitments {
			start := time.Unix(c.At, 0).Local().Format("15:04")
			end := "-"
			if c.Until > 0 {
				end = time.Unix(c.Until, 0).Local().Format("15:04")
			}
			kind := c.Kind
			if kind == "" {
				kind = "meeting"
			}
			// Source annotation (design amendment, docs/design/command-
			// centre.md — "The scheduled anchor and the push surface"):
			// now that commitments are multi-source, each line names WHICH
			// source contributed it — "ics" (the calendar probe, also the
			// back-compat default for a pre-multi-source empty Source) or
			// a pushed source's own name (e.g. "plan"). Appended as its
			// own trailing "(source)" segment, not folded into the
			// "[kind]" bracket, so existing "[meeting]"/"[allday]"
			// substring checks keep matching verbatim.
			source := c.Source
			if source == "" {
				source = "ics"
			}
			fmt.Fprintf(&b, "%d. %s-%s  %-28s %s[%s]%s %s(%s)%s\n", i+1, start, end, c.Title, dim, kind, reset, dim, source, reset)
		}
	}

	b.WriteString("\n")
	b.WriteString(formatCalendarHealth(health, now))
	return b.String()
}

// formatCalendarHealth renders the calendar source's health line(s):
// unreachable diag, unconfigured feed, a healthy last-fetch age, or an
// active error with how long ago the last KNOWN-GOOD fetch was (0 known-good
// fetches ever is called out explicitly — "never fetched successfully" is a
// materially different situation from "was fine 3 minutes ago").
func formatCalendarHealth(h *diag.Reply, now int64) string {
	if h == nil {
		return dim + "calendar: (health unavailable — daemon diag unreachable)" + reset + "\n"
	}
	if h.CalendarLastOK == "" && h.CalendarLastErr == "" {
		return dim + "calendar: (not configured — set ZDEV_CALENDAR_ICS)" + reset + "\n"
	}
	var b strings.Builder
	if h.CalendarLastErr != "" {
		fmt.Fprintf(&b, "%scalendar: ERROR%s %s\n", red, reset, h.CalendarLastErr)
		if okAge, ok := calendarAgeSince(h.CalendarLastOK, now); ok {
			fmt.Fprintf(&b, "%s  last ok %s ago%s\n", dim, formatAge(okAge), reset)
		} else {
			fmt.Fprintf(&b, "%s  never fetched successfully%s\n", dim, reset)
		}
		return b.String()
	}
	if okAge, ok := calendarAgeSince(h.CalendarLastOK, now); ok {
		fmt.Fprintf(&b, "%scalendar: ok%s %s(fetched %s ago)%s\n", green, reset, dim, formatAge(okAge), reset)
	}
	return b.String()
}

// calendarAgeSince parses an RFC3339Nano diag timestamp and returns its age
// relative to now. ok=false when raw is empty or unparsable.
func calendarAgeSince(raw string, now int64) (age int64, ok bool) {
	if raw == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return 0, false
	}
	return now - t.Unix(), true
}

// timeJSONOut is the structured form behind `zdev-show time --json` — a
// small, stable consumer contract (like triageJSONEntry), not a dump of
// proto.Snapshot.
type timeJSONOut struct {
	Commitments []proto.Commitment `json:"commitments"`
	InFocus     bool               `json:"in_focus"`
	FreeUntil   int64              `json:"free_until,omitempty"`
	Calendar    timeHealthJSON     `json:"calendar"`
}

type timeHealthJSON struct {
	// Configured is false only when the daemon has never observed
	// ZDEV_CALENDAR_ICS set (no last-ok timestamp AND no error ever
	// recorded) — distinct from Configured=true, LastErr!="" (configured
	// but currently broken).
	Configured bool   `json:"configured"`
	LastOK     string `json:"last_ok,omitempty"`
	LastErr    string `json:"last_err,omitempty"`
	LastErrAt  string `json:"last_err_at,omitempty"`
}

// formatTimeJSON marshals the time-spine surface. Commitments is always a
// `[]`, never `null`, on the wire — an empty day is a real, valid answer,
// not the absence of one.
func formatTimeJSON(snap *proto.Snapshot, health *diag.Reply) (string, error) {
	out := timeJSONOut{
		Commitments: snap.Commitments,
		InFocus:     snap.InFocus,
		FreeUntil:   snap.FreeUntil,
	}
	if out.Commitments == nil {
		out.Commitments = []proto.Commitment{}
	}
	if health != nil {
		out.Calendar = timeHealthJSON{
			Configured: health.CalendarLastOK != "" || health.CalendarLastErr != "",
			LastOK:     health.CalendarLastOK,
			LastErr:    health.CalendarLastErr,
			LastErrAt:  health.CalendarLastErrAt,
		}
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// reviewAge formats a repo's longest-rotting age, or "-" when unknown.
func reviewAge(r proto.ReviewRepo) string {
	if r.OldestSec > 0 {
		return formatAge(r.OldestSec)
	}
	return "-"
}

// formatAge matches the renderer's fmt_age buckets: seconds under a
// minute, then minutes, hours, days.
func formatAge(sec int64) string {
	switch {
	case sec < 0:
		return "0s"
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

// firstNonEmptyLine returns the first non-blank line from s, or "" if all
// lines are blank (or s is empty).
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			return t
		}
	}
	return ""
}

func defaultSocketPath() string { return platform.ResolveSocketPath() }

// formatAgents loads sidebar.toml and delegates to formatAgentsFromRegistry.
// Returns an error only on hard parse failures; missing file falls back to
// defaults silently per CONFIG-01.
func formatAgents() (string, error) {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return "", err
	}
	return formatAgentsFromRegistry(cfg.AgentRegistry()), nil
}

// formatAgentsFromRegistry prints one line per registered agent (registry
// declaration order) for shell-script consumption by bin/zdev. Format:
//
//	<binary><TAB><launch>
//
// where <binary> is the first whitespace-delimited token of the agent's
// launch command (used for `command -v` PATH probes in the consumer) and
// <launch> is the full launch line. Agents whose Launch is empty are
// skipped — they exist for detection only and shouldn't be auto-spawned.
func formatAgentsFromRegistry(r *agents.Registry) string {
	var b strings.Builder
	for _, spec := range r.All() {
		if spec.Launch == "" {
			continue
		}
		binary := strings.Fields(spec.Launch)
		if len(binary) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", binary[0], spec.Launch)
	}
	return b.String()
}
