// internal/hub/snapshot.go
//
// Snapshot construction + status derivation + port-diff eventlog emission.
// Extracted from state.go in staff-review PR #4 (Arch CRITICAL #2). All
// functions here are read-only against the state model — they don't
// mutate; they produce wire-format outputs (proto.Snapshot, eventlog
// events).
package hub

import (
	"sort"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// buildSnapshot serializes the current state into a proto.Snapshot.
// DATA-10: project list (slash-form canonical) is the row source; tmux
// sessions union in for any not in the project list. Slash-form entries
// suppress dash-form twins so the same project never appears twice.
//
// now is the current unix-second timestamp, threaded in so callers share a
// single deterministic value per pass. Used to populate WaitAcknowledged via
// isWaitAcknowledged (state.go) for each waiting project.
//
// nowMS is the current unix-MILLISECOND timestamp, threaded in for the
// sub-second status-dwell debounce (applyDwell). It is a separate parameter
// rather than now*1000 so callers/tests can drive dwell timing precisely
// without disturbing the second-resolution wait/tier logic.
func buildSnapshot(st *state, seq int64, sentAt time.Time, now, nowMS int64) *proto.Snapshot {
	nameToSession := make(map[string]*session, len(st.sessions))
	for _, sess := range st.sessions {
		if sess.Name == "" {
			// Empty-name sessions appear when a SessionChanged event arrives
			// during tmux bootstrap before SessionRenamed binds a name. The
			// record stays in state.sessions keyed by ID for later rename,
			// but it must not surface as a blank sidebar row.
			continue
		}
		if sess.Name == "zdevd-watcher" {
			continue // D2-05 filter
		}
		if sess.ID == "$_unlinked" {
			continue // Phase 2 simplification
		}
		nameToSession[sess.Name] = sess
	}

	// DATA-10: project list is the canonical row source (D-03 — slash-form
	// names). UNION with session-only entries so unlinked tmux sessions still
	// surface. Slash-form project entries suppress their dash-form session
	// twins so "example/backend" and "example-backend" never both appear.
	seen := make(map[string]struct{}, len(st.projectListNames)+len(nameToSession))
	names := make([]string, 0, len(st.projectListNames)+len(nameToSession))

	// Pass 1: project list names (slash-form canonical, D-03)
	for _, n := range st.projectListNames {
		if _, ok := seen[n]; ok {
			continue
		}
		names = append(names, n)
		seen[n] = struct{}{}
		seen[proto.SessionKey(n)] = struct{}{} // block dash-form twin
	}

	// Pass 2: sessions not covered by the project list. Filter out the
	// daemon's watcher session and synthetic test/control sessions
	// (raw-events-*, sub-test-*, test-control-*) — these are infrastructure
	// from the live-test harness and have no agent panes worth surfacing.
	// Also skip empty-name sessions (see Pass 1 comment on bootstrap timing).
	for _, sess := range st.sessions {
		if sess.Name == "" || shouldSkipSession(sess.Name) || sess.ID == "$_unlinked" {
			continue
		}
		if _, ok := seen[sess.Name]; ok {
			continue
		}
		names = append(names, sess.Name)
		seen[sess.Name] = struct{}{}
	}
	sort.Strings(names)

	projects := make([]proto.Project, 0, len(names))
	for _, n := range names {
		// D-02 (Phase 999.1): nameToSession, projectData, prCounts, and
		// celebrateUntil are all keyed by tmux session Name (dash-form, e.g.
		// "example-backend"). st.projectListNames stores slash-form
		// (e.g., "example/backend"). Normalize to dash-form for all data
		// lookups; proto.Project.Name retains the canonical slash-form display name.
		dataKey := proto.SessionKey(n)
		pd := st.projectData[dataKey]
		pr := st.prCounts[dataKey]

		// Existence flag — "absent" means the tmux session has no panes
		// (or no entry at all). DeriveAttention has no notion of absence
		// because it works on titles; carve it out up-front. When absent,
		// the wire Status is "absent" and Attention is the zero value
		// (AttIdle); the renderer treats absent as a dim row.
		sess := nameToSession[dataKey]
		absent := sess == nil || len(sess.windows) == 0

		// Single-source-of-truth: DeriveAttention reads pane titles +
		// visit/title-change timestamps and produces the per-session UX
		// state plus the wait-start timestamp. The legacy Status string
		// is kept in the wire payload (back-compat for renderers still
		// reading it) but is now a projection of Attention via
		// AttentionToStatus — no independent decision logic.
		//
		// displayAtt is what the renderer sees: the dwell-debounced
		// projection of the derived value. For absent sessions it stays the
		// zero value (AttIdle) and Status is overridden to "absent" below.
		var displayAtt proto.Attention
		if !absent {
			prevWaitStartedTS := pd.WaitStartedTS
			ar := DeriveAttention(AttentionInputs{
				Titles:            sessionTitles(st, sess),
				LastVisitTS:       st.lastVisitTS[dataKey],
				LastTitleChangeTS: st.lastTitleChangeTS[dataKey],
				WaitStartedTS:     pd.WaitStartedTS,
				// Feed the latch from the raw derived history, NOT the
				// debounced display value — the DeriveAttention state machine
				// must continue from its own last output regardless of what
				// the dwell layer is currently showing.
				PrevAttention: pd.AttentionDerived,
			}, now)
			pd.AttentionDerived = ar.Attention
			pd.WaitStartedTS = ar.WaitStartedTS

			// Minimum-dwell debounce (status flap suppression): only promote a
			// derived transition to the displayed Attention once it has held
			// for st.statusDwell. A working→waiting→working blip inside the
			// window never surfaces. With statusDwell == 0 this is a pass-
			// through and displayAtt == ar.Attention.
			committed, pendCand, pendSince := applyDwell(
				pd.Attention, pd.AttentionInit, ar.Attention,
				pd.PendingAttention, pd.PendingSinceMS,
				nowMS, st.statusDwell.Milliseconds(),
			)
			pd.Attention = committed
			pd.AttentionInit = true
			pd.PendingAttention = pendCand
			pd.PendingSinceMS = pendSince
			displayAtt = committed

			// Cascade the wait-lifecycle dependents off WaitStartedTS.
			// When the wait clears (non-zero → 0), drop the captured pane
			// context so it doesn't go stale and reset the tier bitmap so
			// the next wait cycle escalates from the lowest tier again.
			// agents.go used to do this directly; now it's owned here so
			// "the wait state cleared" has a single point of authority. This
			// tracks the DERIVED value (ar), not the debounced display — the
			// wait lifecycle and its notifications must reflect the true
			// pane state, not the dwell-smoothed view.
			if prevWaitStartedTS != 0 && ar.WaitStartedTS == 0 {
				pd.WaitContext = ""
				pd.WaitNotifiedTiers = 0
			}

			st.projectData[dataKey] = pd
		}

		status := AttentionToStatus(displayAtt)
		if absent {
			status = tmuxctl.StatusAbsent
		}

		proj := proto.Project{
			Name:           n,
			Status:         status,
			Attention:      displayAtt,
			Branch:         pd.Branch,
			Ahead:          pd.Ahead,
			Behind:         pd.Behind,
			DirtyCount:     pd.DirtyCount,
			ShellCmd:       pd.ShellCmd,
			ListeningPorts: pd.Ports,
			LastActivityTS: pd.LastActivityTS,
			WaitStartedTS:    pd.WaitStartedTS,
			WaitAcknowledged: isWaitAcknowledged(st, dataKey, pd.WaitStartedTS, now),
			PROpen:           pr.Open,
			PRFail:         pr.Fail,
			PRPend:         pr.Pend,
			FailingChecks:  pr.FailingChecks,
			PendingChecks:  pr.PendingChecks,
			CelebrateUntil: st.celebrateUntil[dataKey],
			AgentStates:    projectAgentStates(pd.AgentStates),
			AgentClaude:    pd.AgentClaude,
			AgentPi:        pd.AgentPi,
			WaitContext:    pd.WaitContext,
			CIStatus:       pd.CIStatus,
			CIConclusion:   pd.CIConclusion,
		}
		projects = append(projects, proj)
	}

	return &proto.Snapshot{
		V:              proto.CurrentProtocolVersion,
		Type:           "snapshot",
		Schema:         proto.SchemaVersion, // single source of truth (Phase 3)
		Seq:            seq,
		SentAt:         sentAt,
		Sessions:       names,
		Projects:       projects,
		CurrentSession: "", // resolved per-connection in Plan 02-04 from hello.TmuxPane
	}
}

// projectAgentStates lifts the per-agent raw status strings stored on
// projectData (recomputeAgents writes "waiting" / "finished" / …) into the
// proto.Attention enum used on the wire. Returns nil when the input map is
// empty so projects with no recognised agent panes don't carry an empty
// agent_states JSON object.
//
// Status → Attention mapping:
//
//	"waiting"       → AttWaiting
//	"finished"      → AttFinished
//	"shell-running" → AttWorking
//	anything else   → skipped (empty/unknown values do not surface)
func projectAgentStates(src map[string]string) map[string]proto.Attention {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]proto.Attention, len(src))
	for name, status := range src {
		var att proto.Attention
		switch status {
		case "waiting":
			att = proto.AttWaiting
		case "finished":
			att = proto.AttFinished
		case "shell-running":
			att = proto.AttWorking
		default:
			continue
		}
		out[name] = att
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// earliestDwellDeadlineMS returns the soonest unix-millisecond time at which
// any project's pending dwell candidate will be promoted to its displayed
// Attention, or 0 if no candidate is pending (or the debounce is disabled).
//
// The hub uses this to arm a one-shot timer so a genuine, sustained
// transition is committed promptly when its window elapses — without it, a
// pending status would only surface on the next event or the supervisor's
// 1Hz heartbeat, adding up to a second of latency to every real change.
func earliestDwellDeadlineMS(st *state) int64 {
	dwellMS := st.statusDwell.Milliseconds()
	if dwellMS <= 0 {
		return 0
	}
	var earliest int64
	for _, pd := range st.projectData {
		if pd.PendingSinceMS == 0 {
			continue
		}
		deadline := pd.PendingSinceMS + dwellMS
		if earliest == 0 || deadline < earliest {
			earliest = deadline
		}
	}
	return earliest
}

// emitPortDiff fires one eventlog.Event per port that opened (in `now`
// but not `prev`) and per port that closed (in `prev` but not `now`).
// Closes come first, then opens; both are sorted ascending so the output
// is deterministic regardless of input order. The Session field carries
// the project name (Phase 3 mapping: project name == tmux session name
// once the "/" → "-" substitution is applied at the probe-producer side).
func emitPortDiff(emit func(eventlog.Event), project string, prev, now []int) {
	prevSet := make(map[int]struct{}, len(prev))
	for _, p := range prev {
		prevSet[p] = struct{}{}
	}
	nowSet := make(map[int]struct{}, len(now))
	for _, p := range now {
		nowSet[p] = struct{}{}
	}
	var closed, opened []int
	for p := range prevSet {
		if _, stillOpen := nowSet[p]; !stillOpen {
			closed = append(closed, p)
		}
	}
	for p := range nowSet {
		if _, wasOpen := prevSet[p]; !wasOpen {
			opened = append(opened, p)
		}
	}
	sort.Ints(closed)
	sort.Ints(opened)
	ts := time.Now().UTC()
	for _, p := range closed {
		emit(eventlog.Event{
			Ts: ts, Type: "port-change",
			Session: project, Port: p, Op: "close",
		})
	}
	for _, p := range opened {
		emit(eventlog.Event{
			Ts: ts, Type: "port-change",
			Session: project, Port: p, Op: "open",
		})
	}
}

// deriveStatus walks the session's panes through the bash baseline status
// hierarchy: waiting > shell-running > finished > alive > absent.
//
// Source: ~/.local/bin/zdev-sidebar-render line 484 — verbatim port of
// the precedence rule. Each pane title is classified via
// tmuxctl.ClassifyPaneTitle (Plan 02-08 Task 8.1).
//
// Phase 2 simplification: the bash baseline uses pane_current_command as
// the shell-running signal; Phase 2 uses the `◎ ` glyph prefix in the
// title (which zdev's pane-titling tooling already emits). Phase 3 lands
// pane_current_command via a second format subscription (DATA-03).
// sessionTitles returns the title of every pane owned by the named session.
// Returns nil when the session is unknown or has no windows (a session
// freshly created by SessionChanged but not yet populated).
func sessionTitles(st *state, s *session) []string {
	if s == nil || len(s.windows) == 0 {
		return nil
	}
	var titles []string
	for _, w := range s.windows {
		for paneID := range w.panesIDs {
			if p, ok := st.panesByID[paneID]; ok {
				titles = append(titles, p.Title)
			}
		}
	}
	return titles
}

func deriveStatus(st *state, s *session) string {
	if s == nil || len(s.windows) == 0 {
		return tmuxctl.StatusAbsent
	}

	// Collect titles for all panes owned by this session.
	var titles []string
	for _, w := range s.windows {
		for paneID := range w.panesIDs {
			if p, ok := st.panesByID[paneID]; ok {
				titles = append(titles, p.Title)
			}
		}
	}

	// Priority order: waiting > shell-running > finished > alive.
	for _, t := range titles {
		if tmuxctl.ClassifyPaneTitle(t) == tmuxctl.StatusWaiting {
			return tmuxctl.StatusWaiting
		}
	}
	for _, t := range titles {
		if tmuxctl.ClassifyPaneTitle(t) == tmuxctl.StatusShellRunning {
			return tmuxctl.StatusShellRunning
		}
	}
	for _, t := range titles {
		if tmuxctl.ClassifyPaneTitle(t) == tmuxctl.StatusFinished {
			return tmuxctl.StatusFinished
		}
	}
	return tmuxctl.StatusAlive
}
