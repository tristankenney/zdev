package layout

// Team-reap (Agent Teams slice D, design 2026-06-12): garbage-collect the
// teammate windows that team-sweep relocated, once their team is gone.
//
// When a team dissolves cleanly its config dir vanishes and the teammate
// processes exit, so their windows die on their own. But a CRASHED teammate
// leaves an orphan window: the process is dead, the pane holds a corpse
// shell, and nothing reaps it because the lead never saw a clean exit. Over
// a dogfood week these accrete — a column of dead member windows nobody can
// tell from live ones.
//
// PlanTeamReap is pure, mirroring PlanTeamSweep: the team-wide pane
// inventory (one `list-panes -a`) plus the set of teams that still exist on
// disk go in, the windows to kill come out. A window is reaped iff it
// carries at least one pane tagged with a non-empty @zdev-team whose team is
// NO LONGER live. The tag is the whole safety story: a window with no tagged
// pane is invisible to reap, so an ordinary work window can never be killed
// even if its session name collides with a dead team.

// ReapPane is one row of the team-wide pane inventory: the window that owns
// the pane, the pane itself, the @zdev-team option the pane carries (empty
// for untagged panes), and the session the window lives in. Mirrors the
// list-panes -a format string the cmd layer gathers.
type ReapPane struct {
	WindowID string
	PaneID   string
	Team     string
	Session  string
}

// ReapTarget is one window the reap will kill, paired with the dead team
// that condemned it so -dry-run can name it. Windows are reported in
// first-seen inventory order for a stable, diff-able plan.
type ReapTarget struct {
	Window string
	Team   string
}

// PlanTeamReap returns the windows to kill: those carrying a pane tagged
// with a non-empty @zdev-team whose team is absent from liveTeams.
//
// The rule is conservative by construction. A window is condemned only when
// it has at least one dead-team tag AND none of its tags name a live team —
// so a window still claimed by a surviving team is always spared, even on
// the (degenerate) chance two teams' panes share a window mid-relocation.
// Untagged windows are never candidates, and the zdevd-watcher session is
// skipped outright (it is never a team window, and must never be torn down).
func PlanTeamReap(panes []ReapPane, liveTeams map[string]bool) []ReapTarget {
	// Aggregate panes up to their windows, preserving first-seen order so
	// the plan is deterministic. Per window we track the order it appeared,
	// whether any tag named a live team, and the first dead team seen.
	order := make([]string, 0)
	seen := make(map[string]bool)
	sawLive := make(map[string]bool)
	deadTeam := make(map[string]string)
	session := make(map[string]string)

	for _, p := range panes {
		if !seen[p.WindowID] {
			seen[p.WindowID] = true
			order = append(order, p.WindowID)
			session[p.WindowID] = p.Session
		}
		if p.Team == "" {
			continue
		}
		if liveTeams[p.Team] {
			sawLive[p.WindowID] = true
			continue
		}
		if _, ok := deadTeam[p.WindowID]; !ok {
			deadTeam[p.WindowID] = p.Team
		}
	}

	var out []ReapTarget
	for _, wid := range order {
		if session[wid] == WatcherSession {
			continue
		}
		team, condemned := deadTeam[wid]
		if !condemned || sawLive[wid] {
			continue
		}
		out = append(out, ReapTarget{Window: wid, Team: team})
	}
	return out
}

// KillWindowCommands maps a reap plan to the tmux command batch the engine
// applies: one `kill-window -t <window>` per target, in plan order. Kept
// beside PlanTeamReap so the "[]Command of kill-window" half of the plan is
// pure and table-tested too; the cmd layer only chains and execs them.
func KillWindowCommands(targets []ReapTarget) []Command {
	if len(targets) == 0 {
		return nil
	}
	out := make([]Command, 0, len(targets))
	for _, t := range targets {
		out = append(out, cmd("kill-window", "-t", t.Window))
	}
	return out
}
