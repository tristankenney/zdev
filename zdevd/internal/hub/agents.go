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
	"time"

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
		// Transition INTO waiting — stamp the moment so the acknowledgment
		// rule (lastVisitTS >= WaitStartedTS) can correctly distinguish
		// "user has seen this wait" from "agent just started waiting again".
		// Don't override a value already set by NotifSeen (zdev-notify file
		// timestamp): that's the more accurate origin time when available.
		//
		// Reset bitmap BEFORE stamping WaitStartedTS — replay-safe ordering:
		// a crash between these two lines leaves (waiting=false, bits=0), not
		// (waiting=true, stale bits set).
		pd.WaitNotifiedTiers = 0
		if pd.WaitStartedTS == 0 {
			pd.WaitStartedTS = time.Now().Unix()
		}
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
	case prevWaiting && !nowWaiting:
		// Transition OUT of waiting — clear the timestamp so a future re-entry
		// cleanly advances WaitStartedTS past prior visits, drop the captured
		// context so it doesn't go stale, and reset the tier bitmap so the
		// next wait cycle starts firing from the lowest tier again.
		//
		// IMPORTANT: this branch must NOT fire on the merely-still-not-waiting
		// case (prevWaiting=false, nowWaiting=false). Bootstrap after a daemon
		// restart calls recomputeAgents for each session BEFORE pane titles
		// arrive — the fresh in-memory pd has AgentClaude/AgentPi == "" so
		// prevWaiting=false, and with no titles yet, nowWaiting=false too. If
		// this branch fired in that case, it would wipe the WaitStartedTS just
		// restored from zdevd-state.json. Then once the "● claude" title
		// arrives, the transition-INTO branch would re-stamp WaitStartedTS to
		// time.Now(), making lastVisitTS look stale and the chip re-flash.
		// Restricting to (prevWaiting && !nowWaiting) means we only clear on
		// an actual exit from waiting we observed.
		//
		// Visit guard: only wipe WaitStartedTS if the user has actually
		// visited the session since the wait started. Otherwise a brief
		// transition out of waiting (sub-agent returning, autonomous
		// follow-up) would reset the wait-age clock and the tier-escalation
		// bitmap — so when the agent flips back to waiting moments later,
		// the user sees a fresh "0s" instead of the accumulated age. The
		// next ClientSessionChanged advances lastVisitTS and lets a
		// subsequent transition clear cleanly.
		if visitTS, ok := s.lastVisitTS[sessionName]; ok && visitTS >= pd.WaitStartedTS {
			pd.WaitStartedTS = 0
			pd.WaitContext = ""
			pd.WaitNotifiedTiers = 0
		}
	}
	s.projectData[sessionName] = pd
}
