package main

// zdevd loops — the loop ledger report (loop-layer phase 4, proposal r2 T1).
//
// Reads ~/.local/state/zdev/loops/*.jsonl (written by bin/zdev-loop: one
// start event per run, one per red iteration, one terminal) and answers
// "how are my loops doing against their goals": per-run rows with ticket,
// terminal state, iterations used, and a summary with the RQ2 numbers —
// runs by terminal state and iterations-per-success. No daemon required;
// missing directory prints nothing (LOG-04 posture, same as history/stops).

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tristankenney/zdev/zdevd/internal/render"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// loopEvent is one ledger line. Fields are a union across the three event
// kinds; absent fields stay zero-valued.
type loopEvent struct {
	Ts        int64  `json:"ts"`
	Run       string `json:"run"`
	Session   string `json:"session"`
	Event     string `json:"event"` // start | iter | terminal
	Dir       string `json:"dir,omitempty"`
	Ticket    string `json:"ticket,omitempty"`
	Goal      string `json:"goal,omitempty"`
	Check     string `json:"check,omitempty"`
	Max       int    `json:"max,omitempty"`
	Resume    int    `json:"resume,omitempty"`
	Iter      int    `json:"iter,omitempty"`
	CheckExit int    `json:"check_exit,omitempty"`
	Strikes   int    `json:"strikes,omitempty"`
	State     string `json:"state,omitempty"` // terminal only
}

// loopRun is one assembled run: a start event plus whatever followed.
type loopRun struct {
	Run      string
	Session  string
	Ticket   string
	Goal     string
	Max      int
	Resume   bool
	StartTs  int64
	LastTs   int64
	Iters    int
	Terminal string // "" while running/abandoned
}

// assembleRuns groups ledger events into runs, ordered by start time.
// Pure function of its input — the report's testable core. Events without
// a start (truncated file, pre-v2 lines that fail to parse are already
// dropped by the caller) still produce a run row so nothing is silently
// hidden; they just lack goal/ticket metadata.
func assembleRuns(events []loopEvent) []loopRun {
	byRun := map[string]*loopRun{}
	order := []string{}
	get := func(ev loopEvent) *loopRun {
		r, ok := byRun[ev.Run]
		if !ok {
			r = &loopRun{Run: ev.Run, Session: ev.Session, StartTs: ev.Ts}
			byRun[ev.Run] = r
			order = append(order, ev.Run)
		}
		return r
	}
	for _, ev := range events {
		r := get(ev)
		if ev.Ts > r.LastTs {
			r.LastTs = ev.Ts
		}
		switch ev.Event {
		case "start":
			r.StartTs = ev.Ts
			r.Ticket = ev.Ticket
			r.Goal = ev.Goal
			r.Max = ev.Max
			r.Resume = ev.Resume == 1
		case "iter":
			if ev.Iter > r.Iters {
				r.Iters = ev.Iter
			}
		case "terminal":
			r.Terminal = ev.State
			if ev.Iter > r.Iters {
				r.Iters = ev.Iter
			}
		}
	}
	out := make([]loopRun, 0, len(order))
	for _, k := range order {
		out = append(out, *byRun[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartTs < out[j].StartTs })
	return out
}

// runDisplayState renders a run's state for the report. A run with no
// terminal is "running" if its last event is fresh, else "abandoned?" —
// absence must never read as success (the ledger inherits the spec's rule).
func runDisplayState(r loopRun, now int64) string {
	if r.Terminal != "" {
		return r.Terminal
	}
	if now-r.LastTs < int64((2 * time.Hour).Seconds()) {
		return "running"
	}
	return "abandoned?"
}

// loopsSubcmd implements `zdevd loops [--dir PATH] [--days N] [--json]`.
func loopsSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd loops", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	defDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "zdev", "loops")
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		defDir = filepath.Join(v, "zdev", "loops")
	}
	dir := fs.String("dir", defDir, "loops ledger directory")
	days := fs.Int("days", 14, "how many days back to include")
	asJSON := fs.Bool("json", false, "emit assembled runs as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	paths, _ := filepath.Glob(filepath.Join(*dir, "*.jsonl"))
	cutoff := time.Now().AddDate(0, 0, -*days).Unix()
	var events []loopEvent
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var ev loopEvent
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue // pre-v2 TSV or malformed — skip silently
			}
			if ev.Ts >= cutoff && ev.Run != "" {
				events = append(events, ev)
			}
		}
		f.Close()
	}

	runs := assembleRuns(events)
	if len(runs) == 0 {
		fmt.Printf("no loop runs in the last %d days\n", *days)
		return 0
	}

	now := time.Now().Unix()
	if *asJSON {
		type jsonRun struct {
			loopRun
			DisplayState string `json:"display_state"`
		}
		out := make([]jsonRun, len(runs))
		for i, r := range runs {
			out[i] = jsonRun{r, runDisplayState(r, now)}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	}

	counts := map[string]int{}
	var successIters []int
	for _, r := range runs {
		st := runDisplayState(r, now)
		counts[st]++
		if st == "success" {
			// iters can be 0 when success came without a red iteration
			// (first turn went green); count it as 1 agent turn.
			it := r.Iters
			if it == 0 {
				it = 1
			}
			successIters = append(successIters, it)
		}
	}

	fmt.Printf("loops, last %d days (runs=%d)\n\n", *days, len(runs))
	for _, st := range []string{"success", "no-op", "stalled", "exhausted", "blocked", "running", "abandoned?"} {
		if counts[st] > 0 {
			fmt.Printf("  %-11s %d\n", st, counts[st])
		}
	}
	if len(successIters) > 0 {
		sort.Ints(successIters)
		fmt.Printf("\n  iterations per success: median %d (n=%d)\n",
			successIters[len(successIters)/2], len(successIters))
	}

	fmt.Println("\n  recent runs:")
	start := 0
	if len(runs) > 15 {
		start = len(runs) - 15
	}
	for _, r := range runs[start:] {
		goal := render.CellTruncate(r.Goal, 44, "…")
		ticket := r.Ticket
		if ticket == "" {
			ticket = "-"
		}
		fmt.Printf("    %s  %-14s %-9s %-10s %2d/%-2d  %s\n",
			time.Unix(r.StartTs, 0).Local().Format("01-02 15:04"),
			truncateRight(r.Session, 14), ticket,
			runDisplayState(r, now), r.Iters, r.Max, goal)
	}
	return 0
}

func truncateRight(s string, n int) string {
	return render.CellTruncate(s, n, "…")
}
