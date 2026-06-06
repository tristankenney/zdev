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
		for _, name := range snap.Triage {
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

	section("Triage queue (zdev triage / next / popup)")
	row(orange+"⚡"+reset, "cheap wait — y/n or numbered prompt, seconds to answer")
	row(redPulse+"✗"+reset, "dead — tops the queue")
	row(redPulse+"●"+reset+" / "+yellow+"◆"+reset, "decision wait / finished-for-review")

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

	section("Time chips (wait age + activity age)")
	row(dim+"42s"+reset, "wait age <60s (just started)")
	row(orange+"2m"+reset, "wait age 60s-5min (warn tier)")
	row(redPulse+"! 7m"+reset, "wait age ≥5min (urgent — also fires macOS notifications)")
	row(dim+"1d"+reset, "last-activity age (only shown when ≥30s and not waiting)")

	section("Header")
	row("zdev projects "+moodIdle+"█"+reset, "title + mood block")
	row(moodIdle+"█"+reset+" / "+moodGreen+"█"+reset+" / "+orange+"█"+reset+" / "+moodRed+"█"+reset,
		"mood — idle / something finished or running / waiting / urgent (dead, ≥3 waits, or ≥5min)")

	section("Footer tally (non-zero buckets only)")
	row(redPulse+"1 dead"+reset+dim+" · "+reset+orange+"2 waiting"+reset+dim+" · "+reset+icy+"3 working"+reset,
		"what demands you; blank when the fleet is quiet")
	row("", "ZDEV_SIDEBAR_FOOTER=compact restores the glyph tally; =off hides it")

	section("Visual cues")
	row(cyan+"▌"+reset+" (left edge)", "breath bar — marks THIS sidebar's own session")
	row(bold+cyan+"project-name"+reset, "current session, bold cyan")
	row(dim+"dimmed row"+reset, "stale (>1h activity) or daemon outage")

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

func defaultSocketPath() string { return platform.SocketPath() }

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
