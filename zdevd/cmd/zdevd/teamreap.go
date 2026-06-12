// team-reap: the cmd-layer glue for layout.PlanTeamReap (Agent Teams slice
// D). Unlike team-sweep — which fires off the after-split-window hook on a
// hot path — reap is a deliberate sweep run on demand (or off a slow timer):
// gather every pane across every session ONCE, compare each @zdev-team tag
// against the teams still on disk, and kill the windows whose team is gone.
//
// The tag is the safety boundary. We only ever kill a window that carries a
// @zdev-team pane option, so a window team-sweep never touched is invisible
// here — there is no path by which an ordinary work window gets reaped. The
// pure plan (internal/layout) owns the live-vs-dead decision and the
// watcher-session exclusion; this file is the I/O shell: one list-panes in,
// one batched kill-window out (or, with -dry-run, the plan to stdout).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/teams"
)

// reapInventoryFormat packs the four fields PlanTeamReap needs from one
// `tmux list-panes -a`. @zdev-team is a PANE option (windows aggregate from
// their panes); an unset option renders empty, which the plan reads as
// "untagged". session_name is LAST so a SplitN keeps it intact even on the
// vanishingly unlikely chance an earlier field carried the '|' delimiter.
const reapInventoryFormat = "#{window_id}|#{pane_id}|#{@zdev-team}|#{session_name}"

const reapFields = 4

// teamReap gathers the team-wide pane inventory, plans the kill set against
// the live teams on disk, and either prints the plan (-dry-run) or applies a
// single batched kill-window. Returns an exit code; gated by
// ZDEV_TEAM_WINDOWS=1 like the rest of the feature.
func (e *layoutEngine) teamReap(dryRun bool, teamsDir string) int {
	if !teamWindowsEnabled() {
		return 0
	}
	ctx := context.Background()

	out, err := e.run(ctx, "list-panes", "-a", "-F", reapInventoryFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout team-reap: list-panes: %v\n", err)
		return 1
	}
	panes := parseReapInventory(out)
	targets := layout.PlanTeamReap(panes, teamSet(teams.LoadAll(teamsDir)))

	if dryRun {
		// The would-kill plan: window id + the dead team that condemned it,
		// one per line, to stdout. Exit 0 without touching tmux — an empty
		// plan prints nothing, matching the rest of the show/next tooling.
		for _, t := range targets {
			fmt.Printf("%s\t%s\n", t.Window, t.Team)
		}
		return 0
	}

	cmds := layout.KillWindowCommands(targets)
	if len(cmds) == 0 {
		return 0
	}
	if err := e.apply(ctx, cmds); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout team-reap: apply: %v\n", err)
		return 1
	}
	return 0
}

// parseReapInventory turns one `list-panes -a` block into the ReapPane slice
// PlanTeamReap consumes. Blank and short rows are skipped (a torn read on a
// vanishing window is benign — the next reap catches it).
func parseReapInventory(out string) []layout.ReapPane {
	var panes []layout.ReapPane
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "|", reapFields)
		if len(f) < reapFields {
			continue
		}
		panes = append(panes, layout.ReapPane{
			WindowID: f[0],
			PaneID:   f[1],
			Team:     f[2],
			Session:  f[3],
		})
	}
	return panes
}

// teamSet flattens LoadAll's map into the name-set membership test
// PlanTeamReap wants — "does this team still exist on disk".
func teamSet(all map[string]*teams.Team) map[string]bool {
	s := make(map[string]bool, len(all))
	for name := range all {
		s[name] = true
	}
	return s
}
