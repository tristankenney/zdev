package layout

import (
	"reflect"
	"testing"
)

const paneNow = int64(1_700_000_000)

func paneCfg() PaneConfig {
	c := DefaultPaneConfig()
	c.Enabled = true
	return c
}

const attachCmd = "exec zdevd pane attach api"

var (
	pShell   = Pane{ID: "%1", Width: 100, Height: 40}
	pAgent   = Pane{ID: "%2", Width: 100, Height: 40, Title: "● claude", Agent: true}
	pSidebar = Pane{ID: "%0", Width: 50, Height: 40, SidebarOpt: true, Title: SidebarTitle}
)

// viewport is an already-open agent pane for session "api".
func viewport(active bool, title string) Pane {
	return Pane{ID: "%9", Width: 100, Height: 8, PaneOpt: "api", Title: title, Active: active}
}

func paneView(panes ...Pane) PaneView {
	return PaneView{
		Window:        Window{ID: "@1", Session: "api", EffectiveWidth: 250, Panes: panes},
		Requested:     true,
		Title:         "tests",
		TurnLive:      true,
		AttachCommand: attachCmd,
	}
}

func openCmd(donor string, rows int) []Command {
	return []Command{cmd("split-window", "-d", "-v", "-l", itoa(rows), "-t", donor, attachCmd)}
}

func TestPlanPanes(t *testing.T) {
	cases := []struct {
		name string
		v    PaneView
		cfg  PaneConfig
		want []Command
	}{
		{
			name: "disabled plans nothing",
			v:    paneView(pSidebar, pShell, pAgent),
			cfg:  DefaultPaneConfig(),
			want: nil,
		},
		{
			name: "request during a live turn opens a detached row off the shell",
			v:    paneView(pSidebar, pShell, pAgent),
			cfg:  paneCfg(),
			want: openCmd("%1", DefaultPaneRows),
		},
		{
			name: "no request: nothing opens",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.Requested = false
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "turn over: a request alone does not earn a pane",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.TurnLive = false
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "operator veto outranks a live request",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.Vetoed = true
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "the agent pane is never the donor",
			// Only an agent and a sidebar — no shell to take rows from.
			v:    paneView(pSidebar, pAgent),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "donor too short to survive the split: refused",
			v: func() PaneView {
				short := pShell
				short.Height = DefaultPaneRows + DefaultDonorFloorRows // one short of the +1 border
				return paneView(pSidebar, short, pAgent)
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "tallest qualifying donor wins",
			v: func() PaneView {
				tall := Pane{ID: "%7", Width: 100, Height: 60}
				return paneView(pSidebar, pShell, tall, pAgent)
			}(),
			cfg:  paneCfg(),
			want: openCmd("%7", DefaultPaneRows),
		},
		{
			name: "already open, title unchanged: idempotent no-op",
			v:    paneView(pSidebar, pShell, pAgent, viewport(false, "api · tests")),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "re-request with a new title relabels rather than reopening",
			v:    paneView(pSidebar, pShell, pAgent, viewport(false, "api · old")),
			cfg:  paneCfg(),
			want: []Command{cmd("select-pane", "-t", "%9", "-T", "api · tests")},
		},
		{
			name: "turn ends, pane unwatched: retired",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent, viewport(false, "api · tests"))
				v.TurnLive = false
				return v
			}(),
			cfg:  paneCfg(),
			want: []Command{cmd("kill-pane", "-t", "%9")},
		},
		{
			name: "turn ends while the operator is IN it: demoted, never killed",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent, viewport(true, "api · tests"))
				v.TurnLive = false
				return v
			}(),
			cfg:  paneCfg(),
			want: []Command{cmd("select-pane", "-t", "%9", "-T", "api · tests · ended")},
		},
		{
			name: "already demoted and still watched: no repeat relabel",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent, viewport(true, "api · tests · ended"))
				v.TurnLive = false
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "veto with a pane open retires it",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent, viewport(false, "api · tests"))
				v.Vetoed = true
				return v
			}(),
			cfg:  paneCfg(),
			want: []Command{cmd("kill-pane", "-t", "%9")},
		},
		{
			name: "zoomed window is untouchable",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.Window.Zoomed = true
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "copy-mode in the window is untouchable",
			v: func() PaneView {
				inMode := pShell
				inMode.InMode = true
				v := paneView(pSidebar, inMode, pAgent)
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "the watcher session is never decorated",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.Window.Session = WatcherSession
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "a teammate window is never decorated",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.Window.TeamWindow = true
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "no attach command resolved: refuse rather than open a blank pane",
			v: func() PaneView {
				v := paneView(pSidebar, pShell, pAgent)
				v.AttachCommand = ""
				return v
			}(),
			cfg:  paneCfg(),
			want: nil,
		},
		{
			name: "an existing viewport is never its own donor",
			// A stale viewport plus no shell: nothing to split, and the
			// viewport must not be recursively split.
			v: func() PaneView {
				vp := viewport(false, "api · tests")
				vp.Height = 60
				v := PaneView{
					Window:        Window{ID: "@1", Session: "api", EffectiveWidth: 250, Panes: []Pane{pAgent, vp}},
					Requested:     true,
					Title:         "tests",
					TurnLive:      true,
					AttachCommand: attachCmd,
				}
				return v
			}(),
			cfg: paneCfg(),
			// It already has one and the title matches → no-op.
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanPanes(tc.v, tc.cfg, paneNow)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlanPanes mismatch\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// The only pane this planner may ever kill is one it tagged itself, and it must
// never kill a pane the operator is sitting in.
func TestPlanPanesOnlyKillsItsOwnUnwatchedPanes(t *testing.T) {
	views := []PaneView{
		// Untagged panes everywhere, no request: must emit nothing at all.
		{Window: Window{ID: "@1", Session: "api", Panes: []Pane{pSidebar, pShell, pAgent}}},
		// Tagged and watched, turn over.
		func() PaneView {
			v := paneView(pShell, pAgent, viewport(true, "api · tests"))
			v.TurnLive = false
			return v
		}(),
		// Vetoed while watched — the veto still must not kill under them.
		func() PaneView {
			v := paneView(pShell, pAgent, viewport(true, "api · tests"))
			v.Vetoed = true
			return v
		}(),
	}
	for i, v := range views {
		for _, c := range PlanPanes(v, paneCfg(), paneNow) {
			if c.Args[0] != "kill-pane" {
				continue
			}
			target := c.Args[2]
			var killed Pane
			for _, p := range v.Window.Panes {
				if p.ID == target {
					killed = p
				}
			}
			if killed.PaneOpt == "" {
				t.Errorf("view %d: killed an untagged pane %s", i, target)
			}
			if killed.Active {
				t.Errorf("view %d: killed pane %s while the operator was in it", i, target)
			}
		}
	}
}

// Applying a plan and re-planning against the world it produced must converge.
func TestPlanPanesConverges(t *testing.T) {
	v := paneView(pSidebar, pShell, pAgent)
	first := PlanPanes(v, paneCfg(), paneNow)
	if len(first) == 0 {
		t.Fatal("expected an open plan")
	}
	// The pane self-tags and titles itself, so the settled world has both.
	v.Window.Panes = append(v.Window.Panes, viewport(false, "api · tests"))
	if got := PlanPanes(v, paneCfg(), paneNow); got != nil {
		t.Errorf("second pass should be a no-op, got %v", got)
	}
}

// The turn gate must end a turn only on POSITIVE evidence. A long quiet turn
// reads as idle (AttWorking decays at 180s), and inferring "over" from that
// would retire a pane the operator was reading — found on the first real-fleet
// run, 2026-08-22.
func TestPlanPanesAgeBackstop(t *testing.T) {
	cfg := paneCfg()

	fresh := paneView(pSidebar, pShell, pAgent)
	fresh.RequestedTS = paneNow - 60
	if got := PlanPanes(fresh, cfg, paneNow); len(got) == 0 {
		t.Error("a fresh request should still open a pane")
	}

	// An unknown request age must NOT be treated as stale.
	unknown := paneView(pSidebar, pShell, pAgent)
	unknown.RequestedTS = 0
	if got := PlanPanes(unknown, cfg, paneNow); len(got) == 0 {
		t.Error("an unknown request age must not retire the pane")
	}

	// Just inside the backstop.
	edge := paneView(pSidebar, pShell, pAgent)
	edge.RequestedTS = paneNow - int64(cfg.MaxAgeSec)
	if got := PlanPanes(edge, cfg, paneNow); len(got) == 0 {
		t.Error("a request exactly at MaxAgeSec is not yet stale")
	}

	// Past it: refused, and an open pane is retired.
	stale := paneView(pSidebar, pShell, pAgent)
	stale.RequestedTS = paneNow - int64(cfg.MaxAgeSec) - 1
	if got := PlanPanes(stale, cfg, paneNow); got != nil {
		t.Errorf("a stale request must not open a pane, got %v", got)
	}
	staleOpen := paneView(pSidebar, pShell, pAgent, viewport(false, "api · tests"))
	staleOpen.RequestedTS = paneNow - int64(cfg.MaxAgeSec) - 1
	want := []Command{cmd("kill-pane", "-t", "%9")}
	if got := PlanPanes(staleOpen, cfg, paneNow); !reflect.DeepEqual(got, want) {
		t.Errorf("stale request should retire the pane\n got: %v\nwant: %v", got, want)
	}

	// MaxAgeSec 0 disables the backstop entirely.
	off := cfg
	off.MaxAgeSec = 0
	ancient := paneView(pSidebar, pShell, pAgent)
	ancient.RequestedTS = 1
	if got := PlanPanes(ancient, off, paneNow); len(got) == 0 {
		t.Error("MaxAgeSec=0 must disable the age backstop")
	}
}
