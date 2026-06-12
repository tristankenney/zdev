// `zdevd layout [window-id]` — the daemon-side replacement for
// bin/zdev-sidebar-toggle's bash internals (architecture review card 4).
//
// The bash forked tmux ~26 times per window per hook fire (hundreds during a
// resize drag — a measured typing-lag source, perf-hunt 2026-06-11), and
// failed silently when the render binary couldn't exec. This subcommand:
//
//   - gathers each window's pane inventory with ONE `tmux list-panes` call
//     (a single format string carries pane geometry + the window width +
//     session name);
//   - computes the command batch with the pure internal/layout engine;
//   - applies it with ONE `tmux` exec (semicolon-chained commands);
//   - FAILS LOUDLY when the render binary is missing — it logs and paints a
//     visible error into the pane instead of leaving a silent blank.
//
// Behavior parity with bin/zdev-sidebar-toggle is the spec: per-window lock,
// zdevd-watcher exclusion, current-window-first sweep, hysteresis, duplicate
// reaping, and the column-aware rebalance all live in internal/layout and are
// exercised by table tests; this file is the I/O shell around that core.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/teams"
)

// layoutTmuxTimeout bounds each tmux subprocess. Layout runs on a hot hook
// path; a wedged tmux query must not hang the hook. 5s is generous for
// localhost list-panes / batch-apply round-trips while still failing fast.
const layoutTmuxTimeout = 5 * time.Second

// layoutSubcmd implements `zdevd layout [window-id]`.
//
//	with a window-id : reconcile only that window (hot path — the hook knows
//	                   exactly what changed);
//	no args          : sweep every window across every session, current
//	                   window first so its sidebar appears with no delay.
//
// Exit codes: 0 ok (including benign per-window skips), 1 tmux missing/exec
// failure, 2 usage.
func layoutSubcmd(args []string) int {
	fs := flag.NewFlagSet("zdevd layout", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketName := fs.String("socket-name", "", "tmux -L socket name (testing; empty = user's default server)")
	teamsDir := fs.String("teams-dir", "", "team-config root for team-sweep / team-reap (testing; empty = ~/.claude/teams)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Team verbs share the engine (one inventory gather, one batched apply,
	// same -socket-name / -teams-dir test seams):
	//   team-sweep [window-id] — slice A: relocate teammate panes into their
	//                            own windows.
	//   team-reap  [-dry-run]  — slice D: kill member windows whose team is
	//                            gone from disk.
	verb := ""
	if fs.NArg() >= 1 {
		verb = fs.Arg(0)
	}
	sweep := verb == "team-sweep"
	reap := verb == "team-reap"
	if !sweep && !reap && fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: zdevd layout [window-id] | zdevd layout team-sweep [window-id] | zdevd layout team-reap [-dry-run]")
		return 2
	}

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout: tmux not on PATH: %v\n", err)
		return 1
	}
	eng := &layoutEngine{
		tmux:       tmux,
		socketName: *socketName,
		cfg:        layout.ConfigFromEnv(os.LookupEnv),
		sidebarCmd: resolveSidebarCommand(),
	}

	if sweep {
		if fs.NArg() > 2 {
			fmt.Fprintln(os.Stderr, "usage: zdevd layout team-sweep [window-id]")
			return 2
		}
		dir := *teamsDir
		if dir == "" {
			dir = teams.DefaultDir()
		}
		return eng.teamSweep(fs.Arg(1), dir)
	}

	if reap {
		// -dry-run may follow the verb (`team-reap -dry-run`); flag.Parse
		// stops at the first non-flag, so the trailing token lands as a
		// positional and we read it ourselves rather than via the flagset.
		dryRun := false
		for _, a := range fs.Args()[1:] {
			if a == "-dry-run" || a == "--dry-run" {
				dryRun = true
				continue
			}
			fmt.Fprintln(os.Stderr, "usage: zdevd layout team-reap [-dry-run]")
			return 2
		}
		dir := *teamsDir
		if dir == "" {
			dir = teams.DefaultDir()
		}
		return eng.teamReap(dryRun, dir)
	}

	if fs.NArg() == 1 {
		eng.processWindow(fs.Arg(0))
		return 0
	}

	current := eng.current(context.Background())
	if current != "" {
		eng.processWindow(current)
	}
	for _, wid := range eng.allWindows(context.Background()) {
		if wid == "" || wid == current {
			continue
		}
		eng.processWindow(wid)
	}
	return 0
}

// layoutEngine carries the resolved tmux binary, the layout config, and the
// sidebar command for one invocation, so per-window work stays argument-free.
type layoutEngine struct {
	tmux       string
	socketName string
	cfg        layout.Config
	sidebarCmd string
}

// tmuxArgs prefixes `-L <socket>` when socketName is set (tests only) so a
// scratch server can be driven without touching the user's default server.
func (e *layoutEngine) tmuxArgs(rest ...string) []string {
	if e.socketName != "" {
		return append([]string{"-L", e.socketName}, rest...)
	}
	return rest
}

func (e *layoutEngine) run(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, layoutTmuxTimeout)
	defer cancel()
	var out, errBuf bytes.Buffer
	c := exec.CommandContext(cctx, e.tmux, e.tmuxArgs(args...)...)
	c.Stdout = &out
	c.Stderr = &errBuf
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

func (e *layoutEngine) current(ctx context.Context) string {
	out, err := e.run(ctx, "display-message", "-p", "#{window_id}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (e *layoutEngine) allWindows(ctx context.Context) []string {
	out, err := e.run(ctx, "list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// inventoryFormat packs everything Plan needs into one list-panes format
// string. Pane title is LAST and parsed with a bounded split so a title
// containing the '|' delimiter still lands intact in the final field.
// window_width and session_name are window-level vars that repeat per pane;
// we read them off the first row.
const inventoryFormat = "#{pane_id}|#{pane_left}|#{pane_top}|#{pane_width}|" +
	"#{pane_height}|#{pane_active}|#{@is-sidebar}|#{window_width}|#{session_name}|#{pane_title}"

const inventoryFields = 10

// processWindow reconciles a single window: lock, gather inventory (one
// list-panes), compute the batch, apply it (one tmux exec). All failures are
// benign skips except a missing render binary (handled loudly upstream in
// resolveSidebarCommand) and apply errors (logged to stderr).
func (e *layoutEngine) processWindow(windowID string) {
	if windowID == "" {
		return
	}
	ctx := context.Background()

	// Per-window lock — concurrent hook fires for the SAME window serialize;
	// fires for different windows run in parallel. mkdir is the atomic
	// primitive (mirrors the bash). A held lock means another fire is mid
	// reconcile; skipping is correct.
	lock := lockDir(windowID)
	if err := os.Mkdir(lock, 0o700); err != nil {
		return
	}
	defer func() { _ = os.Remove(lock) }()

	out, err := e.run(ctx, "list-panes", "-t", windowID, "-F", inventoryFormat)
	if err != nil {
		// Window vanished between the sweep and now, or tmux hiccup — a
		// benign skip on this hot path.
		return
	}

	win, ok := parseInventory(windowID, out)
	if !ok {
		return
	}
	win.EffectiveWidth = e.effectiveWidth(ctx, win.EffectiveWidth)
	win.SidebarCommand = e.sidebarCmd

	cmds := layout.Plan(win, e.cfg)
	if len(cmds) == 0 {
		return
	}
	if err := e.apply(ctx, cmds); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout: apply %s: %v\n", windowID, err)
	}
}

// effectiveWidth uses the window's own width when usable; otherwise falls back
// to the largest attached client's width (the hook can fire from a session no
// client is currently driving). Mirrors the bash's window-width-then-client
// resolution, but the client query happens ONLY on the fallback path.
func (e *layoutEngine) effectiveWidth(ctx context.Context, windowWidth int) int {
	if windowWidth > 0 {
		return windowWidth
	}
	out, err := e.run(ctx, "list-clients", "-F", "#{client_width}")
	if err != nil {
		return 0
	}
	best := 0
	for _, line := range strings.Split(out, "\n") {
		if n, perr := strconv.Atoi(strings.TrimSpace(line)); perr == nil && n > best {
			best = n
		}
	}
	return best
}

// apply chains every command into a single `tmux a b \; c d \; ...` exec.
// The create path relies on this chaining: split-window selects the new pane,
// so the immediately-following set-option/select-pane/resize-pane (which
// target the active pane) act on the just-created sidebar — no second exec to
// capture its id.
func (e *layoutEngine) apply(ctx context.Context, cmds []layout.Command) error {
	var args []string
	for i, c := range cmds {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, c.Args...)
	}
	_, err := e.run(ctx, args...)
	return err
}

// parseInventory turns one list-panes block into a layout.Window. Returns
// (_, false) when there are no usable pane rows.
func parseInventory(windowID, out string) (layout.Window, bool) {
	win := layout.Window{ID: windowID}
	have := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "|", inventoryFields)
		if len(f) < inventoryFields {
			continue
		}
		p := layout.Pane{
			ID:         f[0],
			Left:       atoiOr(f[1], 0),
			Top:        atoiOr(f[2], 0),
			Width:      atoiOr(f[3], 0),
			Height:     atoiOr(f[4], 0),
			Active:     f[5] == "1",
			SidebarOpt: f[6] == "1",
			Title:      f[9],
		}
		win.Panes = append(win.Panes, p)
		if !have {
			// Window-level fields repeat across rows; take them once.
			win.EffectiveWidth = atoiOr(f[7], 0)
			win.Session = f[8]
			have = true
		}
	}
	return win, have
}

// resolveSidebarCommand returns the shell command the sidebar pane execs.
// When the render binary is executable, that's `exec <path>`. When it is
// missing or non-executable, we FAIL LOUDLY (architecture review card 4): log
// to stderr AND return a command that paints a persistent, visible error into
// the pane — never the silent blank the bash left behind. ZDEV_SIDEBAR_RENDER
// overrides the default ~/.local/bin/zdev-sidebar-render path.
func resolveSidebarCommand() string {
	path := os.Getenv("ZDEV_SIDEBAR_RENDER")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".local", "bin", "zdev-sidebar-render")
	}
	if isExecutable(path) {
		return "exec " + path
	}
	fmt.Fprintf(os.Stderr,
		"zdevd layout: sidebar render binary missing or not executable at %q — painting an error pane instead of a blank\n",
		path)
	// Persisted error pane: print to the pane, then idle so the message
	// stays on screen rather than the pane dying back to a blank split.
	// tmux already runs the split command via `sh -c`, so this string IS
	// the sh command — NO extra `sh -c` wrapper (nesting single quotes
	// breaks). printf %b interprets the \n escapes for line breaks.
	msg := fmt.Sprintf("zdev ERROR: sidebar render binary not found or not executable:\\n  %s\\n"+
		"(reinstall zdev, or set ZDEV_SIDEBAR_RENDER)\\n", path)
	return fmt.Sprintf("printf %%b %s; while :; do sleep 3600; done", shSingleQuote(msg))
}

// isExecutable reports whether path is a regular file with any execute bit.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// shSingleQuote wraps s in single quotes for safe embedding in the `sh -c`
// string, escaping any embedded single quotes.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// lockDir is the per-window lock path: tmux ids contain non-path-safe chars
// ('@'), so they're sanitized the same way the bash did.
func lockDir(windowID string) string {
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, windowID)
	return filepath.Join(tmp, "zdev-sidebar-"+safe+".lock")
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
