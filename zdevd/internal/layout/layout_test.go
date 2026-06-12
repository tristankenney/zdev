package layout

import (
	"strings"
	"testing"
)

// renderPlan flattens a []Command into one `;`-joined string per command for
// compact golden comparison: "split-window -hb ...|set-option -p ...|...".
func renderPlan(cmds []Command) string {
	lines := make([]string, len(cmds))
	for i, c := range cmds {
		lines[i] = strings.Join(c.Args, " ")
	}
	return strings.Join(lines, "\n")
}

// pane is a terse Pane constructor for tables: id, left, width.
func pane(id string, left, width int) Pane {
	return Pane{ID: id, Left: left, Width: width, Height: 50}
}

func TestPlan(t *testing.T) {
	cfg := DefaultConfig() // 160 / 50 / 30

	tests := []struct {
		name string
		w    Window
		cfg  Config
		want string
	}{
		{
			name: "empty window id is a no-op",
			w:    Window{ID: "", EffectiveWidth: 300},
			want: "",
		},
		{
			name: "watcher session is never decorated",
			w: Window{
				ID: "@1", Session: WatcherSession, EffectiveWidth: 300,
				Panes: []Pane{pane("%0", 0, 300)},
			},
			want: "",
		},
		{
			name: "hysteresis dead band leaves state untouched (no sidebar)",
			// 140 is below threshold (160) but above floor (130) → no-op.
			w: Window{
				ID: "@1", EffectiveWidth: 140,
				Panes: []Pane{pane("%0", 0, 140)},
			},
			want: "",
		},
		{
			name: "hysteresis dead band leaves an existing sidebar in place",
			w: Window{
				ID: "@1", EffectiveWidth: 140,
				Panes: []Pane{
					{ID: "%0", Left: 0, Width: 50, SidebarOpt: true},
					pane("%1", 51, 89),
				},
			},
			want: "",
		},
		{
			name: "below floor kills the sidebar and evens out",
			w: Window{
				ID: "@1", EffectiveWidth: 120,
				Panes: []Pane{
					{ID: "%0", Left: 0, Width: 50, SidebarOpt: true},
					pane("%1", 51, 69),
				},
			},
			want: strings.Join([]string{
				"kill-pane -t %0",
				"select-layout -t @1 even-horizontal",
			}, "\n"),
		},
		{
			name: "below floor with no sidebar just normalizes",
			w: Window{
				ID: "@1", EffectiveWidth: 120,
				Panes: []Pane{pane("%0", 0, 60), pane("%1", 61, 59)},
			},
			want: "select-layout -t @1 even-horizontal",
		},
		{
			name: "wide, no sidebar, single pane: create + restore focus, no rebalance",
			// One non-sidebar column → n<2 → only the sidebar is pinned.
			w: Window{
				ID: "@1", EffectiveWidth: 200, SidebarCommand: "exec render",
				Panes: []Pane{{ID: "%0", Left: 0, Width: 200, Active: true}},
			},
			want: strings.Join([]string{
				"split-window -hb -l 50 -t %0 -P -F #{pane_id} exec render",
				"set-option -p @is-sidebar 1",
				"select-pane -T zdev-sidebar",
				"resize-pane -x 50",
				"select-pane -t %0",
			}, "\n"),
		},
		{
			name: "wide, no sidebar, two panes: create, rebalance one column, restore focus",
			// Two columns → resize all but the last. each=(200-50-2)/2=74.
			w: Window{
				ID: "@1", EffectiveWidth: 200, SidebarCommand: "exec render",
				Panes: []Pane{
					{ID: "%0", Left: 0, Width: 100, Active: true},
					pane("%1", 101, 100),
				},
			},
			want: strings.Join([]string{
				"split-window -hb -l 50 -t %0 -P -F #{pane_id} exec render",
				"set-option -p @is-sidebar 1",
				"select-pane -T zdev-sidebar",
				"resize-pane -x 50",
				"resize-pane -t %0 -x 74",
				"select-pane -t %0",
			}, "\n"),
		},
		{
			name: "wide, sidebar present: rebalance only around the known sidebar id",
			// each=(351-50-2)/2 = 149 → resize the first column rep only.
			w: Window{
				ID: "@1", EffectiveWidth: 351,
				Panes: []Pane{
					{ID: "%9", Left: 0, Width: 50, SidebarOpt: true},
					pane("%0", 51, 150),
					pane("%1", 202, 149),
				},
			},
			want: strings.Join([]string{
				"resize-pane -t %9 -x 50",
				"resize-pane -t %0 -x 149",
			}, "\n"),
		},
		{
			name: "title-only sidebar (no @is-sidebar option) is recognized",
			w: Window{
				ID: "@1", EffectiveWidth: 200,
				Panes: []Pane{
					{ID: "%9", Left: 0, Width: 50, Title: SidebarTitle},
					pane("%0", 51, 75),
					pane("%1", 127, 73),
				},
			},
			// each=(200-50-2)/2=74 → resize first column only.
			want: strings.Join([]string{
				"resize-pane -t %9 -x 50",
				"resize-pane -t %0 -x 74",
			}, "\n"),
		},
		{
			name: "duplicate sidebars are reaped down to the first, then rebalanced",
			w: Window{
				ID: "@1", EffectiveWidth: 351,
				Panes: []Pane{
					{ID: "%9", Left: 0, Width: 50, SidebarOpt: true},
					{ID: "%8", Left: 51, Width: 50, SidebarOpt: true},
					pane("%0", 102, 150),
					pane("%1", 253, 99),
				},
			},
			// Kill %8 (the second sidebar); pin %9; resize first column.
			// each=(351-50-2)/2 = 149.
			want: strings.Join([]string{
				"kill-pane -t %8",
				"resize-pane -t %9 -x 50",
				"resize-pane -t %0 -x 149",
			}, "\n"),
		},
		{
			name: "column-aware rebalance: a vertical teammate stack is one column",
			// THE 2026-06-11 dogfood bug. Window 351 wide. Sidebar(50) +
			// a shell strip / main pane in the middle column (left 51) +
			// a 3-high teammate stack sharing left 252. Naive per-pane
			// equalization would have made each=(351-50-4)/4≈74 and
			// crushed the stack; column-aware makes n=2 (two distinct
			// non-sidebar lefts), each=(351-50-2)/2=149, resize the middle
			// column only, the stack column absorbs the remainder.
			w: Window{
				ID: "@1", EffectiveWidth: 351,
				Panes: []Pane{
					{ID: "%sb", Left: 0, Width: 50, SidebarOpt: true},
					pane("%main", 51, 200),
					{ID: "%t1", Left: 252, Top: 0, Width: 99, Height: 16},
					{ID: "%t2", Left: 252, Top: 17, Width: 99, Height: 16},
					{ID: "%t3", Left: 252, Top: 34, Width: 99, Height: 16},
				},
			},
			want: strings.Join([]string{
				"resize-pane -t %sb -x 50",
				"resize-pane -t %main -x 149",
			}, "\n"),
		},
		{
			name: "rebalance skips per-column resize when each would be < 10",
			// Threshold lowered so a very narrow-but-eligible window still
			// enters the wide branch; each=(60-50-2)/2=4 (<10) → pin only.
			cfg: Config{Threshold: 55, Width: 50, Hysteresis: 5},
			w: Window{
				ID: "@1", EffectiveWidth: 60,
				Panes: []Pane{
					{ID: "%9", Left: 0, Width: 50, SidebarOpt: true},
					pane("%0", 51, 5),
					pane("%1", 56, 4),
				},
			},
			want: "resize-pane -t %9 -x 50",
		},
		{
			name: "create skips focus restore when no pane is active",
			w: Window{
				ID: "@1", EffectiveWidth: 200, SidebarCommand: "exec render",
				Panes: []Pane{pane("%0", 0, 200)},
			},
			want: strings.Join([]string{
				"split-window -hb -l 50 -t %0 -P -F #{pane_id} exec render",
				"set-option -p @is-sidebar 1",
				"select-pane -T zdev-sidebar",
				"resize-pane -x 50",
			}, "\n"),
		},
		{
			name: "split targets the leftmost pane, not input order",
			w: Window{
				ID: "@1", EffectiveWidth: 200, SidebarCommand: "exec render",
				Panes: []Pane{
					pane("%right", 101, 100),
					{ID: "%left", Left: 0, Width: 100, Active: true},
				},
			},
			want: strings.Join([]string{
				"split-window -hb -l 50 -t %left -P -F #{pane_id} exec render",
				"set-option -p @is-sidebar 1",
				"select-pane -T zdev-sidebar",
				"resize-pane -x 50",
				// columns sorted by left: %left(0) then %right(101);
				// resize all but last → %left only. each=(200-50-2)/2=74.
				"resize-pane -t %left -x 74",
				"select-pane -t %left",
			}, "\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cfg
			if c == (Config{}) {
				c = cfg
			}
			got := renderPlan(Plan(tc.w, c))
			if got != tc.want {
				t.Errorf("Plan mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{
			name: "all unset → defaults",
			env:  map[string]string{},
			want: Config{Threshold: 160, Width: 50, Hysteresis: 30},
		},
		{
			name: "all set → honored",
			env:  map[string]string{"ZDEV_SIDEBAR_THRESHOLD": "200", "ZDEV_SIDEBAR_WIDTH": "60", "ZDEV_SIDEBAR_HYSTERESIS": "40"},
			want: Config{Threshold: 200, Width: 60, Hysteresis: 40},
		},
		{
			name: "explicit zero hysteresis is honored; zero width/threshold fall back",
			env:  map[string]string{"ZDEV_SIDEBAR_HYSTERESIS": "0", "ZDEV_SIDEBAR_WIDTH": "0", "ZDEV_SIDEBAR_THRESHOLD": "0"},
			want: Config{Threshold: 160, Width: 50, Hysteresis: 0},
		},
		{
			name: "garbage values fall back to defaults",
			env:  map[string]string{"ZDEV_SIDEBAR_THRESHOLD": "wide", "ZDEV_SIDEBAR_WIDTH": "-5"},
			want: Config{Threshold: 160, Width: 50, Hysteresis: 30},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }
			got := ConfigFromEnv(lookup)
			if got != tc.want {
				t.Errorf("ConfigFromEnv = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPlan_TeamWindowNeverDecorated pins the teammate-window exclusion: a
// window created by team-sweep (TeamWindow, via the @zdev-team tag) must
// never receive a sidebar — found by the first production reap, where a
// toggle-added sidebar pane kept a dead member window alive past its team.
func TestPlan_TeamWindowNeverDecorated(t *testing.T) {
	w := Window{
		ID: "@19", Session: "zdev", EffectiveWidth: 300, TeamWindow: true,
		Panes:          []Pane{{ID: "%57", Width: 300, Height: 50, Title: "✳ general-purpose"}},
		SidebarCommand: "exec render",
	}
	if got := Plan(w, DefaultConfig()); got != nil {
		t.Fatalf("Plan decorated a teammate window: %v", got)
	}
}
