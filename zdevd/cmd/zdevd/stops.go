package main

// zdevd stops — the wait-split report (loop-layer phase 0a/1, observe-only).
//
// Reads events.ndjson directly (same LOG-04 posture as `zdevd history`: no
// daemon required, a missing file prints nothing) and classifies every
// →waiting transition into the stop taxonomy from docs/design/loop-layer.md:
//
//	policy      — a permission prompt; fixable forever by one allowlist line
//	judgement   — a real question; a human must answer it
//	verifiable? — a question whose summary smells like a machine-checkable
//	              condition (tests/build/lint). The "?" is honest: the hook
//	              says decision, the text says check — the concierge tally
//	              arbitrates.
//	derived     — title-derived wait with no hook reason found (all history
//	              before the enrichment landed is here, so the report dates
//	              its own usefulness)
//
// Classification joins two event streams: the →waiting state-change (title
// -derived, counts the stop) and the wait-reason event (hook-derived, names
// it). They arrive in either order, so the join looks for the session's
// nearest wait-reason within ±joinWindow of the transition; an inline
// Reason on the state-change itself (notify-first ordering) short-circuits
// the search.
//
// This is the C1 instrument: the loop-layer's build order gates on the
// machine-resolvable share (policy + verifiable?) of classified stops.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// joinWindow bounds how far a wait-reason may sit from its →waiting
// transition and still name it. Generous on purpose: the notify watcher
// and the title classifier race through separate debounces, and a wrong
// join within one wait episode is harmless (it IS that episode's reason).
const joinWindow = 2 * time.Minute

// stopClass buckets one stop. Pure function of reason/detail — no clocks,
// no state — so the table test is the spec.
func stopClass(reason, detail string) string {
	switch reason {
	case proto.WaitKindPermission:
		return "policy"
	case proto.WaitKindDecision:
		if verifiableHint(detail) {
			return "verifiable?"
		}
		return "judgement"
	case "":
		return "derived"
	default:
		// Unknown future kinds classify conservatively as judgement —
		// never let an unrecognised stop look machine-resolvable.
		return "judgement"
	}
}

// verifiableHint reports whether a wait summary reads like a
// machine-checkable condition. Deliberately crude keyword matching: the
// point is a first-cut split for C1, validated against the hand tally,
// not NLP. Tuned only by adding words the tally proves are missed.
func verifiableHint(detail string) bool {
	d := strings.ToLower(detail)
	for _, kw := range []string{
		"test", "build", "compile", "lint", "gofmt", "vet",
		"ci ", "ci:", "pipeline", "fail", "failing", "red",
		"error", "errors", "coverage", "golden",
	} {
		if strings.Contains(d, kw) {
			return true
		}
	}
	return false
}

// stop is one classified →waiting transition.
type stop struct {
	ts      time.Time
	project string
	class   string
}

// classifyStops joins →waiting state-changes against wait-reason events and
// returns one classified stop per transition, in input order. Pure function
// of its inputs — the events slice is assumed time-ordered (the eventlog is
// append-only), but the join tolerates reasons on either side of the
// transition, which is the whole point.
func classifyStops(events []eventlog.Event) []stop {
	// Per-session time-ordered reason events.
	reasons := map[string][]eventlog.Event{}
	for _, ev := range events {
		if ev.Type == "wait-reason" {
			reasons[ev.Session] = append(reasons[ev.Session], ev)
		}
	}

	var out []stop
	for _, ev := range events {
		if ev.Type != "state-change" || ev.To != "waiting" {
			continue
		}
		reason, detail := ev.Reason, ev.Detail
		if reason == "" {
			// Nearest wait-reason for this session within the window,
			// either side of the transition.
			var best *eventlog.Event
			var bestGap time.Duration
			for i := range reasons[ev.Session] {
				r := &reasons[ev.Session][i]
				gap := r.Ts.Sub(ev.Ts)
				if gap < 0 {
					gap = -gap
				}
				if gap <= joinWindow && (best == nil || gap < bestGap) {
					best, bestGap = r, gap
				}
			}
			if best != nil {
				reason, detail = best.Reason, best.Detail
			}
		}
		out = append(out, stop{ts: ev.Ts, project: ev.Project, class: stopClass(reason, detail)})
	}
	return out
}

// stopsSubcmd implements `zdevd stops [--path PATH] [--days N] [--by-project]`.
func stopsSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd stops", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("path", eventlog.DefaultPath(), "path to events.ndjson")
	days := fs.Int("days", 14, "how many days back to include")
	byProject := fs.Bool("by-project", false, "add a per-project breakdown")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	f, err := os.Open(*path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0 // LOG-04: no history yet is not an error
		}
		fmt.Fprintf(os.Stderr, "zdevd stops: %v\n", err)
		return 1
	}
	defer f.Close()

	cutoff := time.Now().AddDate(0, 0, -*days)
	var events []eventlog.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev eventlog.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue // malformed lines skip silently, same as history
		}
		if ev.Ts.Before(cutoff) {
			continue
		}
		switch ev.Type {
		case "state-change", "wait-reason":
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd stops: %v\n", err)
		return 1
	}

	stops := classifyStops(events)
	if len(stops) == 0 {
		fmt.Printf("no →waiting transitions in the last %d days\n", *days)
		return 0
	}

	classes := []string{"policy", "verifiable?", "judgement", "derived"}
	total := map[string]int{}
	perDay := map[string]map[string]int{}
	perProj := map[string]map[string]int{}
	for _, st := range stops {
		total[st.class]++
		day := st.ts.Local().Format("2006-01-02")
		if perDay[day] == nil {
			perDay[day] = map[string]int{}
		}
		perDay[day][st.class]++
		if perProj[st.project] == nil {
			perProj[st.project] = map[string]int{}
		}
		perProj[st.project][st.class]++
	}

	n := len(stops)
	fmt.Printf("stops, last %d days (n=%d)\n\n", *days, n)
	for _, c := range classes {
		fmt.Printf("  %-12s %5d  (%d%%)\n", c, total[c], total[c]*100/n)
	}

	// The C1 line: machine-resolvable share of CLASSIFIED stops. Derived
	// stops are excluded from the denominator — they are unclassified, and
	// counting them either way would bias the gate.
	classified := total["policy"] + total["verifiable?"] + total["judgement"]
	if classified > 0 {
		machine := total["policy"] + total["verifiable?"]
		fmt.Printf("\n  C1: machine-resolvable %d/%d classified (%d%%) — gate is 1/3\n",
			machine, classified, machine*100/classified)
	} else {
		fmt.Println("\n  C1: no classified stops yet — data accrues from the enrichment onward")
	}

	fmt.Println("\n  per day:")
	daysSorted := make([]string, 0, len(perDay))
	for d := range perDay {
		daysSorted = append(daysSorted, d)
	}
	sort.Strings(daysSorted)
	for _, d := range daysSorted {
		fmt.Printf("    %s  %s\n", d, formatClassRow(perDay[d], classes))
	}

	if *byProject {
		fmt.Println("\n  per project:")
		projs := make([]string, 0, len(perProj))
		for p := range perProj {
			projs = append(projs, p)
		}
		sort.Slice(projs, func(i, j int) bool {
			ti, tj := 0, 0
			for _, c := range classes {
				ti += perProj[projs[i]][c]
				tj += perProj[projs[j]][c]
			}
			return ti > tj
		})
		for _, p := range projs {
			fmt.Printf("    %-32s %s\n", p, formatClassRow(perProj[p], classes))
		}
	}
	return 0
}

func formatClassRow(row map[string]int, classes []string) string {
	var b strings.Builder
	for _, c := range classes {
		if row[c] > 0 {
			fmt.Fprintf(&b, "%s=%d ", c, row[c])
		}
	}
	return strings.TrimRight(b.String(), " ")
}
