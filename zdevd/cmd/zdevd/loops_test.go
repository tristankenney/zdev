package main

import "testing"

// assembleRuns + runDisplayState are the report's spec: runs group by id,
// terminals win, absence never reads as success.
func TestAssembleRunsAndDisplayState(t *testing.T) {
	now := int64(1_786_100_000)
	events := []loopEvent{
		// run A: full lifecycle to success at iter 2
		{Ts: now - 900, Run: "A", Session: "zdev", Event: "start", Ticket: "DEMO-70", Goal: "region type", Max: 6, Resume: 1},
		{Ts: now - 800, Run: "A", Session: "zdev", Event: "iter", Iter: 1, CheckExit: 1, Strikes: 1},
		{Ts: now - 700, Run: "A", Session: "zdev", Event: "terminal", State: "success", Iter: 2},
		// run B: stalled
		{Ts: now - 600, Run: "B", Session: "pay-app", Event: "start", Goal: "flag wiring", Max: 4},
		{Ts: now - 500, Run: "B", Session: "pay-app", Event: "iter", Iter: 1, CheckExit: 1, Strikes: 1},
		{Ts: now - 450, Run: "B", Session: "pay-app", Event: "iter", Iter: 2, CheckExit: 1, Strikes: 2},
		{Ts: now - 440, Run: "B", Session: "pay-app", Event: "terminal", State: "stalled", Iter: 2},
		// run C: started recently, no terminal → running
		{Ts: now - 60, Run: "C", Session: "zdev", Event: "start", Goal: "live", Max: 3},
		// run D: started long ago, no terminal → abandoned?
		{Ts: now - 3*86400, Run: "D", Session: "zdev", Event: "start", Goal: "old", Max: 3},
	}
	runs := assembleRuns(events)
	if len(runs) != 4 {
		t.Fatalf("assembled %d runs, want 4", len(runs))
	}
	// ordered by start ts: D, A, B, C
	if runs[0].Run != "D" || runs[3].Run != "C" {
		t.Fatalf("order wrong: %v %v %v %v", runs[0].Run, runs[1].Run, runs[2].Run, runs[3].Run)
	}
	a := runs[1]
	if a.Ticket != "DEMO-70" || a.Max != 6 || !a.Resume || a.Iters != 2 || a.Terminal != "success" {
		t.Errorf("run A assembled wrong: %+v", a)
	}
	if got := runDisplayState(runs[2], now); got != "stalled" {
		t.Errorf("run B display = %q, want stalled", got)
	}
	if got := runDisplayState(runs[3], now); got != "running" {
		t.Errorf("run C display = %q, want running", got)
	}
	if got := runDisplayState(runs[0], now); got != "abandoned?" {
		t.Errorf("run D display = %q, want abandoned? — absence must never read as success", got)
	}
}
