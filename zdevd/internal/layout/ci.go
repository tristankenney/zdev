package layout

const CIOption = "@zdev-ci"

type CIView struct {
	Window        Window
	Failing       bool
	Suppressed    bool
	Anchored      bool
	AttachCommand string
}

func (v CIView) ciPane() (Pane, bool) {
	for _, p := range v.Window.Panes {
		if p.CIOpt != "" {
			return p, true
		}
	}
	return Pane{}, false
}

// PlanCI reconciles the lowest-priority inferred row. Budget eviction is
// handled separately by PlanRowBudget so this planner only owns CI lifecycle.
func PlanCI(v CIView, cfg PaneConfig) []Command {
	if !cfg.Enabled || v.Anchored || v.Window.ID == "" || v.Window.Session == WatcherSession ||
		v.Window.TeamWindow || v.Window.Zoomed || v.Window.anyPaneInMode() {
		return nil
	}
	p, have := v.ciPane()
	want := v.Failing && !v.Suppressed && v.AttachCommand != ""
	if have && !want {
		if p.Active {
			return nil
		}
		return []Command{cmd("kill-pane", "-t", p.ID)}
	}
	if !have && want {
		d, ok := logsDonor(v.Window, cfg)
		if !ok {
			return nil
		}
		active := activePaneID(v.Window.Panes)
		if active == "" {
			active = d.ID
		}
		return []Command{
			cmd("split-window", "-v", "-l", itoa(cfg.Rows), "-t", d.ID, v.AttachCommand),
			cmd("set-option", "-p", CIOption, v.Window.Session),
			cmd("select-pane", "-T", "ci · failing"),
			cmd("select-pane", "-t", active),
		}
	}
	return nil
}

// RowBudgetView describes demand in priority order: requested agent viewport,
// runner logs, then CI. The first two may evict lower-priority existing rows
// when the shell donor cannot fund their split.
type RowBudgetView struct {
	Window    Window
	WantAgent bool
	WantLogs  bool
	WantCI    bool
}

func PlanRowBudget(v RowBudgetView, cfg PaneConfig) []Command {
	if !cfg.Enabled || v.Window.Zoomed || v.Window.anyPaneInMode() {
		return nil
	}
	var shell Pane
	found := false
	var agent, logs, ci *Pane
	for i := range v.Window.Panes {
		p := &v.Window.Panes[i]
		switch {
		case p.PaneOpt != "":
			agent = p
		case p.LogsOpt != "":
			logs = p
		case p.CIOpt != "":
			ci = p
		case !p.isSidebar() && !p.Agent && (!found || p.Height > shell.Height):
			shell, found = *p, true
		}
	}
	if !found {
		return nil
	}
	kill := func(p *Pane) []Command {
		if p == nil || p.Active {
			return nil
		}
		return []Command{cmd("kill-pane", "-t", p.ID)}
	}
	// A resize may leave the donor below its invariant even before new demand.
	if shell.Height < cfg.DonorFloorRows {
		if x := kill(ci); x != nil {
			return x
		}
		if x := kill(logs); x != nil {
			return x
		}
	}
	canSplit := shell.Height-cfg.Rows-1 >= cfg.DonorFloorRows
	if v.WantAgent && agent == nil && !canSplit {
		if x := kill(ci); x != nil {
			return x
		}
		if x := kill(logs); x != nil {
			return x
		}
	}
	if v.WantLogs && logs == nil && !canSplit {
		if x := kill(ci); x != nil {
			return x
		}
	}
	return nil
}
