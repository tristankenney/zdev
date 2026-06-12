package layout

// Team-sweep (Agent Teams slice A, design 2026-06-12): relocate Claude Code
// teammate panes OUT of the lead's working window into their own windows in
// the same session. Claude Code spawns tmux-backend teammates as splits of
// the lead's window, which destroys the operator's three-pane layout
// (sidebar | shell | main agent) — the dogfood verdict was that no amount of
// in-window corralling fixes that. Pane IDs are location-independent in tmux
// and Claude Code addresses teammates strictly by pane ID, so break-pane
// moves a teammate without its harness noticing.
//
// PlanTeamSweep is pure, mirroring Plan: pane inventory + the pane→member
// mapping in, an ordered []Command out. The cmd layer owns reading the team
// configs (~/.claude/teams), the ZDEV_TEAM_WINDOWS knob, and the mid-join
// retry (tmuxPaneId is briefly empty while a teammate joins).

// TeamPane identifies the team member that owns a pane, keyed externally by
// pane ID. Member becomes the new window's name; both values are stamped as
// pane options so later passes (and the daemon, post-restart) can identify
// member windows without re-reading team configs.
type TeamPane struct {
	Member string
	Team   string
}

// MemberOption is the pane option that marks a relocated teammate pane.
// Pane options travel with the pane through break-pane, so the tag is
// stamped BEFORE the move and survives it.
const (
	MemberOption = "@zdev-member"
	TeamOption   = "@zdev-team"
)

// PlanTeamSweep returns the commands that move every team-member pane in w
// into its own window of w's session. Idempotency is structural: a pane
// already relocated is no longer in this window's inventory, and a member
// pane that IS the whole window (len(Panes) == 1) is already in target state.
//
// Ordering per pane: tag first (options travel with the pane), then
// break-pane with -d (never yank the operator's focus) and -n (window named
// after the member). The sidebar pane is never relocated even if a team
// config claims its ID — a corrupt/stale config must not be able to tear the
// sidebar out of the window.
func PlanTeamSweep(w Window, members map[string]TeamPane) []Command {
	if w.ID == "" || w.Session == WatcherSession {
		return nil
	}
	if len(members) == 0 || len(w.Panes) < 2 {
		return nil
	}
	var out []Command
	for _, p := range w.Panes {
		tm, ok := members[p.ID]
		if !ok || tm.Member == "" {
			continue
		}
		if p.isSidebar() {
			continue
		}
		out = append(out,
			cmd("set-option", "-p", "-t", p.ID, MemberOption, tm.Member),
			cmd("set-option", "-p", "-t", p.ID, TeamOption, tm.Team),
			cmd("break-pane", "-d", "-s", p.ID, "-t", w.Session+":", "-n", tm.Member),
		)
	}
	return out
}
