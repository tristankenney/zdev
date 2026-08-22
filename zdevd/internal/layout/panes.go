package layout

// Agent-requested panes — the pane half of daemon-driven topology
// (docs/design/window-topology.md).
//
// PlanPanes is the third pure planner beside Plan and PlanTeamSweep, and it
// answers one question: does this window currently deserve the turn-scoped
// viewport its agent asked for?
//
// # The agent proposes, the planner disposes
//
// A request is never a command. It arrives as a file (internal/panereq) saying
// only "a pane titled X, please", and every reason to say no lives here: the
// window guards, a live turn, the operator's veto, and a donor with room. That
// keeps exactly one policy for panes regardless of who asked, so an agent can
// never reach a surface the daemon would not have opened itself.
//
// # The shell is the donor; the agent is sacred
//
// The new pane splits the largest NON-agent, non-sidebar pane. You can always
// get another shell; you cannot get back the prompt you were halfway through
// typing — and because the agent pane's geometry never changes, its TUI never
// reflows. That removes the entire class of "opening a pane scrambled my input"
// failure by construction rather than by guard.
//
// # Turn-scoped
//
// The request is only honored while the agent's turn is live. The Stop hook
// that ends a turn therefore also retires the pane, so a looping agent cannot
// accumulate panes across turns — the bound is free, from hook plumbing that
// already exists.
//
// One exception, and it is the behavior that decides whether this feels good:
// if the operator is IN the pane when the turn ends, it is demoted rather than
// killed. Killing a pane somebody is mid-read is the same failure as destroying
// their scrollback.

// Pane option and defaults for agent panes.
const (
	// PaneOption tags a pane as an agent viewport and records which session
	// asked for it. Set by the pane's own command via $TMUX_PANE (verified
	// 2026-08-22) so the tag can never be missing — a pane is tagged before
	// it renders a byte.
	PaneOption = "@zdev-pane"

	// DefaultPaneRows is the viewport's height. Eight lines shows a stack
	// trace's head or a test summary without taking a third of the window.
	DefaultPaneRows = 8

	// DefaultDonorFloorRows is the height a donor must keep AFTER the split.
	// Below this a shell is not a shell any more, so the request is refused
	// and the agent's output stays in its own transcript.
	DefaultDonorFloorRows = 8

	// DefaultPaneMaxAgeSec ages out a request whose turn-end signal never
	// arrived — an agent killed mid-turn, or hooks not installed. Generous,
	// because it is a backstop against a leak and not the primary lifetime:
	// the Stop hook is. An hour of a stale viewport is a nuisance; retiring a
	// live one mid-turn is the failure that matters.
	DefaultPaneMaxAgeSec = 3600

	// endedSuffix marks a demoted pane — the turn ended while the operator
	// was reading it, so the viewport stopped following and is now theirs to
	// close.
	endedSuffix = " · ended"
)

// PaneConfig holds the pane tunables. Build via DefaultPaneConfig.
type PaneConfig struct {
	// Enabled gates the whole planner. Default false: current behavior is
	// the default (ZDEV_PANES=1).
	Enabled bool

	// Rows is the requested height of a new viewport.
	Rows int

	// DonorFloorRows is the height the donor must retain after splitting.
	DonorFloorRows int

	// MaxAgeSec is the backstop that retires a request whose turn-end signal
	// never came.
	MaxAgeSec int

	// LogsCommand is the operator-configured command followed by the inferred
	// runner logs pane. Empty disables inferred logs while leaving requested
	// agent panes available.
	LogsCommand string
}

// DefaultPaneConfig returns the disabled-by-default configuration.
func DefaultPaneConfig() PaneConfig {
	return PaneConfig{
		Enabled:        false,
		Rows:           DefaultPaneRows,
		DonorFloorRows: DefaultDonorFloorRows,
		MaxAgeSec:      DefaultPaneMaxAgeSec,
	}
}

// PaneView is the observed world for one window.
type PaneView struct {
	// Window carries the identity and the guards this planner reuses
	// wholesale — watcher exclusion, teammate windows, zoom, copy-mode — plus
	// the pane inventory the donor is chosen from.
	Window Window

	// Requested is true when a well-formed request file exists for
	// Window.Session.
	Requested bool

	// Title is the sanitized request title, rendered on the pane border.
	Title string

	// RequestedTS is when the request was written. Used only by the age
	// backstop; 0 disables it (an unknown age is not a reason to retire).
	RequestedTS int64

	// TurnLive is true while the agent's turn is still standing.
	//
	// This is deliberately NOT derived from the agent looking busy. Attention
	// reads AttWorking from a braille-spinner title or a HookWorkTS fresher
	// than hookWorkFreshSec (180s, internal/hub/attention.go) — so a long turn
	// whose title is parked at a bare "claude" and whose last hook fired four
	// minutes ago reads as idle, and inferring "turn over" from that would
	// retire a pane while the operator was still reading output in it. Found
	// on the first real-fleet run, 2026-08-22.
	//
	// A turn therefore ends only on POSITIVE evidence: the Stop hook
	// withdrawing the request (bin/zdev-notify), the agent dying, or the
	// request ageing out. Absence of evidence is not evidence of an ended
	// turn.
	TurnLive bool

	// Vetoed is true when the operator closed this agent's pane by hand
	// during the current turn. A veto is not a dismissal: nothing reopens
	// until the turn cycles.
	Vetoed bool

	// AttachCommand is the shell command a new pane execs. Resolved by the
	// cmd layer (it self-tags via $TMUX_PANE, then tails the stream), so this
	// package needs no knowledge of paths or binaries.
	AttachCommand string
}

// stale reports whether the request has outlived the age backstop.
func (v PaneView) stale(cfg PaneConfig, nowUnix int64) bool {
	if cfg.MaxAgeSec <= 0 || v.RequestedTS <= 0 {
		return false
	}
	return nowUnix-v.RequestedTS > int64(cfg.MaxAgeSec)
}

// agentPane returns the existing zdev viewport in the window, if any.
func (v PaneView) agentPane() (Pane, bool) {
	for _, p := range v.Window.Panes {
		if p.PaneOpt != "" {
			return p, true
		}
	}
	return Pane{}, false
}

// donor picks the pane the viewport takes its rows from: the tallest pane that
// is neither the sidebar, nor an agent, nor an existing viewport, and that can
// still stand after giving up cfg.Rows. Returns (_, false) when nothing
// qualifies — in which case no pane opens, which is the correct answer.
func (v PaneView) donor(cfg PaneConfig) (Pane, bool) {
	var best Pane
	found := false
	for _, p := range v.Window.Panes {
		if p.isSidebar() || p.Agent || p.PaneOpt != "" || p.LogsOpt != "" {
			continue
		}
		// A split costs the donor cfg.Rows plus the border line.
		if p.Height-cfg.Rows-1 < cfg.DonorFloorRows {
			continue
		}
		if !found || p.Height > best.Height {
			best, found = p, true
		}
	}
	return best, found
}

// PlanPanes reconciles one window's agent viewport. An empty result is a
// deliberate no-op.
func PlanPanes(v PaneView, cfg PaneConfig, nowUnix int64) []Command {
	if !cfg.Enabled {
		return nil
	}
	w := v.Window
	if w.ID == "" {
		return nil
	}
	// Every guard Plan applies applies here too, and more strongly: this
	// planner changes geometry, which is the destructive part.
	if w.Session == WatcherSession || w.TeamWindow {
		return nil
	}
	if w.Zoomed || w.anyPaneInMode() {
		return nil
	}

	existing, have := v.agentPane()
	want := v.Requested && v.TurnLive && !v.Vetoed && !v.stale(cfg, nowUnix)

	switch {
	case have && want:
		// Already open and still wanted. Keep the border label in sync so a
		// re-request with a new title is not silently ignored, and undo any
		// earlier demotion.
		label := paneLabel(existing.PaneOpt, v.Title)
		if existing.Title == label {
			return nil
		}
		return []Command{cmd("select-pane", "-t", existing.ID, "-T", label)}

	case have && !want:
		// The operator is reading it: demote, never kill. Retitling is the
		// whole demotion — the pane keeps its content and becomes theirs.
		if existing.Active {
			label := paneLabel(existing.PaneOpt, v.Title) + endedSuffix
			if existing.Title == label {
				return nil
			}
			return []Command{cmd("select-pane", "-t", existing.ID, "-T", label)}
		}
		// Ours, tagged, unwatched — killing is safe and is the only kill this
		// planner ever emits.
		return []Command{cmd("kill-pane", "-t", existing.ID)}

	case !have && want:
		if v.AttachCommand == "" {
			return nil
		}
		d, ok := v.donor(cfg)
		if !ok {
			return nil
		}
		// -d so the operator's cursor never moves; -v so the viewport is a
		// short wide row (terminal output is line-oriented) rather than a
		// narrow column.
		return []Command{cmd(
			"split-window", "-d", "-v",
			"-l", itoa(cfg.Rows),
			"-t", d.ID,
			v.AttachCommand,
		)}
	}
	return nil
}

// paneLabel is the pane-border text: the owning session and the agent's title,
// or just the session when the agent supplied nothing usable.
func paneLabel(session, title string) string {
	if title == "" {
		return session
	}
	return session + " · " + title
}

// IsSidebar lets the cmd layer reuse the sidebar predicate when classifying
// panes, without duplicating the option-or-title rule.
func (p Pane) IsSidebar() bool { return p.isSidebar() }
