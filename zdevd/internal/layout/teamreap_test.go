package layout

import (
	"reflect"
	"testing"
)

func TestPlanTeamReap(t *testing.T) {
	live := map[string]bool{"deepen": true}

	cases := []struct {
		name  string
		panes []ReapPane
		live  map[string]bool
		want  []ReapTarget
	}{
		{
			name: "live team spared",
			panes: []ReapPane{
				{WindowID: "@5", PaneID: "%9", Team: "deepen", Session: "zitcha-backend"},
			},
			live: live,
			want: nil,
		},
		{
			name: "dead team reaped",
			panes: []ReapPane{
				{WindowID: "@6", PaneID: "%9", Team: "ghost", Session: "zitcha-backend"},
			},
			live: live,
			want: []ReapTarget{{Window: "@6", Team: "ghost"}},
		},
		{
			name: "untagged windows untouched",
			panes: []ReapPane{
				{WindowID: "@1", PaneID: "%0", Team: "", Session: "zitcha-backend"},
				{WindowID: "@1", PaneID: "%1", Team: "", Session: "zitcha-backend"},
			},
			live: live,
			want: nil,
		},
		{
			name: "watcher session excluded even when tagged dead",
			panes: []ReapPane{
				{WindowID: "@2", PaneID: "%9", Team: "ghost", Session: WatcherSession},
			},
			live: live,
			want: nil,
		},
		{
			name: "mixed fleet: only dead-team windows reaped, inventory order",
			panes: []ReapPane{
				{WindowID: "@1", PaneID: "%0", Team: "", Session: "s"},       // untagged
				{WindowID: "@6", PaneID: "%9", Team: "ghost", Session: "s"},  // dead
				{WindowID: "@5", PaneID: "%8", Team: "deepen", Session: "s"}, // live
				{WindowID: "@7", PaneID: "%10", Team: "old", Session: "s"},   // dead
			},
			live: live,
			want: []ReapTarget{{Window: "@6", Team: "ghost"}, {Window: "@7", Team: "old"}},
		},
		{
			name: "window with both a live and a dead tag is spared",
			panes: []ReapPane{
				{WindowID: "@8", PaneID: "%9", Team: "ghost", Session: "s"},
				{WindowID: "@8", PaneID: "%10", Team: "deepen", Session: "s"},
			},
			live: live,
			want: nil,
		},
		{
			name:  "empty inventory — no-op",
			panes: nil,
			live:  live,
			want:  nil,
		},
		{
			name: "no live teams at all — every tagged window reaped",
			panes: []ReapPane{
				{WindowID: "@6", PaneID: "%9", Team: "ghost", Session: "s"},
				{WindowID: "@1", PaneID: "%0", Team: "", Session: "s"},
			},
			live: map[string]bool{},
			want: []ReapTarget{{Window: "@6", Team: "ghost"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanTeamReap(tc.panes, tc.live)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PlanTeamReap:\n got:  %v\n want: %v", got, tc.want)
			}
		})
	}
}

func TestKillWindowCommands(t *testing.T) {
	if got := KillWindowCommands(nil); got != nil {
		t.Fatalf("empty plan should yield nil commands, got %v", got)
	}
	targets := []ReapTarget{{Window: "@6", Team: "ghost"}, {Window: "@7", Team: "old"}}
	want := []Command{
		cmd("kill-window", "-t", "@6"),
		cmd("kill-window", "-t", "@7"),
	}
	if got := KillWindowCommands(targets); !reflect.DeepEqual(got, want) {
		t.Fatalf("KillWindowCommands:\n got:  %v\n want: %v", got, want)
	}
}
