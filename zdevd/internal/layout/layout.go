// Package layout is the daemon-side layout engine that decides, for one tmux
// window, the exact sequence of tmux commands needed to keep exactly one zdev
// sidebar pane present (when the window is wide enough) or none (when it
// isn't). It is the pure replacement for the arithmetic and branching that
// bin/zdev-sidebar-toggle used to do in bash by forking tmux ~26 times per
// window per hook fire — a measured typing-lag source during resize drags
// (perf-hunt 2026-06-11).
//
// The core (Plan) is a PURE function: pane inventory + window width + config
// in, an ordered []Command out. It performs zero I/O, never reads the
// environment, and never calls time.Now() — the cmd/zdevd `layout` subcommand
// gathers the inventory with one `tmux list-panes` call, calls Plan, and
// applies the batch with a single `tmux` exec. That makes every behavioral
// edge (threshold, hysteresis, watcher exclusion, column-aware rebalance,
// duplicate reaping) a table-driven test row rather than a live-tmux ritual.
//
// Behavior parity with bin/zdev-sidebar-toggle is the spec. Where a decision
// references the bash, the relevant commit/ID is cited so the two stay in
// sync until the bash is retired.
package layout

import (
	"sort"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

// SidebarTitle is the pane title stamped on the sidebar pane (and recognized
// as a sidebar even when the @is-sidebar option is somehow absent). Mirrors
// the `pane_title == "zdev-sidebar"` half of the bash's detection predicate.
const SidebarTitle = "zdev-sidebar"

// Defaults match bin/zdev-sidebar-toggle. 160 threshold: a fullscreen laptop
// terminal is ~180 cols, so the older 200 suppressed the sidebar on the most
// common screen size (dogfood 2026-06-06). 50 width leaves two ~54-col panes
// at 160 — tight but workable. 30 hysteresis keeps the sidebar from flapping
// in the band just under the threshold.
const (
	DefaultThreshold  = 160
	DefaultWidth      = 50
	DefaultHysteresis = 30
)

// Config holds the three width tunables. Zero values are NOT meaningful;
// callers build a Config via DefaultConfig (optionally overridden from the
// environment in the cmd layer — internal/ never reads env directly).
type Config struct {
	Threshold  int // ZDEV_SIDEBAR_THRESHOLD: min window width to show a sidebar
	Width      int // ZDEV_SIDEBAR_WIDTH: pinned sidebar column width
	Hysteresis int // ZDEV_SIDEBAR_HYSTERESIS: kill-below band under Threshold
}

// DefaultConfig returns the Config that reproduces bin/zdev-sidebar-toggle's
// built-in defaults.
func DefaultConfig() Config {
	return Config{Threshold: DefaultThreshold, Width: DefaultWidth, Hysteresis: DefaultHysteresis}
}

// Pane is one pane in a window's inventory, as gathered from a single
// `tmux list-panes` format string. Left/Top/Width/Height are tmux cells.
// SidebarOpt is the @is-sidebar option (1 → true); Title is the pane title.
// A pane counts as the sidebar when EITHER holds (see isSidebar) — the option
// is authoritative, the title is the fallback the bash also honored.
type Pane struct {
	ID         string
	Left       int
	Top        int
	Width      int
	Height     int
	Title      string
	Active     bool
	SidebarOpt bool

	// InMode is #{pane_in_mode}: the pane is in copy-mode (or any other
	// tmux mode). Resizing it discards the scroll position the operator was
	// reading, so a window containing one is left entirely alone.
	InMode bool

	// Agent is true when the pane title classifies as an agent (resolved by
	// the cmd layer against internal/agents — internal/layout stays
	// dependency-free). The pane planner never takes rows from an agent.
	Agent bool

	// PaneOpt is the @zdev-pane option: non-empty means this pane IS a zdev
	// agent viewport, and the value names the session it was opened for.
	PaneOpt string
}

func (p Pane) isSidebar() bool { return p.SidebarOpt || p.Title == SidebarTitle }

// anyPaneInMode reports whether any pane in the window is in copy-mode.
func (w Window) anyPaneInMode() bool {
	for _, p := range w.Panes {
		if p.InMode {
			return true
		}
	}
	return false
}

// Window is the full input to Plan: the target window, the session that owns
// it (for the zdevd-watcher exclusion), the effective width to threshold
// against (the window's own width, with the client width as the caller's
// fallback), the pane inventory, and the shell command the sidebar pane
// should exec when one is created.
type Window struct {
	ID      string
	Session string

	// Zoomed is #{window_zoomed_flag}. A zoomed window is the operator
	// deliberately filling the screen with one pane; splitting, killing or
	// rebalancing under that fights them and silently discards the zoom.
	// Plan refuses to touch such a window at all.
	Zoomed bool

	// EffectiveWidth is the width Plan thresholds against — the window's
	// own width (so a hook firing from a clientless session still sizes by
	// the window's last-known geometry), falling back to the client width
	// upstream. Used both for the show/hide decision and as win_w in the
	// column rebalance.
	EffectiveWidth int

	// TeamWindow marks a window created by team-sweep for an Agent Teams
	// teammate (any pane reporting a non-empty @zdev-team — the option is
	// stamped on both the pane and the window, and tmux format lookup
	// falls back pane→window). Plan never decorates these: a sidebar per
	// teammate window is clutter, an extra renderer process each, and —
	// found by the first production reap — the surviving sidebar pane
	// kept dead member windows alive past their team.
	TeamWindow bool

	Panes []Pane

	// SidebarCommand is the shell command passed to `split-window` when a
	// sidebar is created. The cmd layer resolves it: `exec <render-bin>`
	// when the render binary is executable, or a loud in-pane error
	// display when it is missing/non-executable (never a silent blank).
	// Unused on the reap/rebalance/kill paths.
	SidebarCommand string
}

// WatcherSession is the session name Plan refuses to decorate. zdevd-watcher
// is the daemon's control-mode anchor, not a workspace; giving it a sidebar
// made it look like a legitimate place to work, which is how user panes ended
// up squatting in the keepalive window after a plain `tmux attach` landed
// there (third occurrence, 2026-06-07; see process_window in the bash).
const WatcherSession = "zdevd-watcher"

// Command is one tmux command as an argv slice (the leading `tmux` is
// implied). The executor chains a Plan's Commands into a single `tmux a b \;
// c d \; ...` invocation. Args never carry the literal `;` separator — the
// executor inserts it between Commands.
type Command struct {
	Args []string
}

func cmd(args ...string) Command { return Command{Args: args} }

// Plan returns the ordered tmux commands that reconcile the window to the
// invariant "exactly one sidebar when wide enough, none when too narrow,
// columns balanced around a pinned sidebar". An empty result is a deliberate
// no-op (watcher window, hysteresis dead-band, or nothing to change).
//
// State table (effective width W, sidebar count C), mirroring the bash:
//
//	W >= Threshold,            C == 0 → create sidebar, rebalance, restore focus
//	W >= Threshold,            C  > 1 → reap extras, rebalance
//	W >= Threshold,            C == 1 → rebalance only
//	W <  Threshold-Hysteresis, C >= 1 → kill all sidebars, even-horizontal
//	W <  Threshold-Hysteresis, C == 0 → even-horizontal (normalize)
//	Threshold-Hyst <= W < Threshold   → no-op (dead band: keep current state)
func Plan(w Window, cfg Config) []Command {
	if w.ID == "" {
		return nil
	}
	// NEVER decorate the watcher session (load-bearing — see WatcherSession).
	if w.Session == WatcherSession {
		return nil
	}
	// NEVER decorate a teammate's window (see Window.TeamWindow).
	if w.TeamWindow {
		return nil
	}
	// NEVER mutate a window the operator has taken over.
	//
	// Both of these were unguarded until 2026-08-22, which meant an ordinary
	// resize hook could reflow a zoomed window (discarding the zoom) or
	// resize a pane in copy-mode (discarding the operator's scroll position).
	// Found while designing the pane planner — geometry changes are far more
	// destructive than the sidebar's original show/hide made them look.
	// Deferring is always safe: the next hook fire reconciles once the
	// operator is out.
	if w.Zoomed || w.anyPaneInMode() {
		return nil
	}

	var sidebars []Pane
	for _, p := range w.Panes {
		if p.isSidebar() {
			sidebars = append(sidebars, p)
		}
	}

	// GHOST WINDOW: nothing left but zdev's own panes.
	//
	// tmux closes a window when its last pane exits, and a session when its
	// last window closes. A sidebar pane defeats both: when the operator's
	// shell and agent exit, the renderer keeps the window — and therefore the
	// session — alive, and that empty session then rows itself in the sidebar
	// as a project nobody can account for. Found 2026-08-22 as a `zdev` row
	// surviving the checkout rename by two days, its only pane a renderer.
	// This is the same failure PlanTeamReap already names for member windows
	// ("the surviving sidebar pane kept dead member windows alive past their
	// team"), one level up.
	//
	// Reaping our own panes just lets tmux do what it would have done
	// unaided. Only zdev-owned panes are ever killed, so a window with any
	// real pane left is untouched.
	if !w.hasWorkPane() {
		return ghostPlan(w)
	}

	switch {
	case w.EffectiveWidth >= cfg.Threshold:
		switch {
		case len(sidebars) == 0:
			return createPlan(w, cfg)
		case len(sidebars) > 1:
			return reapPlan(w, cfg, sidebars)
		default:
			return rebalanceCmds(w, cfg, sidebars[0].ID)
		}
	case w.EffectiveWidth < cfg.Threshold-cfg.Hysteresis:
		return killPlan(w, sidebars)
	default:
		// Hysteresis dead band: leave whatever is there untouched so the
		// sidebar doesn't flap as a client hovers near the threshold.
		return nil
	}
}

// createPlan splits a new sidebar in at the left edge, marks it, rebalances
// the columns around it, and restores focus to the previously-active pane.
// The new pane's id is unknown ahead of time, so the marking and the
// sidebar-pin step target the ACTIVE pane: split-window selects the new pane,
// so the immediately-following commands act on it. Focus is then handed back
// explicitly to the captured prev-active pane.
func createPlan(w Window, cfg Config) []Command {
	leftmost := leftmostPaneID(w.Panes)
	if leftmost == "" {
		return nil
	}
	prevActive := activePaneID(w.Panes)

	cmds := []Command{
		// -hb: split horizontally, new pane BEFORE (left of) the target.
		// -l: exact width. -P -F: print the new pane id (the executor
		// discards it; we rely on active-pane semantics below, but the
		// flags match the bash and keep the command self-describing).
		cmd("split-window", "-hb", "-l", itoa(cfg.Width), "-t", leftmost,
			"-P", "-F", "#{pane_id}", w.SidebarCommand),
		// -p with no -t: pane option on the active (just-created) pane.
		cmd("set-option", "-p", "@is-sidebar", "1"),
		// Title on the active pane → recognized as a sidebar by title too.
		cmd("select-pane", "-T", SidebarTitle),
	}
	// Pin + balance: the active pane is the new sidebar (target "").
	cmds = append(cmds, rebalanceCmds(w, cfg, "")...)
	if prevActive != "" {
		cmds = append(cmds, cmd("select-pane", "-t", prevActive))
	}
	return cmds
}

// reapPlan kills every sidebar pane except the first (inventory order), then
// rebalances around the survivor. Duplicate sidebars arise when two hook
// fires race a split before either sees the other's pane.
func reapPlan(w Window, cfg Config, sidebars []Pane) []Command {
	var cmds []Command
	for _, extra := range sidebars[1:] {
		cmds = append(cmds, cmd("kill-pane", "-t", extra.ID))
	}
	cmds = append(cmds, rebalanceCmds(w, cfg, sidebars[0].ID)...)
	return cmds
}

// killPlan removes all sidebar panes and normalizes the remaining panes to an
// even horizontal split. Used below the hysteresis floor (too narrow to keep
// a sidebar) — and harmless when there are no sidebars (just normalizes).
func killPlan(w Window, sidebars []Pane) []Command {
	var cmds []Command
	for _, p := range sidebars {
		cmds = append(cmds, cmd("kill-pane", "-t", p.ID))
	}
	cmds = append(cmds, cmd("select-layout", "-t", w.ID, "even-horizontal"))
	return cmds
}

// hasWorkPane reports whether any pane in the window is NOT zdev's own — a
// shell, an editor, an agent, anything the operator would lose.
func (w Window) hasWorkPane() bool {
	for _, p := range w.Panes {
		if p.isSidebar() || p.PaneOpt != "" {
			continue
		}
		return true
	}
	return false
}

// ghostPlan kills every zdev-owned pane in a window that has nothing else
// left, so tmux can close the window (and, if it was the last one, the
// session). Emits nothing when there is nothing of ours to reap — a genuinely
// empty inventory is a gather failure, not a licence to kill.
func ghostPlan(w Window) []Command {
	var cmds []Command
	for _, p := range w.Panes {
		if p.isSidebar() || p.PaneOpt != "" {
			cmds = append(cmds, cmd("kill-pane", "-t", p.ID))
		}
	}
	return cmds
}

// rebalanceCmds pins the sidebar to cfg.Width and hands every other COLUMN an
// equal share of the remaining width. Equalizing COLUMNS — not panes — is
// load-bearing: Agent-Teams teammates split VERTICALLY into the lead's
// window, so several panes share one pane_left. Treating each stacked pane as
// its own width slot computed each ≈ (w-50)/5 ≈ 59, crushed the whole stack
// into a 59-col column and ballooned the sidebar to fill the rest (dogfood
// screenshot, 2026-06-11; commit 58b822c). One representative pane per
// distinct pane_left sizes its column.
//
// sidebarTarget is the pane to pin: a known pane id, or "" meaning "the
// active pane" (used right after split-window, when the new pane's id isn't
// known but it IS the active pane).
//
// The last representative is intentionally left unsized so it absorbs the
// rounding remainder (mirrors the bash's `sed '$d'`), and the same guards
// apply: fewer than two columns, or a per-column width below 10, skip the
// per-column resizes (but still pin the sidebar).
func rebalanceCmds(w Window, cfg Config, sidebarTarget string) []Command {
	var cmds []Command
	if sidebarTarget == "" {
		cmds = append(cmds, cmd("resize-pane", "-x", itoa(cfg.Width)))
	} else {
		cmds = append(cmds, cmd("resize-pane", "-t", sidebarTarget, "-x", itoa(cfg.Width)))
	}

	reps := columnReps(w.Panes)
	n := len(reps)
	if n < 2 {
		return cmds
	}
	each := (w.EffectiveWidth - cfg.Width - n) / n
	if each < 10 {
		return cmds
	}
	// Resize all but the last column; the last absorbs the remainder.
	for _, id := range reps[:len(reps)-1] {
		cmds = append(cmds, cmd("resize-pane", "-t", id, "-x", itoa(each)))
	}
	return cmds
}

// columnReps returns one representative pane id per distinct pane_left among
// the NON-sidebar panes, ordered left-to-right. The first pane encountered at
// each Left wins (stable over input order, after a stable sort by Left).
func columnReps(panes []Pane) []string {
	cols := make([]Pane, 0, len(panes))
	for _, p := range panes {
		if !p.isSidebar() {
			cols = append(cols, p)
		}
	}
	sort.SliceStable(cols, func(i, j int) bool { return cols[i].Left < cols[j].Left })

	var reps []string
	seen := make(map[int]bool)
	for _, p := range cols {
		if seen[p.Left] {
			continue
		}
		seen[p.Left] = true
		reps = append(reps, p.ID)
	}
	return reps
}

// leftmostPaneID returns the id of the pane with the smallest Left (first in
// input order on ties). The new sidebar is split in before this pane.
func leftmostPaneID(panes []Pane) string {
	best := ""
	bestLeft := 0
	for _, p := range panes {
		if best == "" || p.Left < bestLeft {
			best, bestLeft = p.ID, p.Left
		}
	}
	return best
}

// activePaneID returns the id of the first active pane, or "" if none is
// marked active (focus restoration is then skipped).
func activePaneID(panes []Pane) string {
	for _, p := range panes {
		if p.Active {
			return p.ID
		}
	}
	return ""
}
