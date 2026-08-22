package layout

import (
	"reflect"
	"testing"
)

func TestPlanLogs(t *testing.T) {
	cfg := paneCfg()
	base := LogsView{
		Window:   Window{ID: "@1", Session: "api", Panes: []Pane{pSidebar, pShell, pAgent}},
		RunnerUp: true, AttachCommand: "exec zdevd pane logs-attach api",
	}
	wantOpen := []Command{
		cmd("split-window", "-v", "-l", itoa(cfg.Rows), "-t", "%1", base.AttachCommand),
		cmd("set-option", "-p", LogsOption, "api"),
		cmd("select-pane", "-T", "logs · runner"),
		cmd("select-pane", "-t", "%1"),
	}
	if got := PlanLogs(base, cfg); !reflect.DeepEqual(got, wantOpen) {
		t.Fatalf("open plan = %v, want %v", got, wantOpen)
	}
	for _, mutate := range []func(*LogsView){
		func(v *LogsView) { v.RunnerUp = false },
		func(v *LogsView) { v.Suppressed = true },
		func(v *LogsView) { v.Anchored = true },
		func(v *LogsView) { v.AttachCommand = "" },
	} {
		v := base
		mutate(&v)
		if got := PlanLogs(v, cfg); got != nil {
			t.Errorf("vetoed open = %v", got)
		}
	}

	logs := Pane{ID: "%8", Height: 8, LogsOpt: "api", Title: "logs · runner"}
	down := base
	down.RunnerUp = false
	down.Window.Panes = append(down.Window.Panes, logs)
	wantKill := []Command{cmd("kill-pane", "-t", "%8")}
	if got := PlanLogs(down, cfg); !reflect.DeepEqual(got, wantKill) {
		t.Errorf("down plan = %v, want %v", got, wantKill)
	}
	down.Window.Panes[len(down.Window.Panes)-1].Active = true
	if got := PlanLogs(down, cfg); got != nil {
		t.Errorf("active logs pane must survive: %v", got)
	}
	down.Anchored = true
	if got := PlanLogs(down, cfg); got != nil {
		t.Errorf("anchor must freeze teardown: %v", got)
	}
}

func TestPlanLogsOnlyKillsTaggedPane(t *testing.T) {
	v := LogsView{Window: Window{ID: "@1", Session: "api", Panes: []Pane{pShell, pAgent}}}
	for _, c := range PlanLogs(v, paneCfg()) {
		if c.Args[0] == "kill-pane" {
			t.Fatalf("killed untagged pane: %v", c)
		}
	}
}
