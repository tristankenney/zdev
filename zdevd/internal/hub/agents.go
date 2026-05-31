// internal/hub/agents.go
//
// Agent-status recomputation for a session — extracted from state.go in
// staff-review PR #4 (Arch CRITICAL #2). The pre-split file had 1021 LOC
// with three large functions in one place; this split keeps state.go for
// types + applyEvent and isolates the agent state machine here.
//
// recomputeAgents is invoked by applyEvent (state.go) whenever an event
// could change the per-pane agent status of a session: pane title changes,
// window/pane attach, session changes. Pure mutation — runs on the hub
// goroutine, no I/O except via asyncCapture (which dispatches off-thread).
package hub

import (
	"log/slog"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// recomputeAgents walks every pane in the named session, classifies each
// pane's title via tmuxctl.ClassifyAgent + ClassifyPaneTitle, and updates
// projectData[sessionName].AgentClaude / AgentPi accordingly.
//
// Transition handling:
//   - !prevWaiting && nowWaiting → stamp WaitStartedTS (if zero), reset the
//     WaitNotifiedTiers bitmap (replay-safe ordering), and dispatch a pane
//     capture so WaitContext gets populated for the snapshot.
//   - prevWaiting && !nowWaiting → clear WaitStartedTS, WaitContext, and the
//     tier bitmap. Restricted to observed-exit only so a daemon-restart
//     replay can't wipe restored state before pane titles arrive.
//
// Pane capture: prefers the production async path (state.asyncCapture)
// which dispatches off the hub goroutine so a slow tmux can't stall event
// processing for up to 1.5s per wait-start. Tests fall back to the
// synchronous paneCapturer when asyncCapture is nil.
func recomputeAgents(s *state, sessionName string) {
	sess, ok := sessionByName(s, sessionName)
	if !ok {
		// Session no longer exists — clear chip attribution.
		pd := s.projectData[sessionName]
		pd.AgentClaude = ""
		pd.AgentPi = ""
		s.projectData[sessionName] = pd
		return
	}
	var claudeWait, claudeDone, piWait, piDone bool
	for _, w := range sess.windows {
		for paneID := range w.panesIDs {
			p, ok := s.panesByID[paneID]
			if !ok {
				continue
			}
			// Agent attribution and pane-status are orthogonal: a pane can
			// be classified as an agent (claude/pi) AND have any status
			// (waiting/finished/shell-running/alive). Only StatusWaiting and
			// StatusFinished translate to a visible agent chip; idle prompts
			// (StatusShellRunning for the literal "✳ Claude Code" form) and
			// working spinners (Braille = StatusShellRunning) leave the chip
			// empty so the user isn't pulsed at for sessions where the agent
			// is alive but not demanding a response.
			agent := tmuxctl.ClassifyAgent(p.Title)
			if agent == "" {
				continue
			}
			status := tmuxctl.ClassifyPaneTitle(p.Title)
			switch agent {
			case "claude":
				switch status {
				case tmuxctl.StatusFinished:
					claudeDone = true
				case tmuxctl.StatusWaiting:
					claudeWait = true
				}
			case "pi":
				switch status {
				case tmuxctl.StatusFinished:
					piDone = true
				case tmuxctl.StatusWaiting:
					piWait = true
				}
			}
		}
	}
	pd := s.projectData[sessionName]
	prevWaiting := pd.AgentClaude == "waiting" || pd.AgentPi == "waiting"
	switch {
	case claudeWait:
		pd.AgentClaude = "waiting"
	case claudeDone:
		pd.AgentClaude = "finished"
	default:
		pd.AgentClaude = ""
	}
	switch {
	case piWait:
		pd.AgentPi = "waiting"
	case piDone:
		pd.AgentPi = "finished"
	default:
		pd.AgentPi = ""
	}
	nowWaiting := pd.AgentClaude == "waiting" || pd.AgentPi == "waiting"
	switch {
	case !prevWaiting && nowWaiting:
		// Transition INTO waiting — fire the side effects only. The
		// canonical WaitStartedTS lifecycle and tier-escalation are now
		// owned by hub.DeriveAttention (snapshot.go), which sees the same
		// title-change event one debounce later. The only event-time
		// effect that can't wait for the next snapshot is the pane
		// capture: it needs the agent pane content AS OF NOW, before the
		// user types anything that scrolls it away.
		//
		// phase4-v2: capture the agent pane content so the user can see what
		// the agent is waiting on without switching sessions. Prefer claude over
		// pi on tiebreak. Skip for daemon-internal sessions.
		if !shouldSkipSession(sessionName) {
			capturePaneID := ""
			for _, w := range sess.windows {
				for pid := range w.panesIDs {
					p, ok := s.panesByID[pid]
					if !ok {
						continue
					}
					if tmuxctl.ClassifyAgent(p.Title) == "claude" &&
						tmuxctl.ClassifyPaneTitle(p.Title) == tmuxctl.StatusWaiting {
						capturePaneID = pid
						break // claude wins — stop search
					}
				}
				if capturePaneID != "" {
					break
				}
			}
			// Fallback: pi pane.
			if capturePaneID == "" {
				for _, w := range sess.windows {
					for pid := range w.panesIDs {
						p, ok := s.panesByID[pid]
						if !ok {
							continue
						}
						if tmuxctl.ClassifyAgent(p.Title) == "pi" &&
							tmuxctl.ClassifyPaneTitle(p.Title) == tmuxctl.StatusWaiting {
							capturePaneID = pid
							break
						}
					}
					if capturePaneID != "" {
						break
					}
				}
			}
			if capturePaneID != "" {
				if s.asyncCapture != nil {
					// Production path: dispatch the capture off the hub
					// goroutine. The worker re-enters via
					// h.Submit(PaneCaptureReady{...}) and applyEvent's
					// PaneCaptureReady case writes the text onto
					// pd.WaitContext. WaitContext stays "" until the
					// capture returns; that's an acceptable ~50ms (worker
					// + debounce) delay before the captured text appears
					// in a snapshot, and replaces a 1.5s worst-case stall
					// of the entire hub goroutine on every wait-start.
					s.asyncCapture(sessionName, capturePaneID)
				} else if s.paneCapturer != nil {
					// Fallback (tests, hubs constructed without asyncCapture
					// wiring): synchronous capture. Bounded by the 1.5s
					// timeout inside realPaneCapture.
					captured, cerr := s.paneCapturer(capturePaneID)
					if cerr != nil {
						slog.Warn("hub: capture-pane failed",
							"err", cerr, "pane", capturePaneID, "project", sessionName)
					} else {
						pd.WaitContext = captured
					}
				}
			}
		}
	}
	// Transition OUT of waiting is handled in buildSnapshot, where the
	// canonical decision lives — DeriveAttention produces the new
	// WaitStartedTS (0 on exit, with latch-until-visit semantics) and
	// the snapshot loop cascades the dependent clears (WaitContext,
	// WaitNotifiedTiers) when WaitStartedTS transitions back to 0.
	s.projectData[sessionName] = pd
}
