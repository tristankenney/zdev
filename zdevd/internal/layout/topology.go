package layout

import (
	"sort"
	"strings"
)

// Daemon-driven window topology (ROADMAP NEXT, operator signal 2026-08-21):
// zdevd decides which windows should EXIST in front of the operator, not just
// which panes live inside one window.
//
// PlanTopology is the fleet-scope sibling of Plan. Same contract: observed
// state in, an ordered []Command out, zero I/O, no clock (nowUnix is threaded
// in), so every behavioral edge is a table row instead of a live-tmux ritual.
//
// Phase 1 answers exactly one question: "is an unacked permission prompt
// waiting somewhere other than where I'm looking?" If yes, the agent's own
// window is LINKED into the client session so the operator answers it without
// leaving. When the wait clears, the link goes away.
//
// # Why link-window and not new-window
//
// A tmux window can belong to several sessions at once. zdev therefore never
// creates the agent's window — it links the existing one and unlinks it later,
// which makes the whole feature non-destructive by construction:
//
//	unlink-window (without -k) REFUSES when a window is linked to only one
//	session — "window only linked to one session", exit 1.
//
// Verified against tmux 3.7b, 2026-08-22. That refusal is the safety net: the
// planner never emits -k, so no sequence of plans can destroy an agent's
// window. Killing is reserved for windows zdev itself spawned (none in phase
// 1).
//
// # Ownership doctrine
//
// Generalized verbatim from PlanTeamReap: the tag is the whole safety story.
// A window is a candidate for unlinking only when it carries OwnedOption AND
// is still linked into more than one session. Untagged windows are invisible
// to this planner and can never be touched, so an ordinary work window the
// operator built by hand is safe even if it sits in the reserved index band.
//
// # Index placement is a REQUEST, not a guarantee
//
// Probed 2026-08-22: with `renumber-windows on` (the operator's setting),
// closing any window compacts the whole index space — a link placed at :90
// became :2 the moment an earlier window closed. Reserving a high band is
// therefore impossible under that config, and the planner does not pretend
// otherwise: LinkIndex is where the link is REQUESTED, everything downstream
// keys off window_id, which is stable for the window's whole life. Consumers
// must never navigate to a zdev link by index.

// Topology option names. OwnedOption tags a window as zdev-managed and records
// which agent session it was linked for; the tag lives on the window object,
// so it is visible from every session the window is linked into (verified
// 2026-08-22) — which is exactly what makes a one-shot `list-windows -F`
// discovery query in the client session sufficient.
const (
	OwnedOption = "@zdev-owned"

	// DefaultLinkIndex is the requested index for the first link. High
	// enough to sit past hand-made windows on a normal day; see the
	// renumber caveat above for why it is not a reservation.
	DefaultLinkIndex = 90

	// DefaultTopoDwellSeconds debounces link churn. A window appearing is a
	// far louder event than a sidebar pane appearing, so this is
	// deliberately slower than the sidebar's width hysteresis: a prompt
	// that resolves itself inside the dwell never earns a window.
	DefaultTopoDwellSeconds = 4
)

// TopoConfig holds the topology tunables. Zero values are NOT meaningful;
// build one via DefaultTopoConfig and override in the cmd layer (internal/
// never reads the environment).
type TopoConfig struct {
	// Enabled gates the whole planner. Default false — current behavior is
	// the default, per the standing config-knob convention (ZDEV_TOPOLOGY=1).
	Enabled bool

	// LinkIndex is the requested window index for the first link;
	// subsequent links take LinkIndex+1, +2, ... in plan order.
	LinkIndex int

	// DwellSeconds is how long a wait must have stood before it earns a
	// window.
	DwellSeconds int
}

// DefaultTopoConfig returns the disabled-by-default configuration.
func DefaultTopoConfig() TopoConfig {
	return TopoConfig{
		Enabled:      false,
		LinkIndex:    DefaultLinkIndex,
		DwellSeconds: DefaultTopoDwellSeconds,
	}
}

// TopoAgent is one fleet member as the topology planner sees it. Deliberately
// primitives rather than proto types: internal/layout stays dependency-free
// and the cmd layer owns the proto → layout mapping (same split as Pane).
type TopoAgent struct {
	// Session is the tmux session name owning the agent (the project key).
	Session string

	// WindowID is the "@<n>" window owning the agent's pane. Empty means the
	// daemon could not resolve it; such an agent is never linked (there is
	// nothing safe to link).
	WindowID string

	// Waiting is true when the agent's derived attention is "waiting".
	Waiting bool

	// Permission narrows that to the cheap y/n class (proto.WaitKindPermission).
	// Phase 1 links nothing else: a real decision costs thought and belongs
	// in the triage queue, not in a window that appears under you.
	Permission bool

	// Acked is true once the operator has visited past the highest crossed
	// wait tier. An acked wait has already been seen and must not re-link.
	Acked bool

	// WaitStartedTS is the unix second the wait began; 0 = not waiting.
	WaitStartedTS int64
	Dead          bool
}

// eligible reports the SNAPSHOT-ONLY half of the decision: everything
// knowable without asking tmux a single question. Split out from wantsLink
// because the reconciler's cheap gates (TopoSignature, TopoPending) run
// before any window id has been resolved — folding the tmux-resolved checks
// in here would make both gates unconditionally false and silently disable
// the whole reconcile path.
func (a TopoAgent) eligible(cfg TopoConfig, nowUnix int64) bool {
	switch {
	case a.Session == "":
		return false
	// The watcher is the daemon's control-mode anchor, never a workspace.
	case a.Session == WatcherSession:
		return false
	case a.Dead:
		return true
	case !a.Waiting || !a.Permission || a.Acked:
		return false
	case a.WaitStartedTS <= 0:
		return false
	case nowUnix-a.WaitStartedTS < int64(cfg.DwellSeconds):
		return false
	}
	return true
}

// wantsLink reports whether this agent has earned a window in the client
// session: the snapshot-only criteria plus the two that need tmux. Every
// condition is a "no" by default.
func (a TopoAgent) wantsLink(clientSession string, cfg TopoConfig, nowUnix int64) bool {
	// No resolvable window means there is nothing safe to link.
	if a.WindowID == "" {
		return false
	}
	// Never link a window out of the session the operator is already in —
	// they can reach it with next-window, and a self-link would duplicate a
	// window into its own session.
	if clientSession != "" && a.Session == clientSession {
		return false
	}
	return a.eligible(cfg, nowUnix)
}

// TopoLink is one zdev-owned window currently linked into the client session,
// as gathered by a single `list-windows -t <client>` call.
type TopoLink struct {
	// Index is the window's index within the client session. Used only to
	// address the unlink; never trust it to be the index that was requested
	// (see the renumber caveat).
	Index string

	// WindowID is the stable "@<n>" identity.
	WindowID string

	// OwnedBy is the OwnedOption tag value — the agent session this link was
	// created for. Empty means the window is NOT zdev-managed and is
	// therefore untouchable.
	OwnedBy string

	// LinkedSessions is window_linked_sessions. A value <= 1 means this link
	// is the window's only home (its agent session died while linked), so
	// unlinking it would be destructive — tmux refuses, and so does the
	// planner.
	LinkedSessions int
}

// unlinkable reports whether the planner is permitted to unlink this window at
// all, independent of whether it still wants to.
func (l TopoLink) unlinkable() bool {
	return l.OwnedBy != "" && l.WindowID != "" && l.Index != "" && l.LinkedSessions > 1
}

// TopoView is the observed world at one instant: where the operator is
// looking, what zdev already linked there, and the fleet.
type TopoView struct {
	// ClientSession is the session the operator's client is attached to.
	// Empty (no attached client) plans nothing — there is no "here" to link
	// into, and linking into a detached session would ambush them later.
	ClientSession string

	// Links are the windows currently in ClientSession, tagged or not. The
	// planner needs the untagged ones too: they prove an index is occupied
	// and they must be left alone.
	Links []TopoLink

	// Agents is the fleet.
	Agents []TopoAgent

	// Anchored is the airlock: the operator is in focus on an anchored task.
	// The command-centre contract is that nothing deferred may interrupt
	// while anchored, and a window materializing IS an interruption — so the
	// planner emits nothing and the held set gets its hearing at the
	// boundary review instead.
	Anchored bool
}

// PlanTopology returns the ordered tmux commands that reconcile the client
// session's zdev-owned links to "exactly the unacked permission prompts that
// have stood past the dwell, and nothing else".
//
// An empty result is a deliberate no-op (disabled, anchored, no client, or
// already reconciled). Unlinks are emitted before links so a freed index is
// available to the same batch.
func PlanTopology(v TopoView, cfg TopoConfig, nowUnix int64) []Command {
	if !cfg.Enabled {
		return nil
	}
	// No attached client, or the client is parked in the watcher: nothing to
	// decorate, and the watcher must never be decorated at all.
	if v.ClientSession == "" || v.ClientSession == WatcherSession {
		return nil
	}
	// The airlock wins. Held prompts are not lost — they are still waiting,
	// and the next unanchored plan links them.
	if v.Anchored {
		return nil
	}

	// Which agent sessions have earned a window, and which window each one
	// would contribute. Plan order follows Agents order for determinism.
	wanted := make(map[string]string, len(v.Agents)) // session -> windowID
	wantedWindow := make(map[string]bool, len(v.Agents))
	var wantOrder []TopoAgent
	for _, a := range v.Agents {
		if !a.wantsLink(v.ClientSession, cfg, nowUnix) {
			continue
		}
		if _, dup := wanted[a.Session]; dup {
			continue
		}
		wanted[a.Session] = a.WindowID
		wantedWindow[a.WindowID] = true
		wantOrder = append(wantOrder, a)
	}

	// Pass 1 — retire links that are no longer wanted, and note which wanted
	// windows are already present so pass 2 stays idempotent.
	present := make(map[string]bool, len(v.Links))
	occupied := make(map[string]bool, len(v.Links))
	var out []Command
	for _, l := range v.Links {
		occupied[l.Index] = true
		if l.OwnedBy == "" {
			// Untagged: an operator's own window. Invisible to this planner.
			continue
		}
		// A tagged window whose wait is still live stays, and counts as
		// present so it is not linked twice.
		if wantedWindow[l.WindowID] && wanted[l.OwnedBy] == l.WindowID {
			present[l.WindowID] = true
			continue
		}
		// Wanted no longer — retire it, but only if that is non-destructive.
		// An orphaned link (its agent session died while linked) is the
		// window's last home; tmux would refuse the unlink and forcing it
		// with -k would destroy the corpse the operator may still want to
		// read. Leave it and let the dead-agent trigger own it.
		if !l.unlinkable() {
			present[l.WindowID] = true
			continue
		}
		out = append(out, cmd("unlink-window", "-t", v.ClientSession+":"+l.Index))
		delete(occupied, l.Index)
	}

	// Pass 2 — link what is missing, at the first free index at or after
	// LinkIndex. Detached (-d) so the operator's cursor never moves: that is
	// the one thing this automation may not touch.
	next := cfg.LinkIndex
	for _, a := range wantOrder {
		if present[a.WindowID] {
			continue
		}
		idx := ""
		for {
			idx = itoa(next)
			next++
			if !occupied[idx] {
				break
			}
		}
		occupied[idx] = true
		out = append(out,
			cmd("link-window", "-d", "-s", a.WindowID, "-t", v.ClientSession+":"+idx),
			cmd("set-option", "-w", "-t", a.WindowID, OwnedOption, a.Session),
		)
	}
	return out
}

// TopoSignature summarizes everything in a snapshot that could change the
// plan, WITHOUT consulting tmux. The reconciler compares consecutive
// signatures and only pays for a tmux gather when one changes, so an idle
// fleet costs nothing per published snapshot.
//
// clientSession is deliberately excluded (learning it costs a tmux call), so
// the signature can over-trigger slightly — a reconcile that turns out to be a
// no-op. That is the correct bias: the signature is a change detector, never
// the decision.
func TopoSignature(agents []TopoAgent, anchored bool, cfg TopoConfig, nowUnix int64) string {
	if !cfg.Enabled {
		return "disabled"
	}
	var fleet []string
	var earned []string
	for _, a := range agents {
		if a.Session != "" && a.Session != WatcherSession {
			fleet = append(fleet, a.Session)
		}
		if a.eligible(cfg, nowUnix) {
			earned = append(earned, a.Session)
		}
	}
	sort.Strings(fleet)
	sort.Strings(earned)
	prefix := "open"
	if anchored {
		prefix = "anchored"
	}
	return prefix + "\x00" + strings.Join(fleet, "\x00") + "\x01" + strings.Join(earned, "\x00")
}

type AgentPaneRef struct {
	ID           string
	RemainOnExit bool
}

func PlanRemainOnExit(panes []AgentPaneRef, cfg TopoConfig) []Command {
	if !cfg.Enabled {
		return nil
	}
	var out []Command
	for _, p := range panes {
		if p.ID != "" && !p.RemainOnExit {
			out = append(out, cmd("set-option", "-p", "-t", p.ID, "remain-on-exit", "on"))
		}
	}
	return out
}

// TopoNextDeadline returns the earliest unix second at which the plan could
// change through the passage of time alone — the moment the soonest pending
// wait crosses the dwell. Returns 0 when no wait is pending, meaning there is
// nothing to wake up for.
//
// This is what lets the reconciler stay event-driven: instead of a heartbeat
// (banned by the daemon's hidden-ticker discipline — scripts/check-no-daemon-fork.sh),
// it arms ONE timer for exactly this instant, and arms nothing at all when the
// answer is 0.
func TopoNextDeadline(agents []TopoAgent, cfg TopoConfig, nowUnix int64) int64 {
	if !cfg.Enabled {
		return 0
	}
	var soonest int64
	for _, a := range agents {
		// Eligible-but-for-the-dwell is precisely the pending set.
		if !a.eligible(TopoConfig{DwellSeconds: 0}, nowUnix) {
			continue
		}
		deadline := a.WaitStartedTS + int64(cfg.DwellSeconds)
		if deadline <= nowUnix {
			// Already past the dwell — no future wake-up needed for this one;
			// the current pass already accounts for it.
			continue
		}
		if soonest == 0 || deadline < soonest {
			soonest = deadline
		}
	}
	return soonest
}
