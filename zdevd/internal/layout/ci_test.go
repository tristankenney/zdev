package layout

import (
	"reflect"
	"testing"
)

func TestPlanCIAndSuppression(t *testing.T) {
	cfg := paneCfg()
	base := CIView{Window: Window{ID: "@1", Session: "api", Panes: []Pane{pShell, pAgent}}, Failing: true, AttachCommand: "exec zdevd pane ci-attach api"}
	want := []Command{
		cmd("split-window", "-v", "-l", itoa(cfg.Rows), "-t", "%1", base.AttachCommand),
		cmd("set-option", "-p", CIOption, "api"),
		cmd("select-pane", "-T", "ci · failing"),
		cmd("select-pane", "-t", "%1"),
	}
	if got := PlanCI(base, cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("open = %v, want %v", got, want)
	}
	base.Suppressed = true
	if got := PlanCI(base, cfg); got != nil {
		t.Fatalf("suppressed open = %v", got)
	}
	ci := Pane{ID: "%8", CIOpt: "api", Height: 8}
	base.Window.Panes = append(base.Window.Panes, ci)
	base.Failing = false
	base.Suppressed = false
	if got := PlanCI(base, cfg); !reflect.DeepEqual(got, []Command{cmd("kill-pane", "-t", "%8")}) {
		t.Fatalf("clear = %v", got)
	}
}

func TestPlanRowBudgetEvictionOrder(t *testing.T) {
	cfg := paneCfg()
	shell := pShell
	shell.Height = cfg.DonorFloorRows
	logs := Pane{ID: "%7", LogsOpt: "api", Height: cfg.Rows}
	ci := Pane{ID: "%8", CIOpt: "api", Height: cfg.Rows}
	v := RowBudgetView{Window: Window{ID: "@1", Session: "api", Panes: []Pane{shell, pAgent, logs, ci}}, WantAgent: true, WantLogs: true, WantCI: true}
	if got := PlanRowBudget(v, cfg); !reflect.DeepEqual(got, []Command{cmd("kill-pane", "-t", "%8")}) {
		t.Fatalf("first eviction = %v", got)
	}
	v.Window.Panes = []Pane{shell, pAgent, logs}
	if got := PlanRowBudget(v, cfg); !reflect.DeepEqual(got, []Command{cmd("kill-pane", "-t", "%7")}) {
		t.Fatalf("second eviction = %v", got)
	}
	logs.Active = true
	v.Window.Panes = []Pane{shell, pAgent, logs}
	if got := PlanRowBudget(v, cfg); got != nil {
		t.Fatalf("must not evict active logs: %v", got)
	}
}
