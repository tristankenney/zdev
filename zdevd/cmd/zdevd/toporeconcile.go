package main

// The hub-driven half of daemon window topology: a reconciler that watches
// published snapshots and keeps the client session's zdev-owned links matching
// the fleet, so the operator never runs `zdevd layout topology` by hand.
//
// # Why this is not in internal/hub
//
// The hub is a single-writer goroutine with a pure applyEvent, and it already
// has exactly one side-effect seam (notifier). Adding a second one to drive
// tmux would put window I/O inside the state machine's blast radius for no
// benefit — the subscriber contract already delivers every published snapshot
// to any consumer that asks. So this registers as an ordinary in-process
// subscriber and the hub is untouched.
//
// # Cost discipline
//
// Snapshots publish on a ~16ms debounce, so reconciling naively would mean
// tens of tmux execs a second. A snapshot-only signature
// (layout.TopoSignature) gates that: no tmux calls at all unless the set of
// link-earning sessions actually changed. An idle fleet costs one string
// comparison per snapshot and no subprocesses.
//
// # No heartbeat
//
// The daemon's hidden-ticker discipline (scripts/check-no-daemon-fork.sh
// bans time.NewTicker / time.AfterFunc outright) rules out polling, and it is
// right to: a periodic wake-up would burn the idle budget `make bench-idle`
// defends. Only two things can change the plan without a new snapshot, and
// neither needs one:
//
//	a dwell threshold crossing — armed as ONE one-shot timer for exactly the
//	  instant layout.TopoNextDeadline names, and not armed at all when there
//	  is no pending wait (the same time.NewTimer pattern the hub's own dwell
//	  uses);
//	the operator switching client sessions — already a forced publish
//	  (Hub.lastClientSessionsSeq), so it arrives as a snapshot like anything
//	  else.
//
// With an idle fleet this loop blocks on a channel receive and holds no timer.

import (
	"context"
	"log/slog"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/panereq"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// topoReconciler drives layout.PlanTopology from published snapshots.
type topoReconciler struct {
	h   *hub.Hub
	eng *layoutEngine
	cfg layout.TopoConfig

	// now is threaded rather than called directly so the reconcile decision
	// stays testable (project convention: no time.Now() in logic).
	now func() time.Time

	// lastSig is the signature of the fleet as of the last reconcile that
	// actually ran. Empty string is a real value (nothing earns a window),
	// so applied tracks whether we have reconciled at all.
	lastSig string
	applied bool

	// paneCfg / paneDir / panes drive the pane half; see the panes section
	// at the bottom of this file.
	paneCfg layout.PaneConfig
	paneDir string
	panes   map[string]*paneState
}

func newTopoReconciler(h *hub.Hub, eng *layoutEngine, cfg layout.TopoConfig,
	paneCfg layout.PaneConfig, paneDir string) *topoReconciler {
	return &topoReconciler{
		h:       h,
		eng:     eng,
		cfg:     cfg,
		now:     time.Now,
		paneCfg: paneCfg,
		paneDir: paneDir,
	}
}

// Run subscribes and reconciles until ctx is cancelled. Returns nil on
// cancellation; a registration failure is fatal to this loop only.
//
// Disabled is the default: with ZDEV_TOPOLOGY unset this returns immediately
// and the daemon never subscribes, so the feature costs nothing until it is
// switched on.
func (r *topoReconciler) Run(ctx context.Context) error {
	if !r.cfg.Enabled && !r.paneCfg.Enabled {
		slog.Debug("topology: disabled (set ZDEV_TOPOLOGY=1 / ZDEV_PANES=1 to enable)")
		return nil
	}

	sub := hub.NewSubscriber("", "")
	if err := r.h.Register(sub, nil); err != nil {
		// The hub is already shutting down; nothing to reconcile.
		slog.Warn("topology: register failed", "err", err)
		return nil
	}
	defer r.h.Unregister(sub)

	slog.Info("topology: reconciler started",
		"link_index", r.cfg.LinkIndex, "dwell_s", r.cfg.DwellSeconds)

	// dwell is the single one-shot timer, armed only while a wait is pending.
	// A nil channel blocks forever, which is exactly the idle behavior wanted.
	var dwell *time.Timer
	var dwellC <-chan time.Time
	stopDwell := func() {
		if dwell != nil {
			dwell.Stop()
			dwell, dwellC = nil, nil
		}
	}
	defer stopDwell()

	var latest *proto.Snapshot
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.Done():
			return nil
		case snap := <-sub.Snaps():
			if snap == nil {
				continue
			}
			latest = snap
		case <-dwellC:
			// The timer has fired; it is one-shot, so drop it before
			// deciding whether a fresh one is warranted.
			dwell, dwellC = nil, nil
			if latest == nil {
				continue
			}
		}

		r.consider(ctx, latest)
		r.considerPanes(ctx, latest)

		// Re-arm for the next dwell crossing, if there is one. Recomputed
		// from the current snapshot every pass so an answered wait cancels
		// the wake-up it had scheduled.
		nowT := r.now()
		deadline := layout.TopoNextDeadline(snapshotAgents(latest), r.cfg, nowT.Unix())
		stopDwell()
		if deadline > 0 {
			wait := time.Unix(deadline, 0).Sub(nowT) + topoDwellSlack
			if wait < topoDwellSlack {
				wait = topoDwellSlack
			}
			dwell = time.NewTimer(wait)
			dwellC = dwell.C
		}
	}
}

// topoDwellSlack pads the armed deadline so the timer fires strictly AFTER the
// dwell has elapsed rather than on the same second, where integer-second
// truncation could leave the wait one tick short of eligible and schedule a
// second wake-up for nothing.
const topoDwellSlack = 250 * time.Millisecond

// consider decides whether this wake-up is worth any tmux calls, then
// reconciles if it is.
//
// The signature covers both callers uniformly: a dwell crossing changes the
// earning set, so the timer's wake-up moves the signature exactly like a fleet
// change would, and no separate "came from a timer" path is needed.
func (r *topoReconciler) consider(ctx context.Context, snap *proto.Snapshot) {
	nowUnix := r.now().Unix()
	agents := snapshotAgents(snap)
	anchored := snapshotAnchored(snap)
	sig := layout.TopoSignature(agents, anchored, r.cfg, nowUnix)

	if r.applied && sig == r.lastSig {
		return
	}

	r.reconcile(ctx, agents, anchored, nowUnix)
	r.lastSig = sig
	r.applied = true
}

// considerPanes is the pane half's entry point. Panes are NOT gated on the
// window signature: a turn ending or an operator closing a pane changes
// nothing about which sessions are earning a linked window, so the two halves
// have independent triggers.
func (r *topoReconciler) considerPanes(ctx context.Context, snap *proto.Snapshot) {
	r.reconcilePanes(ctx, snap)
}

// reconcile performs the gather → plan → apply cycle. Every tmux failure is
// logged and swallowed: a wedged or restarting tmux must never take the daemon
// down, and the next tick retries.
func (r *topoReconciler) reconcile(ctx context.Context, agents []layout.TopoAgent, anchored bool, nowUnix int64) {
	client := r.eng.clientSession(ctx)
	if client == "" {
		return
	}

	links := r.eng.clientLinks(ctx, client)
	windows := r.eng.agentWindows(ctx)
	resolved := make([]layout.TopoAgent, len(agents))
	for i, a := range agents {
		a.WindowID = windows[a.Session]
		resolved[i] = a
	}

	plan := layout.PlanTopology(layout.TopoView{
		ClientSession: client,
		Links:         links,
		Agents:        resolved,
		Anchored:      anchored,
	}, r.cfg, nowUnix)
	if len(plan) == 0 {
		return
	}

	if err := r.eng.apply(ctx, plan); err != nil {
		slog.Warn("topology: apply failed", "err", err, "commands", len(plan))
		return
	}
	slog.Info("topology: reconciled", "client", client, "commands", len(plan))
}

// snapshotAgents projects the wire snapshot onto planner inputs WITHOUT
// resolving window ids — that costs a tmux call and is only done once the
// signature says a reconcile is warranted.
func snapshotAgents(snap *proto.Snapshot) []layout.TopoAgent {
	if snap == nil {
		return nil
	}
	out := make([]layout.TopoAgent, 0, len(snap.Projects))
	for _, p := range snap.Projects {
		out = append(out, layout.TopoAgent{
			Session:       p.Name,
			Waiting:       p.Attention == proto.AttWaiting,
			Permission:    p.WaitKind == proto.WaitKindPermission,
			Acked:         p.WaitAcknowledged,
			WaitStartedTS: p.WaitStartedTS,
		})
	}
	return out
}

func snapshotAnchored(snap *proto.Snapshot) bool {
	return snap != nil && snap.Anchor != nil && snap.Anchor.Title != ""
}

// ---------- panes ----------
//
// The pane half rides the same subscriber: a snapshot carries the turn state
// that gates every request, so there is nothing else to wait for.
//
// Two pieces of state live here rather than in the planner, because both are
// differences between successive observations:
//
//	the VETO — the operator closed an agent's pane by hand. Seen as "we had a
//	  tagged pane, the request and turn are both still live, and now the pane
//	  is gone". A veto is not a dismissal: nothing reopens until the turn
//	  cycles, which is what stops the feature becoming a fight.
//	turn-end CLEANUP — when a turn ends the request file is withdrawn, so the
//	  agent must ask again next turn. Without this, "turn-scoped" would only
//	  be true until the next turn started.

// paneState is the reconciler's per-session memory for panes.
type paneState struct {
	sawPane        bool // a tagged viewport was present at the last gather
	vetoed         bool // the operator closed it during this turn
	sawLogs        bool
	logsSuppressed bool
	runnerUp       bool
}

// reconcilePanes reconciles every window's agent viewport. Called from the same
// consider() pass as the window planner, and gated the same way — it only runs
// when the pane feature is enabled.
func (r *topoReconciler) reconcilePanes(ctx context.Context, snap *proto.Snapshot) {
	if !r.paneCfg.Enabled {
		return
	}
	if r.panes == nil {
		r.panes = map[string]*paneState{}
	}
	reqs, err := panereq.ReadAll(r.paneDir)
	if err != nil {
		slog.Warn("topology: pane requests unreadable", "err", err)
		return
	}
	bySession := make(map[string]panereq.Request, len(reqs))
	for _, q := range reqs {
		bySession[q.Session] = q
	}
	turns := turnState(snap)
	runners := runnerState(snap)
	anchored := snapshotAnchored(snap)

	// Withdraw requests the daemon can prove are finished. The Stop hook is
	// the normal path (bin/zdev-notify closes the request itself), so this is
	// the backstop for the two cases where no hook will ever fire: the agent
	// died, or the request has aged out. Retiring on "looks idle" is exactly
	// what this must NOT do — see turnState.
	nowUnix := r.now().Unix()
	for session, q := range bySession {
		reason := ""
		switch {
		case !turnLiveFor(turns, session):
			reason = "agent died"
		case r.paneCfg.MaxAgeSec > 0 && q.TS > 0 && nowUnix-q.TS > int64(r.paneCfg.MaxAgeSec):
			reason = "request aged out"
		}
		if reason == "" {
			continue
		}
		if err := panereq.Close(r.paneDir, session); err != nil {
			slog.Warn("topology: withdrawing pane request failed", "session", session, "err", err)
		} else {
			slog.Info("topology: pane request withdrawn", "session", session, "reason", reason)
		}
		delete(bySession, session)
		r.paneStateFor(session).vetoed = false
	}

	for _, wid := range r.eng.allWindows(ctx) {
		v, ok := r.eng.paneView(ctx, wid, r.paneDir, bySession, turns)
		if !ok {
			continue
		}
		session := v.Window.Session
		st := r.paneStateFor(session)

		_, havePane := paneViewport(v)
		// Veto detection: it was there, it is wanted, and it is gone.
		if st.sawPane && !havePane && v.Requested && v.TurnLive {
			if !st.vetoed {
				slog.Info("topology: operator closed the agent pane — suppressed for this turn",
					"session", session)
			}
			st.vetoed = true
		}
		v.Vetoed = st.vetoed

		plan := layout.PlanPanes(v, r.paneCfg, nowUnix)
		appliedPlan := plan
		if len(plan) > 0 {
			if err := r.eng.apply(ctx, plan); err != nil {
				slog.Warn("topology: pane apply failed", "window", wid, "err", err)
				appliedPlan = nil
			} else {
				slog.Info("topology: pane reconciled", "session", session, "commands", len(plan))
			}
		}
		// Record what the NEXT pass compares against. Re-gathering just to
		// know whether the pane exists would double the tmux cost; the plan
		// tells us what it did.
		st.sawPane = expectedPane(havePane, appliedPlan)

		_, haveLogs := logsViewport(v.Window)
		runnerUp := runners[session]
		// A down edge clears suppression; the next up edge may open again.
		manualClose := advanceLogsState(st, haveLogs, runnerUp, anchored)
		if manualClose {
			if !st.logsSuppressed {
				slog.Info("topology: operator closed logs pane — suppressed until runner cycles", "session", session)
			}
			st.logsSuppressed = true
		}
		logsAttach := ""
		if r.paneCfg.LogsCommand != "" {
			logsAttach = r.eng.logsAttachCommand(session)
		}
		logsPlan := layout.PlanLogs(layout.LogsView{
			Window: v.Window, RunnerUp: runnerUp, Suppressed: st.logsSuppressed,
			Anchored: anchored, AttachCommand: logsAttach,
		}, r.paneCfg)
		// Both planners gathered the same geometry. If the requested viewport
		// just split the donor, defer logs until the resulting tmux snapshot so
		// its floor check uses the real post-split height.
		if expectedPane(havePane, plan) != havePane {
			logsPlan = nil
		}
		if len(logsPlan) > 0 {
			if err := r.eng.apply(ctx, logsPlan); err != nil {
				slog.Warn("topology: logs pane apply failed", "window", wid, "err", err)
				logsPlan = nil
			} else {
				slog.Info("topology: logs pane reconciled", "session", session, "commands", len(logsPlan))
			}
		}
		st.sawLogs = expectedPane(haveLogs, logsPlan)
		st.runnerUp = runnerUp
	}
}

// advanceLogsState applies the edge-sensitive half of suppression. It returns
// true only for disappearance while the condition still stands; a down edge
// clears the veto so the next up edge may earn the pane again.
func advanceLogsState(st *paneState, haveLogs, runnerUp, anchored bool) bool {
	if st.runnerUp && !runnerUp {
		st.logsSuppressed = false
	}
	return st.sawLogs && !haveLogs && runnerUp && !anchored
}

func runnerState(snap *proto.Snapshot) map[string]bool {
	out := map[string]bool{}
	if snap == nil {
		return out
	}
	for _, p := range snap.Projects {
		out[p.Name] = len(p.ListeningPorts) > 0
	}
	return out
}

func logsViewport(w layout.Window) (layout.Pane, bool) {
	for _, p := range w.Panes {
		if p.LogsOpt != "" {
			return p, true
		}
	}
	return layout.Pane{}, false
}

func (r *topoReconciler) paneStateFor(session string) *paneState {
	if r.panes == nil {
		r.panes = map[string]*paneState{}
	}
	st, ok := r.panes[session]
	if !ok {
		st = &paneState{}
		r.panes[session] = st
	}
	return st
}

// paneViewport reports whether the window already holds a tagged viewport.
func paneViewport(v layout.PaneView) (layout.Pane, bool) {
	for _, p := range v.Window.Panes {
		if p.PaneOpt != "" {
			return p, true
		}
	}
	return layout.Pane{}, false
}

// expectedPane derives the post-apply presence of a viewport from the plan,
// so veto detection needs no second gather.
func expectedPane(had bool, plan []layout.Command) bool {
	for _, c := range plan {
		switch c.Args[0] {
		case "split-window":
			return true
		case "kill-pane":
			return false
		}
	}
	return had
}
