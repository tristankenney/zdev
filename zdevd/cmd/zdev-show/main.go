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
//
// zdev-show dials the daemon's unix socket, reads one snapshot, and exits.
// It never subscribes to the stream — the connection is closed immediately
// after the snapshot is received. Schema mismatch and dial errors go to
// stderr with exit code 1; "no context" cases exit 0. --legend never dials.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	row(redPulse+"●"+reset, "agent waiting (pulsing red)")
	row(icy+"◎"+reset, "shell-running (cyan)")
	row(yellow+"◆"+reset, "agent finished")
	row("·", "alive (per-project palette color)")
	row(dim+"·"+reset, "stale (>1h since activity) or absent")

	section("Branch chip")
	row("feature/foo… ", "current branch (truncated to fit)")
	row("+3", "commits ahead of base branch")

	section("PR chip (per-repo aggregate)")
	row(red+"✗ 3/5 PR"+reset, "3 of 5 open PRs are failing")
	row(orange+"⊙ 2/5 PR"+reset, "2 of 5 open PRs are pending checks")
	row(green+"✓ 2 PR"+reset, "all open PRs green")
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
	row("zdev projects", "title")
	row("🌿 / 😀 / 😬 / 🔥", "mood — calm / something finished / waiting / urgent (≥3 waits or ≥5min)")
	row("[go]", "renderer build tag (debug)")

	section("Footer tally (counts by marker)")
	row("0● 1◎ 0◆ 16· 0·", "waiting / shell-running / finished / alive / absent")

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

// formatShow renders the dim header and verbatim WaitContext for a project.
// If WaitContext is empty it returns the no-context fallback message.
func formatShow(p *proto.Project) string {
	if p.WaitContext == "" {
		return fmt.Sprintf("(no waiting context for %s)\n", p.Name)
	}
	body := p.WaitContext
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return fmt.Sprintf("%s─── %s ──%s\n%s", dim, p.Name, reset, body)
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

func defaultSocketPath() string {
	return filepath.Join(os.Getenv("HOME"),
		"Library", "Application Support", "zdev", "zdevd.sock")
}
