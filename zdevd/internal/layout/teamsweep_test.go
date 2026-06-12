package layout

import (
	"reflect"
	"testing"
)

func sweepWindow(panes ...Pane) Window {
	return Window{ID: "@1", Session: "zitcha-backend", EffectiveWidth: 300, Panes: panes}
}

func TestPlanTeamSweep(t *testing.T) {
	shell := Pane{ID: "%1", Width: 150, Title: "shell"}
	lead := Pane{ID: "%2", Width: 150, Title: "✳ Implementing"}
	mate := Pane{ID: "%9", Width: 100, Title: "⠂ general-purpose"}
	mate2 := Pane{ID: "%10", Width: 100, Title: "✳ general-purpose"}
	sidebar := Pane{ID: "%0", Width: 50, Title: SidebarTitle, SidebarOpt: true}

	members := map[string]TeamPane{
		"%9":  {Member: "hub-core", Team: "deepen"},
		"%10": {Member: "probe-runtime", Team: "deepen"},
	}

	cases := []struct {
		name    string
		w       Window
		members map[string]TeamPane
		want    []Command
	}{
		{
			name:    "single member pane relocated, others untouched",
			w:       sweepWindow(sidebar, shell, lead, mate),
			members: members,
			want: []Command{
				cmd("set-option", "-p", "-t", "%9", MemberOption, "hub-core"),
				cmd("set-option", "-p", "-t", "%9", TeamOption, "deepen"),
				cmd("break-pane", "-d", "-s", "%9", "-t", "zitcha-backend:", "-n", "hub-core"),
				cmd("set-option", "-w", "-t", "%9", TeamOption, "deepen"),
			},
		},
		{
			name:    "two members both relocated in inventory order",
			w:       sweepWindow(shell, mate, mate2),
			members: members,
			want: []Command{
				cmd("set-option", "-p", "-t", "%9", MemberOption, "hub-core"),
				cmd("set-option", "-p", "-t", "%9", TeamOption, "deepen"),
				cmd("break-pane", "-d", "-s", "%9", "-t", "zitcha-backend:", "-n", "hub-core"),
				cmd("set-option", "-w", "-t", "%9", TeamOption, "deepen"),
				cmd("set-option", "-p", "-t", "%10", MemberOption, "probe-runtime"),
				cmd("set-option", "-p", "-t", "%10", TeamOption, "deepen"),
				cmd("break-pane", "-d", "-s", "%10", "-t", "zitcha-backend:", "-n", "probe-runtime"),
				cmd("set-option", "-w", "-t", "%10", TeamOption, "deepen"),
			},
		},
		{
			name:    "no members in window — no-op",
			w:       sweepWindow(sidebar, shell, lead),
			members: members,
			want:    nil,
		},
		{
			name:    "empty member map — no-op",
			w:       sweepWindow(shell, mate),
			members: nil,
			want:    nil,
		},
		{
			name: "member alone in its window is already target state",
			w: Window{ID: "@7", Session: "zitcha-backend", Panes: []Pane{
				{ID: "%9", Title: "⠂ general-purpose"},
			}},
			members: members,
			want:    nil,
		},
		{
			name: "sidebar never relocated even when a config claims its ID",
			w:    sweepWindow(sidebar, shell),
			members: map[string]TeamPane{
				"%0": {Member: "evil", Team: "bad"},
			},
			want: nil,
		},
		{
			name: "watcher session excluded",
			w: Window{ID: "@1", Session: WatcherSession, Panes: []Pane{
				shell, mate,
			}},
			members: members,
			want:    nil,
		},
		{
			name:    "empty member name skipped (mid-write config)",
			w:       sweepWindow(shell, mate),
			members: map[string]TeamPane{"%9": {Member: "", Team: "deepen"}},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanTeamSweep(tc.w, tc.members)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PlanTeamSweep:\n got:  %v\n want: %v", got, tc.want)
			}
		})
	}
}

// Idempotency across a simulated second pass: after the sweep the member
// panes are gone from the source window's inventory, so a re-plan is empty.
func TestPlanTeamSweep_SecondPassEmpty(t *testing.T) {
	members := map[string]TeamPane{"%9": {Member: "hub-core", Team: "deepen"}}
	after := sweepWindow(
		Pane{ID: "%0", Title: SidebarTitle, SidebarOpt: true},
		Pane{ID: "%1", Title: "shell"},
		Pane{ID: "%2", Title: "✳ Implementing"},
	)
	if got := PlanTeamSweep(after, members); got != nil {
		t.Fatalf("second pass should be a no-op, got %v", got)
	}
}
