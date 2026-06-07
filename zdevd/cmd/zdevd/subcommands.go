// Subcommand entry points for the daemon binary (D4-06).
//
// The `zdevd` binary doubles as the introspection CLI: when invoked with
// `diag` or `history` as os.Args[1], main() short-circuits to the subcommand
// path BEFORE flag.Parse / Run starts the daemon. With no args (or with
// `serve`), the binary continues into run() per the launchd contract.
//
// D4-06 single-binary cohesion: keeps `~/.local/bin/zdevd` as the only
// installable binary; no symlinks; the LaunchAgent plist invokes `zdevd`
// with no args (still starts as a daemon).
//
// D4-09 hybrid output: human-readable default; --json for scripts.
//
// LOG-04 outage-resilience: history reads events.ndjson directly via
// eventlog.TailLines. Daemon need NOT be running. Missing path is silent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tristankenney/zdev/zdevd/internal/demo"
	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	socketpkg "github.com/tristankenney/zdev/zdevd/internal/socket"
)

// diagSubcmd implements `zdevd diag [--socket PATH] [--json]`. Returns the
// process exit code (0 on success, 1 on socket/RPC failure, 2 on usage).
//
// The dial timeout is 2s — diag round-trips are sub-millisecond on localhost,
// so a 2s ceiling is generous. The renderer's `snapshotReadTimeout` (1s) gates
// the read once dial succeeds.
func diagSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd diag", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", defaultSocketPath(), "path to zdevd unix socket")
	asJSON := fs.Bool("json", false, "emit raw NDJSON (default: human-readable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := socketpkg.DialDiag(ctx, *socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd diag: %v\n", err)
		return 1
	}
	if *asJSON {
		b, jerr := json.Marshal(reply)
		if jerr != nil {
			fmt.Fprintf(os.Stderr, "zdevd diag: marshal: %v\n", jerr)
			return 1
		}
		fmt.Println(string(b))
	} else {
		fmt.Print(diag.FormatHuman(reply))
	}
	return 0
}

// historySubcmd implements `zdevd history [--path PATH] [--tail N] [--json]`.
// Reads ~/.local/state/zdev/events.ndjson (and .ndjson.1 if needed) via
// eventlog.TailLines. Default --tail is 50.
//
// LOG-04 outage-resilience: a missing file returns no error and prints
// nothing. Same behavior the daemon would surface: events.ndjson is created
// on first emission, so a fresh install before the daemon ever ran has no
// history yet — that's fine, not an error.
func historySubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("path", eventlog.DefaultPath(), "path to events.ndjson")
	tail := fs.Int("tail", 50, "number of most recent events to show")
	asJSON := fs.Bool("json", false, "emit raw NDJSON lines (default: human-readable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tail <= 0 {
		fmt.Fprintln(os.Stderr, "zdevd history: --tail must be > 0")
		return 2
	}
	lines, err := eventlog.TailLines(*path, *tail)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd history: %v\n", err)
		return 1
	}
	if *asJSON {
		for _, line := range lines {
			fmt.Println(string(line))
		}
		return 0
	}
	for _, line := range lines {
		var ev eventlog.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "zdevd history: skipping malformed line: %v\n", err)
			continue
		}
		fmt.Println(formatEventHuman(ev))
	}
	return 0
}

// demoSubcmd implements `zdevd demo [--socket PATH]`.
//
// It starts a scripted fake-fleet server on the daemon socket so a real
// zdev-sidebar can connect from a clean clone with no agents, no gh auth,
// and no tmux fleet. The demo replays committed golden fixtures, animating
// tier escalation (idle → working → waiting → death) in ~30 seconds.
//
// After connecting a sidebar, Ctrl-C stops the demo and closes all subscriber
// connections cleanly.
func demoSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket", defaultSocketPath(), "unix socket path for the demo server")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	src, err := demo.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd demo: load fixtures: %v\n", err)
		return 1
	}

	srv := socketpkg.NewServer(*socketPath)
	srv.SetHub(src)
	if err := srv.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd demo: listen: %v\n", err)
		return 1
	}
	defer func() { _ = srv.Close() }()

	fmt.Fprintf(os.Stderr, "zdevd demo: serving on %s (Ctrl-C to stop)\n", *socketPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return src.Run(gctx) })
	g.Go(func() error {
		if serr := srv.Serve(gctx); serr != nil && !errors.Is(serr, context.Canceled) {
			return serr
		}
		return nil
	})
	if gerr := g.Wait(); gerr != nil && !errors.Is(gerr, context.Canceled) {
		fmt.Fprintf(os.Stderr, "zdevd demo: %v\n", gerr)
		return 1
	}
	return 0
}

// formatEventHuman renders one Event as a `[HH:MM:SS] type k=v ...` line per
// D4-09 default human output. Local-time formatting (the user reading the
// log is on the daemon's host).
func formatEventHuman(ev eventlog.Event) string {
	ts := ev.Ts.Local().Format("15:04:05")
	switch ev.Type {
	case "state-change":
		return fmt.Sprintf("[%s] state-change session=%s project=%s %s→%s",
			ts, ev.Session, ev.Project, ev.From, ev.To)
	case "pr-count":
		return fmt.Sprintf("[%s] pr-count project=%s %d→%d",
			ts, ev.Project, ev.OpenBefore, ev.OpenAfter)
	case "port-change":
		return fmt.Sprintf("[%s] port-change session=%s :%d %s",
			ts, ev.Session, ev.Port, ev.Op)
	case "daemon-start":
		return fmt.Sprintf("[%s] daemon-start version=%s pid=%d",
			ts, ev.Version, ev.PID)
	case "daemon-stop":
		return fmt.Sprintf("[%s] daemon-stop pid=%d", ts, ev.PID)
	default:
		return fmt.Sprintf("[%s] %s (unknown type)", ts, ev.Type)
	}
}
