package hub

import (
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/teams"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestCursorClamp_TeamDissolve pins the dissolve-clamp (Agent Teams
// slice C hard requirement): the flattened row list shrinks when a team
// dissolves while the cursor is parked on one of its member rows. The
// next cursor op must clamp into the new bounds — never index out of
// range, never wrap from a stale base onto a silently-wrong row.
func TestCursorClamp_TeamDissolve(t *testing.T) {
	s := buildTestState("proj-a", []string{"%1"}, []string{"shell"})
	s.projectListNames = []string{"proj-a"}
	s.teamWindows = true
	s.panesByID["%1"].Cwd = "/ws/proj-a"

	applyEvent(s, tmuxctl.TeamsChanged{Teams: map[string]*teams.Team{
		"alpha": {Name: "alpha", Members: []teams.Member{
			{Name: "team-lead", AgentType: "team-lead", CWD: "/ws/proj-a"},
			{Name: "blk", AgentType: "general-purpose", Color: "green", TmuxPaneID: "%42"},
		}},
	}}, nil)

	// Activate the cursor and walk it onto the member row (last row).
	applyEvent(s, tmuxctl.CursorMove{Delta: 1}, nil) // activate at 0
	rows := cursorFlatRows(s)
	if len(rows) < 2 {
		t.Fatalf("expected project+member rows, got %d", len(rows))
	}
	applyEvent(s, tmuxctl.CursorMove{Delta: len(rows) - 1}, nil)
	if s.cursorRow != len(rows)-1 {
		t.Fatalf("cursor not on member row: %d", s.cursorRow)
	}

	// Dissolve the team out from under the parked cursor.
	applyEvent(s, tmuxctl.TeamsChanged{Teams: nil}, nil)
	shrunk := cursorFlatRows(s)
	if len(shrunk) >= len(rows) {
		t.Fatalf("row list did not shrink: %d -> %d", len(rows), len(shrunk))
	}

	// Next op clamps: a +1 move from the stale index must land in bounds.
	applyEvent(s, tmuxctl.CursorMove{Delta: 1}, nil)
	if s.cursorRow < 0 || s.cursorRow >= len(shrunk) {
		t.Fatalf("cursor out of bounds after dissolve: row=%d rows=%d", s.cursorRow, len(shrunk))
	}
	// And a delta=0 (select-shaped) op from a freshly-stale index is also
	// clamped rather than reading past the end.
	s.cursorRow = 99
	applyEvent(s, tmuxctl.CursorMove{Delta: 0}, nil)
	if s.cursorRow < 0 || s.cursorRow >= len(shrunk) {
		t.Fatalf("delta=0 clamp failed: row=%d rows=%d", s.cursorRow, len(shrunk))
	}
}
