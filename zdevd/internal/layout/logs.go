package layout

// LogsOption tags the inferred runner logs pane. The value is the owning
// session; only panes carrying this option may be retired by PlanLogs.
const LogsOption = "@zdev-logs"

// LogsView is the complete observed world for one inferred logs row.
type LogsView struct {
	Window        Window
	RunnerUp      bool
	Suppressed    bool
	Anchored      bool
	AttachCommand string
}

func (v LogsView) logsPane() (Pane, bool) {
	for _, p := range v.Window.Panes {
		if p.LogsOpt != "" {
			return p, true
		}
	}
	return Pane{}, false
}

// PlanLogs reconciles the phase-3 inferred runner logs pane. Anchoring freezes
// topology: it neither opens nor tears down an existing row.
func PlanLogs(v LogsView, cfg PaneConfig) []Command {
	if !cfg.Enabled || v.Anchored || v.Window.ID == "" ||
		v.Window.Session == WatcherSession || v.Window.TeamWindow ||
		v.Window.Zoomed || v.Window.anyPaneInMode() {
		return nil
	}
	p, have := v.logsPane()
	want := v.RunnerUp && !v.Suppressed && v.AttachCommand != ""
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
		// Unlike agent viewports, inferred panes are tagged in the same tmux
		// batch that creates them. split-window selects the new pane, the next
		// two commands therefore target it, and the final command restores the
		// operator's cursor before tmux returns.
		return []Command{
			cmd("split-window", "-v", "-l", itoa(cfg.Rows), "-t", d.ID, v.AttachCommand),
			cmd("set-option", "-p", LogsOption, v.Window.Session),
			cmd("select-pane", "-T", "logs · runner"),
			cmd("select-pane", "-t", active),
		}
	}
	return nil
}

func logsDonor(w Window, cfg PaneConfig) (Pane, bool) {
	var best Pane
	found := false
	for _, p := range w.Panes {
		if p.isSidebar() || p.Agent || p.PaneOpt != "" || p.LogsOpt != "" {
			continue
		}
		if p.Height-cfg.Rows-1 < cfg.DonorFloorRows {
			continue
		}
		if !found || p.Height > best.Height {
			best, found = p, true
		}
	}
	return best, found
}
