// team-sweep: the cmd-layer glue for layout.PlanTeamSweep (Agent Teams
// slice A). Fired by the after-split-window hook for EVERY split — teammate
// or human — so the cheap exits run first: knob off, no teams on disk, no
// tmux-backend members. Only when a team exists does it gather the window
// inventory and plan the relocation.
//
// The mid-join race: Claude Code splits the teammate pane BEFORE writing its
// tmuxPaneId into the team config (we observed the field briefly empty during
// join — docs/design/agent-teams.md). The hook fires at split time, so the
// sweep polls the configs for a bounded window while a join is visibly in
// flight (some member has an empty tmuxPaneId) and the swept window still
// holds unclaimed panes. The poll is read-only on tiny JSON files; the hook
// runs detached (run-shell -b) so tmux never waits on it.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/teams"
)

// teamSweepJoinWait bounds the mid-join poll. Observed joins resolve within
// ~1-2s; 5s covers a loaded machine without leaving hook processes lingering.
const teamSweepJoinWait = 5 * time.Second

// teamWindowsEnabled gates the whole feature (ZDEV_TEAM_WINDOWS=1). Read in
// the cmd layer — internal/ never reads the environment. config.ApplyUserEnv
// ran in main(), so the ~/.config/zdev/env gap-fill applies to hook-spawned
// invocations that never source the user's rc files.
func teamWindowsEnabled() bool { return os.Getenv("ZDEV_TEAM_WINDOWS") == "1" }

// teamSweep relocates teammate panes in one window (or, with an empty
// windowID, every window) into their own windows. Returns an exit code.
func (e *layoutEngine) teamSweep(windowID, teamsDir string) int {
	if !teamWindowsEnabled() {
		return 0
	}
	ctx := context.Background()

	deadline := time.Now().Add(teamSweepJoinWait)
	for {
		members, joinPending := memberPanes(teams.LoadAll(teamsDir))
		if len(members) > 0 {
			swept := false
			if windowID != "" {
				swept = e.sweepWindow(ctx, windowID, members)
			} else {
				for _, wid := range e.allWindows(ctx) {
					if e.sweepWindow(ctx, wid, members) {
						swept = true
					}
				}
			}
			if swept {
				return 0
			}
		}
		// Nothing relocated. Keep waiting only while a join is visibly in
		// flight — a human split with no pending join exits immediately.
		if !joinPending || time.Now().After(deadline) {
			return 0
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sweepWindow plans + applies the relocation for one window, then runs the
// normal layout pass so the source window snaps back to its three-pane shape
// in the same invocation. Returns true when panes were relocated.
func (e *layoutEngine) sweepWindow(ctx context.Context, windowID string, members map[string]layout.TeamPane) bool {
	out, err := e.run(ctx, "list-panes", "-t", windowID, "-F", inventoryFormat)
	if err != nil {
		return false // window vanished — benign on a hook hot path
	}
	win, ok := parseInventory(windowID, out)
	if !ok {
		return false
	}
	cmds := layout.PlanTeamSweep(win, members)
	if len(cmds) == 0 {
		return false
	}
	if err := e.apply(ctx, cmds); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout team-sweep: apply %s: %v\n", windowID, err)
		return false
	}
	e.processWindow(windowID)
	return true
}

// memberPanes flattens every team's tmux-backend members into the
// paneID→TeamPane map PlanTeamSweep consumes, and reports whether any
// teammate join is visibly in flight (an empty tmuxPaneId on a non-lead,
// non-in-process member — the brief mid-join state).
func memberPanes(all map[string]*teams.Team) (map[string]layout.TeamPane, bool) {
	members := make(map[string]layout.TeamPane)
	joinPending := false
	for teamName, t := range all {
		for _, m := range t.PaneMembers() {
			members[m.TmuxPaneID] = layout.TeamPane{Member: m.Name, Team: teamName}
		}
		for _, m := range t.Members {
			if m.AgentType != "team-lead" && m.TmuxPaneID == "" {
				joinPending = true
			}
		}
	}
	return members, joinPending
}
