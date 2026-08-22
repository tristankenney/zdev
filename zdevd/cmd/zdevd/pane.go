// `zdevd pane {open,close,attach,reconcile}` — the agent-facing side of
// turn-scoped panes (docs/design/window-topology.md).
//
//	open <session> [-title T]  record a request; prints the stream path the
//	                           agent writes to. One per session, by design.
//	close <session>            withdraw it; the pane goes on the next reconcile.
//	attach <session>           run INSIDE the new pane: self-tag via $TMUX_PANE,
//	                           label the border, then tail the stream. Not for
//	                           humans — the planner puts this in split-window.
//	reconcile [-dry-run]       sweep every window once (the manual path; the
//	                           daemon does this on snapshot changes).
//
// The agent never names a command. It gets a path to write to and the daemon
// owns the process, so a pane grants no capability bash did not already give.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/panereq"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// paneSubcmd implements `zdevd pane <verb>`. Exit codes: 0 ok, 1 failure,
// 2 usage.
func paneSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd pane", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	title := fs.String("title", "", "border label for the pane (sanitized, one line)")
	dirFlag := fs.String("dir", "", "request directory (testing; empty = $TMPDIR/"+panereq.SubdirName+")")
	socketName := fs.String("socket-name", "", "tmux -L socket name (testing)")
	dryRun := fs.Bool("dry-run", false, "reconcile: print the plan, change nothing")
	// flag.Parse stops at the first non-flag argument, so `pane open api
	// -title X` would leave -title as a positional and fail as a usage error
	// — exactly how the generated attach command first broke, inside a pane
	// nobody was looking at. Parse iteratively so flags may appear before,
	// after, or between positionals: the form an agent or a human actually
	// types.
	pos, parsed := parseInterspersed(fs, args)
	if !parsed {
		return 2
	}
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "usage: zdevd pane {open <session> [-title T] | close <session> | attach <session> | reconcile [-dry-run]}")
		return 2
	}
	dir := *dirFlag
	if dir == "" {
		dir = panereq.Dir(os.TempDir())
	}

	switch verb := pos[0]; verb {
	case "open":
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "usage: zdevd pane open <session> [-title T]")
			return 2
		}
		stream, err := panereq.Open(dir, pos[1], *title, time.Now().Unix())
		if err != nil {
			fmt.Fprintf(os.Stderr, "zdevd pane open: %v\n", err)
			return 1
		}
		// The stream path IS the return value: the agent appends to it.
		fmt.Println(stream)
		return 0

	case "close":
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "usage: zdevd pane close <session>")
			return 2
		}
		if err := panereq.Close(dir, pos[1]); err != nil {
			fmt.Fprintf(os.Stderr, "zdevd pane close: %v\n", err)
			return 1
		}
		return 0

	case "attach":
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "usage: zdevd pane attach <session>")
			return 2
		}
		return paneAttach(dir, pos[1], *socketName)

	case "logs-attach":
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "usage: zdevd pane logs-attach <session>")
			return 2
		}
		cmd := layout.PaneConfigFromEnv(os.LookupEnv).LogsCommand
		if cmd == "" {
			fmt.Fprintln(os.Stderr, "zdevd pane logs-attach: ZDEV_PANES_LOGS_COMMAND is empty")
			return 1
		}
		return logsAttach(pos[1], *socketName, cmd)

	case "ci-attach":
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "usage: zdevd pane ci-attach <session>")
			return 2
		}
		cmd := layout.PaneConfigFromEnv(os.LookupEnv).CICommand
		if cmd == "" {
			fmt.Fprintln(os.Stderr, "zdevd pane ci-attach: ZDEV_PANES_CI_COMMAND is empty")
			return 1
		}
		return surfaceAttach("CI", cmd)

	case "reconcile":
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "usage: zdevd pane reconcile [-dry-run]")
			return 2
		}
		eng, rc := newPaneEngine(*socketName)
		if rc != 0 {
			return rc
		}
		return eng.reconcilePanes(dir, *dryRun, layout.PaneConfigFromEnv(os.LookupEnv))

	default:
		fmt.Fprintf(os.Stderr, "zdevd pane: unknown verb %q\n", verb)
		return 2
	}
}

func logsAttach(session, _ string, command string) int {
	self := os.Getenv("TMUX_PANE")
	if self == "" {
		fmt.Fprintln(os.Stderr, "zdevd pane logs-attach: no $TMUX_PANE — not running inside tmux")
		return 1
	}
	return surfaceAttach("logs", command)
}

func surfaceAttach(kind, command string) int {
	c := exec.Command("/bin/sh", "-lc", command)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n[zdev] %s command exited: %v; pane remains until its condition clears or you close it\n", kind, err)
	} else {
		fmt.Fprintf(os.Stderr, "\n[zdev] %s command exited; pane remains until its condition clears or you close it\n", kind)
	}
	// Do not let an unexpectedly short-lived command look like an operator
	// close: disappearance while the inferred condition still stands is the
	// suppression signal. Keep the tagged pane until a real clear edge (or
	// the operator explicitly closes it).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	<-sigs
	return 0
}

// paneAttach runs inside the freshly split pane. It tags and titles ITSELF via
// $TMUX_PANE — verified 2026-08-22 that a `split-window -d` child still gets
// that variable — which is why the planner can emit a single-command batch and
// still be certain the pane is never untagged. Then it execs the tail.
func paneAttach(dir, session, socketName string) int {
	self := os.Getenv("TMUX_PANE")
	if self == "" {
		fmt.Fprintln(os.Stderr, "zdevd pane attach: no $TMUX_PANE — not running inside tmux")
		return 1
	}
	req, ok := panereq.Read(dir, session)
	if !ok {
		// The request vanished between plan and exec. Say so in the pane
		// rather than tailing nothing forever.
		fmt.Fprintf(os.Stderr, "zdevd pane attach: no live request for %q\n", session)
		return 1
	}

	tmuxArgs := func(rest ...string) []string {
		if socketName != "" {
			return append([]string{"-L", socketName}, rest...)
		}
		return rest
	}
	ctx, cancel := context.WithTimeout(context.Background(), layoutTmuxTimeout)
	defer cancel()
	label := session
	if req.Title != "" {
		label = session + " · " + req.Title
	}
	// Tag first: if the title call fails the pane is still identifiable, so
	// the planner can always retire it.
	runTmux(ctx, tmuxArgs("set-option", "-p", "-t", self, layout.PaneOption, session)...)
	runTmux(ctx, tmuxArgs("select-pane", "-t", self, "-T", label)...)

	return tailStream(req.Stream)
}

// paneEngine reuses the layout engine's tmux plumbing; panes need the same
// gather/apply discipline as the sidebar.
func newPaneEngine(socketName string) (*layoutEngine, int) {
	tmuxBin, err := lookTmux()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd pane: tmux not on PATH: %v\n", err)
		return nil, 1
	}
	return &layoutEngine{
		tmux:       tmuxBin,
		socketName: socketName,
		cfg:        layout.ConfigFromEnv(os.LookupEnv),
	}, 0
}

// reconcilePanes sweeps every window once: gather, plan, apply.
func (e *layoutEngine) reconcilePanes(dir string, dryRun bool, cfg layout.PaneConfig) int {
	ctx := context.Background()
	reqs, err := panereq.ReadAll(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd pane reconcile: requests: %v\n", err)
		return 1
	}
	bySession := make(map[string]panereq.Request, len(reqs))
	for _, r := range reqs {
		bySession[r.Session] = r
	}

	snap, err := e.snapshot(ctx)
	if err != nil && !dryRun {
		// Without a snapshot there is no turn state, and honoring a request
		// with no turn bound is exactly what must not happen.
		fmt.Fprintf(os.Stderr, "zdevd pane reconcile: snapshot: %v\n", err)
		return 1
	}
	turns := turnState(snap)

	var plans int
	for _, wid := range e.allWindows(ctx) {
		v, ok := e.paneView(ctx, wid, dir, bySession, turns)
		if !ok {
			continue
		}
		plan := layout.PlanPanes(v, cfg, time.Now().Unix())
		if len(plan) == 0 {
			continue
		}
		plans++
		if dryRun {
			fmt.Printf("%s (%s):\n", wid, v.Window.Session)
			for _, c := range plan {
				fmt.Printf("  tmux %s\n", strings.Join(c.Args, " "))
			}
			continue
		}
		if err := e.apply(ctx, plan); err != nil {
			fmt.Fprintf(os.Stderr, "zdevd pane reconcile: apply %s: %v\n", wid, err)
		}
	}
	if dryRun && plans == 0 {
		fmt.Println("pane reconcile: (no-op)")
	}
	return 0
}

// paneView gathers one window into the planner's input.
func (e *layoutEngine) paneView(ctx context.Context, windowID, dir string,
	bySession map[string]panereq.Request, turns map[string]bool) (layout.PaneView, bool) {

	out, err := e.run(ctx, "list-panes", "-t", windowID, "-F", inventoryFormat)
	if err != nil {
		return layout.PaneView{}, false
	}
	win, ok := parseInventory(windowID, out)
	if !ok {
		return layout.PaneView{}, false
	}
	markAgentPanes(&win)

	req, requested := bySession[win.Session]
	v := layout.PaneView{
		Window:      win,
		Requested:   requested,
		Title:       req.Title,
		RequestedTS: req.TS,
		// A session absent from the snapshot has no death record, so its
		// turn stands — a request only exists because an agent wrote it.
		TurnLive: !requested || turnLiveFor(turns, win.Session),
	}
	if requested {
		v.AttachCommand = e.attachCommand(win.Session, dir)
	}
	return v, true
}

// attachCommand is what a new pane execs. Self-tagging happens in `pane
// attach`, so the planner stays a pure function of observed state.
//
// Flags precede the verb. flag.Parse stops at the first non-flag argument, so
// a trailing `-dir`/`-socket-name` would be silently swallowed as a positional
// and turn into a usage error inside a pane nobody is looking at — which is
// exactly how this was first written, and how it failed.
func (e *layoutEngine) attachCommand(session, dir string) string {
	self := e.execPath
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil || self == "" {
			return ""
		}
	}
	parts := []string{shellQuote(self), "pane"}
	if dir != "" {
		parts = append(parts, "-dir", shellQuote(dir))
	}
	if e.socketName != "" {
		parts = append(parts, "-socket-name", shellQuote(e.socketName))
	}
	parts = append(parts, "attach", shellQuote(session))
	return "exec " + strings.Join(parts, " ")
}

func (e *layoutEngine) logsAttachCommand(session string) string {
	return e.surfaceAttachCommand("logs-attach", session)
}

func (e *layoutEngine) ciAttachCommand(session string) string {
	return e.surfaceAttachCommand("ci-attach", session)
}

func (e *layoutEngine) surfaceAttachCommand(verb, session string) string {
	self := e.execPath
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil || self == "" {
			return ""
		}
	}
	parts := []string{shellQuote(self), "pane"}
	if e.socketName != "" {
		parts = append(parts, "-socket-name", shellQuote(e.socketName))
	}
	parts = append(parts, verb, shellQuote(session))
	return "exec " + strings.Join(parts, " ")
}

// markAgentPanes flags the panes whose titles the agent registry recognizes, so
// the planner can refuse to take rows from an agent without importing
// internal/agents itself.
func markAgentPanes(win *layout.Window) {
	reg := agents.NewRegistry(agents.Builtin())
	for i := range win.Panes {
		if win.Panes[i].IsSidebar() {
			continue
		}
		if name, _ := reg.Classify(win.Panes[i].Title); name != "" {
			win.Panes[i].Agent = true
		}
	}
}

// turnState maps session → "the agent's turn is still standing".
//
// A standing request IS the turn: the Stop hook withdraws it
// (bin/zdev-notify), so by the time the request is gone the turn is over and
// this map is not consulted for it at all. What remains here is the one
// POSITIVE counter-signal a snapshot can give — the agent is dead, so no
// turn-end hook is ever coming.
//
// It deliberately does NOT return false for an idle-looking agent. AttWorking
// decays with hookWorkFreshSec (180s), so a long quiet turn reads as idle and
// that inference would retire a pane the operator was reading. Absence of
// evidence is not evidence of an ended turn.
func turnState(snap *proto.Snapshot) map[string]bool {
	live := make(map[string]bool)
	if snap == nil {
		return live
	}
	for _, p := range snap.Projects {
		live[p.Name] = p.Attention != proto.AttDead
	}
	return live
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func lookTmux() (string, error) { return exec.LookPath("tmux") }

// runTmux fires one tmux command and ignores the outcome. Used only by
// `pane attach` for its own tagging: if tagging fails the pane is cosmetically
// wrong, which is not worth refusing to show the operator their output over.
func runTmux(ctx context.Context, args ...string) {
	bin, err := lookTmux()
	if err != nil {
		return
	}
	_ = exec.CommandContext(ctx, bin, args...).Run()
}

// tailStream follows the request's stream file for the life of the pane.
// `tail -F` (not -f) so it survives the agent truncating or recreating the
// file mid-turn.
func tailStream(path string) int {
	bin, err := exec.LookPath("tail")
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd pane attach: tail not on PATH: %v\n", err)
		return 1
	}
	c := exec.Command(bin, "-n", "200", "-F", path)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return 1
	}
	return 0
}

// parseInterspersed parses fs against args, tolerating flags anywhere among the
// positionals, and returns the positionals in order. parsed=false means fs has
// already reported a usage error.
func parseInterspersed(fs *flag.FlagSet, args []string) (positional []string, parsed bool) {
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, false
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, true
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

// turnLiveFor treats an unknown session as live: absence from the snapshot is
// not a death, and only a death ends a turn early.
func turnLiveFor(turns map[string]bool, session string) bool {
	live, known := turns[session]
	return !known || live
}
