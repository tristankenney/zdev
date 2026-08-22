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
// pane's title through state.agents (agents.Registry.Classify) once per
// pane, and writes the per-agent status map onto projectData.AgentStates.
//
// buildSnapshot copies pd.AgentStates onto proto.Project.AgentStates so
// the wire carries the full map.
//
// Transition handling (set-diff against the prior AgentStates):
//   - For any agent whose status flips from non-"waiting" to "waiting",
//     this counts as an enter-waiting edge for the session. The pane
//     capture dispatches off the first such agent's waiting pane, in
//     registry declaration order — generalising the prior claude-before-pi
//     hard-coded tiebreak.
//   - WaitStartedTS lifecycle + WaitContext clear-on-exit + tier-bitmap
//     reset are owned by DeriveAttention (snapshot.go); recomputeAgents
//     only fires the side effect that can't wait for the next debounce —
//     the pane capture itself, which needs the agent pane content AS OF
//     NOW before the user types anything that scrolls it away.
func recomputeAgents(s *state, sessionName string) {
	sess, ok := sessionByName(s, sessionName)
	if !ok {
		// Session no longer exists — clear chip attribution.
		pd := s.projectData[sessionName]
		pd.AgentStates = nil
		s.projectData[sessionName] = pd
		return
	}

	// Walk every pane once, classify via the registry, and bucket the
	// resulting (agent, status) pair into per-agent priority slots.
	// Priority within an agent: waiting > finished > shell-running > "".
	// A single pane can only contribute one slot for its attributed agent.
	//
	// paneByAgent records the FIRST pane (in iteration order) that drove the
	// agent into the waiting bucket — used for the enter-waiting capture
	// dispatch below. Iteration order across windows/panesIDs maps is not
	// stable in Go, but registry declaration order IS — so we pick the
	// capture pane by walking s.agents.All() and selecting whichever agent
	// declared first has a waiting pane recorded.
	type agentBucket struct {
		hasWaiting, hasFinished, hasSpinner bool
		waitingPaneID                       string
	}
	buckets := make(map[string]*agentBucket)

	for _, w := range sess.windows {
		for paneID := range w.panesIDs {
			p, ok := s.panesByID[paneID]
			if !ok {
				continue
			}
			// Registry → name attribution: "which agent owns this title?"
			// ClassifyPaneTitle → authoritative status: the registry's status
			// would surface "✳ Claude Code" as waiting because it matches
			// the "✳ " WaitingMarker, but ClassifyPaneTitle's literal-string
			// carve-out demotes that specific idle-prompt title to alive so
			// 19 idle Claude sessions don't pulse on every daemon restart.
			// Keep the two responsibilities separate; agents.Registry is for
			// attribution, tmuxctl.ClassifyPaneTitle for status semantics.
			name, status := s.agents.Classify(p.Title)
			if name == "" {
				continue
			}
			// Claude Code uses this exact title for its idle prompt even
			// though the broad "✳ " marker otherwise means waiting.
			if name == "claude" && p.Title == "✳ Claude Code" {
				continue
			}
			b := buckets[name]
			if b == nil {
				b = &agentBucket{}
				buckets[name] = b
			}
			switch status {
			case tmuxctl.StatusWaiting:
				if !b.hasWaiting {
					b.waitingPaneID = paneID
				}
				b.hasWaiting = true
			case tmuxctl.StatusFinished:
				b.hasFinished = true
			case tmuxctl.StatusShellRunning:
				b.hasSpinner = true
			}
		}
	}

	pd := s.projectData[sessionName]
	prev := pd.AgentStates
	// Walking the registry in declaration order keeps AgentStates' insertion
	// order deterministic across recompute passes — important because Go
	// map iteration is randomized and any consumer that picks "the first
	// agent" (e.g. enter-waiting capture below, A6's renderer chip order)
	// needs the registry's order, not whatever Go decided this run.
	next := make(map[string]string, len(buckets))
	for _, spec := range s.agents.All() {
		b, ok := buckets[spec.Name]
		if !ok {
			continue
		}
		var status string
		switch {
		case b.hasWaiting:
			status = "waiting"
		case b.hasFinished:
			status = "finished"
		case b.hasSpinner:
			// Mirrors the agent attribution path used by recomputeAgents
			// historically — only Waiting and Finished produce a visible
			// chip; the spinner bucket exists for completeness but emits
			// no per-agent state entry today.
			continue
		default:
			continue
		}
		next[spec.Name] = status
	}

	// Enter-waiting detection: any agent that wasn't waiting in `prev` but
	// IS waiting in `next` is a transition. Pick the capture pane from the
	// FIRST such agent in registry declaration order. Generalises the
	// claude-before-pi hard-coded tiebreak.
	enteredWaiting := ""
	for _, spec := range s.agents.All() {
		if next[spec.Name] != "waiting" {
			continue
		}
		if prev[spec.Name] == "waiting" {
			continue
		}
		enteredWaiting = spec.Name
		break
	}

	pd.AgentStates = next

	if enteredWaiting != "" && !shouldSkipSession(sessionName) {
		// phase4-v2: capture the agent pane content so the user can see what
		// the agent is waiting on without switching sessions. The pane to
		// capture is the first one we saw drive `enteredWaiting` to waiting
		// during the bucket walk above.
		capturePaneID := buckets[enteredWaiting].waitingPaneID
		if capturePaneID != "" {
			// zd-47u: route the capture through the session's source tmux
			// socket. Default-socket sessions resolve to "" which yields a
			// plain `tmux capture-pane …` (no -L flag); GT-socket sessions
			// resolve to the GT socket name and the capturer prepends
			// `-L <socket>`. Without this lookup, GT-socket capture fails
			// against the default socket and WaitContext stays empty.
			socketName := s.sessionSocket[sessionName]
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
				s.asyncCapture(sessionName, capturePaneID, socketName)
			} else if s.paneCapturer != nil {
				// Fallback (tests, hubs constructed without asyncCapture
				// wiring): synchronous capture. Bounded by the 1.5s
				// timeout inside realPaneCapture.
				captured, cerr := s.paneCapturer(capturePaneID, socketName)
				if cerr != nil {
					slog.Warn("hub: capture-pane failed",
						"err", cerr, "pane", capturePaneID, "project", sessionName, "socket", socketName)
				} else {
					pd.WaitContext = captured
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
