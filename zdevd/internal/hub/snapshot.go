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
	"github.com/tristankenney/zdev/zdevd/internal/teams"
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
	// sessIndex owns name→session resolution: the empty-name / watcher /
	// synthetic / $_unlinked skip rules, the deterministic same-name
	// collision winner (a ghost record racing its SessionsListed prune must
	// not flap row status via random map iteration — dogfood 2026-06-12),
	// and the slash/dash canonicalization used by lookupProject. See
	// sessindex.go.
	sessIndex := buildSessionIndex(st)

	// DATA-10: project list is the canonical row source (D-03 — slash-form
	// names). UNION with session-only entries so unlinked tmux sessions still
	// surface. Slash-form project entries suppress their dash-form session
	// twins so "example/backend" and "example-backend" never both appear.
	seen := make(map[string]struct{}, len(st.projectListNames)+len(st.sessions))
	names := make([]string, 0, len(st.projectListNames)+len(st.sessions))

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
	//
	// When showUnmanaged is true: collect these into unmanagedNames so they
	// sort after the managed block and are rendered dim. When false (default):
	// append directly to names so the current mixed-sort behaviour is preserved
	// exactly — zero behaviour change when the feature is disabled.
	var unmanagedNames []string
	for _, sess := range st.sessions {
		if skipForIndex(sess) {
			continue
		}
		if _, ok := seen[sess.Name]; ok {
			continue
		}
		seen[sess.Name] = struct{}{}
		if st.showUnmanaged {
			unmanagedNames = append(unmanagedNames, sess.Name)
		} else {
			names = append(names, sess.Name)
		}
	}
	sort.Strings(names)
	sort.Strings(unmanagedNames) // no-op when hide (default)

	// Lead de-aggregation (Agent Teams slice B): when teamWindows is on,
	// collect the panes claimed by team members once so each session's
	// attention derivation can skip them — the lead row then reflects the
	// lead only. nil when the knob is off or no team owns a real pane, in
	// which case sessionTitlesExcluding behaves exactly as sessionTitles.
	var excludeMemberPanes map[string]struct{}
	if st.teamWindows {
		excludeMemberPanes = teamMemberPaneIDs(st)
	}

	// Build the unmanaged set for O(1) lookup when assembling projects.
	unmanagedSet := make(map[string]struct{}, len(unmanagedNames))
	for _, n := range unmanagedNames {
		unmanagedSet[n] = struct{}{}
	}

	// Fresh backing array so snap.Sessions is never an alias of `names`.
	// The immutable-after-publish contract requires snapshots are not mutated
	// after publication (Invariant 8 / snapshotEqualsCore contract).
	allNames := make([]string, len(names)+len(unmanagedNames))
	copy(allNames, names)
	copy(allNames[len(names):], unmanagedNames)

	projects := make([]proto.Project, 0, len(allNames))
	for _, n := range allNames {
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
		sess := sessIndex.lookupProject(n)
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
				Titles:            sessionTitlesExcluding(st, sess, excludeMemberPanes),
				LastVisitTS:       st.lastVisitTS[dataKey],
				LastTitleChangeTS: st.lastTitleChangeTS[dataKey],
				WaitStartedTS:     pd.WaitStartedTS,
				// Feed the latch from the raw derived history, NOT the
				// debounced display value — the DeriveAttention state machine
				// must continue from its own last output regardless of what
				// the dwell layer is currently showing.
				PrevAttention: pd.AttentionDerived,
				// ...but the latch only ARMS for waits the system believes:
				// displayed (survived the waiting dwell) or hook-receipted.
				// A dwell-suppressed title blip is neither and must die
				// with its title.
				WaitConfirmed: pd.Attention == proto.AttWaiting ||
					(pd.HookWaitTS > 0 && now-pd.HookWaitTS <= hookWaitFreshSec),
			}, now)
			pd.AttentionDerived = ar.Attention
			pd.WaitStartedTS = ar.WaitStartedTS

			// Minimum-dwell debounce (status flap suppression): only promote a
			// derived transition to the displayed Attention once it has held
			// for the window. The window is TRANSITION-AWARE (dogfood
			// 2026-06-07 — flapping working→waiting→working between agent
			// commands):
			//
			//   into waiting, hook-confirmed → 0 (instant: the agent
			//     declared "I'm asking NOW" through zdev-notify; fresh
			//     HookWaitTS is the receipt)
			//   into waiting, title-only     → waitingDwell (~7s — must
			//     out-live one 5s title-poll period, because a single ✳
			//     blip sample stands unrefuted until the next poll; the
			//     old flat 250ms could never suppress it)
			//   every other transition       → statusDwell (250ms)
			dwellMS := dwellWindowMS(st, &pd, ar.Attention, now)
			committed, pendCand, pendSince := applyDwell(
				pd.Attention, pd.AttentionInit, ar.Attention,
				pd.PendingAttention, pd.PendingSinceMS,
				nowMS, dwellMS,
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
				pd.WaitKind = ""
				pd.WaitSummary = ""
			}

			// Death lifecycle (NOW#3): a title-derived working/waiting
			// attention clears the death ONLY when the title moved AFTER
			// the death was stamped — i.e. a restarted agent. The
			// strictly-newer guard closes a real race: the SessionEnd
			// hook fires while the dying pane's stale ✳/⠂ title still
			// exists (the pane dies milliseconds later), and a snapshot
			// pass between the two events would otherwise read the
			// corpse's leftover title as life. Otherwise an unresolved
			// death MASKS the displayed attention: it is hook-confirmed
			// (no flapping possible), so it sits outside the dwell
			// debounce as a final override rather than a dwell candidate.
			if pd.DeadSinceTS > 0 {
				alive := (ar.Attention == proto.AttWorking || ar.Attention == proto.AttWaiting) &&
					st.lastTitleChangeTS[dataKey] > pd.DeadSinceTS
				if alive {
					pd.DeadSinceTS = 0
					pd.DeadReason = ""
					pd.DeadNotified = false
				} else {
					displayAtt = proto.AttDead
				}
			}

			st.projectData[dataKey] = pd
		}

		status := AttentionToStatus(displayAtt)
		if absent {
			status = tmuxctl.StatusAbsent
		}

		// Dead rows reuse the wait wire fields (phase4-v11): the death
		// time rides WaitStartedTS so existing age rendering works, and
		// the exit reason rides WaitSummary as the triage gist.
		wireWaitStarted := pd.WaitStartedTS
		wireWaitSummary := pd.WaitSummary
		if displayAtt == proto.AttDead {
			wireWaitStarted = pd.DeadSinceTS
			wireWaitSummary = pd.DeadReason
		}

		_, isUnmanaged := unmanagedSet[n]
		proj := proto.Project{
			Name:             n,
			Status:           status,
			Attention:        displayAtt,
			Branch:           pd.Branch,
			Ahead:            pd.Ahead,
			Behind:           pd.Behind,
			DirtyCount:       pd.DirtyCount,
			ShellCmd:         pd.ShellCmd,
			ListeningPorts:   pd.Ports,
			LastActivityTS:   pd.LastActivityTS,
			WaitStartedTS:    wireWaitStarted,
			WaitAcknowledged: isWaitAcknowledged(st, dataKey, wireWaitStarted, now),
			WaitKind:         pd.WaitKind,
			WaitSummary:      wireWaitSummary,
			PROpen:           pr.Open,
			PRFail:           pr.Fail,
			PRPend:           pr.Pend,
			FailingChecks:    pr.FailingChecks,
			PendingChecks:    pr.PendingChecks,
			CelebrateUntil:   st.celebrateUntil[dataKey],
			AgentStates:      projectAgentStates(pd.AgentStates),
			AgentClaude:      pd.AgentClaude,
			AgentPi:          pd.AgentPi,
			WaitContext:      pd.WaitContext,
			CIStatus:         pd.CIStatus,
			CIConclusion:     pd.CIConclusion,
			Unmanaged:        isUnmanaged,
		}
		projects = append(projects, proj)
	}

	// TeamGroups (phase4-v16): Agent Teams with the lead resolved to a project
	// row by pane cwd. Computed once per pass (team count is tiny) and shared
	// with rankTriage so the queue's member entries agree with the wire groups.
	teamGroups := teamGroupsFor(st)

	return &proto.Snapshot{
		V:              proto.CurrentProtocolVersion,
		Type:           "snapshot",
		Schema:         proto.SchemaVersion, // single source of truth (Phase 3)
		Seq:            seq,
		SentAt:         sentAt,
		Sessions:       allNames,
		Projects:       projects,
		CurrentSession: "", // resolved per-connection in Plan 02-04 from hello.TmuxPane
		// Triage (phase4-v9) ranks the rows just assembled above, so the
		// queue always reflects exactly what the renderer draws —
		// including the dwell-debounced Attention. Computed here (not
		// per-subscriber) so every surface shares one ordering. Waiting team
		// members join the queue (slice C) only when teamWindows de-aggregates
		// them off the lead row, so a wait is never counted twice.
		Triage: rankTriage(projects, teamGroups, st.teamWindows, now),
		// Cursor (phase4-v14, zd-e6e): propagate hub cursor state so every
		// subscriber's renderer highlights the selected row consistently.
		CursorRow:    st.cursorRow,
		CursorActive: st.cursorActive,
		TeamGroups:   teamGroups,
		// TeamRows: the daemon's knob is the single row-order authority —
		// the renderer reads this, not its own env (see proto.Snapshot).
		TeamRows: st.teamWindows,
	}
}

// teamGroupsFor converts the hub's Agent Teams state to wire TeamGroups,
// sorted by team name for deterministic output. The lead anchors to a
// project row by cwd: the first session owning a pane whose Cwd equals
// the lead's cwd wins (the lead runs INSIDE some tmux pane — its claude
// session's cwd is that pane's cwd). No match → LeadProject "" and the
// renderer skips the badge (the team still rides the wire for zdev-show).
// The lead itself is excluded from Members — it IS the anchor.
func teamGroupsFor(st *state) []proto.TeamGroup {
	if len(st.agentTeams) == 0 {
		return nil
	}
	names := make([]string, 0, len(st.agentTeams))
	for n := range st.agentTeams {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]proto.TeamGroup, 0, len(names))
	for _, n := range names {
		t := st.agentTeams[n]
		g := proto.TeamGroup{Name: t.Name}
		if lead := t.Lead(); lead != nil && lead.CWD != "" {
			g.LeadProject = projectByPaneCwd(st, lead.CWD)
		}
		for _, m := range t.Members {
			if m.AgentType == "team-lead" {
				continue
			}
			inProcess := m.TmuxPaneID == teams.InProcessPaneID
			paneID := ""
			if !inProcess {
				paneID = m.TmuxPaneID
			}
			g.Members = append(g.Members, proto.TeamMember{
				Name:      m.Name,
				Color:     m.Color,
				InProcess: inProcess,
				PaneID:    paneID,
				// Status (v20): tmux-backend members classify their pane
				// title (the same signal the session-level derivation
				// reads) so a nested row can pinpoint WHO is working /
				// blocked / done; in-process members carry "idle" from
				// the lead-inbox idle_notification derivation, else "".
				Status: memberStatus(st, m, t.MemberIdle[m.Name]),
				// WindowID (v20): the window that owns the member's pane
				// after team-sweep relocates it into its own window —
				// empty for in-process members and unassociated panes.
				WindowID: windowIDForPane(st, paneID),
			})
		}
		out = append(out, g)
	}
	return out
}

// memberStatus derives the wire Status for one team member (proto.TeamMember
// vocabulary). For a tmux-backend member it classifies the pane title with
// the same tmuxctl.ClassifyPaneTitle the session-level derivation uses, and
// maps the result into the project attention vocabulary:
//
//	waiting       → "waiting" (blocked on input)
//	shell-running → "working"
//	finished      → "done"
//	alive / no pane / unknown → ""
//
// For an in-process member (no pane to read) the only signal is the lead's
// inbox: idle==true → "idle", else "". An in-process member is never
// "waiting"/"working"/"done" — those require a pane title.
func memberStatus(st *state, m teams.Member, idle bool) string {
	if m.TmuxPaneID == "" || m.TmuxPaneID == teams.InProcessPaneID {
		if idle {
			return "idle"
		}
		return ""
	}
	pn, ok := st.panesByID[m.TmuxPaneID]
	if !ok {
		return ""
	}
	switch tmuxctl.ClassifyPaneTitle(pn.Title) {
	case tmuxctl.StatusWaiting:
		return "waiting"
	case tmuxctl.StatusShellRunning:
		return "working"
	case tmuxctl.StatusFinished:
		return "done"
	default:
		return ""
	}
}

// windowIDForPane returns the tmux window id (`@<n>`) of the window that
// owns paneID, scanning every session's windows (the $_unlinked parking lot
// included — a mid-attach member pane parks there briefly). Returns "" for
// an empty paneID or a pane no window claims yet. Read-only; hub goroutine
// only. Determinism across the windowless-collision window is not required:
// a pane belongs to at most one window's panesIDs set at a time.
func windowIDForPane(st *state, paneID string) string {
	if paneID == "" {
		return ""
	}
	for _, sess := range st.sessions {
		for _, w := range sess.windows {
			if _, ok := w.panesIDs[paneID]; ok {
				return w.ID
			}
		}
	}
	return ""
}

// projectByPaneCwd returns the PROJECT ROW NAME anchoring dir: the first
// real session (sorted for determinism; infrastructure sessions and the
// $_unlinked bucket excluded — they'd otherwise win the race and name a
// row that doesn't exist) owning a pane whose Cwd matches dir exactly.
// Exact match only: prefix matching would mis-anchor teams running in
// subdirectories of one project to a sibling worktree.
//
// Session names are dash-form but managed project rows are slash-form
// (the SessionKey mapping) — the result is canonicalized back through
// projectListNames so the renderer's `p.Name == LeadProject` comparison
// works for managed projects; unmanaged sessions keep their session
// name, which IS their row name.
func projectByPaneCwd(st *state, dir string) string {
	// The session index owns the skip rules and the deterministic collision
	// winner; sortedNames gives the stable first-match order (an
	// infrastructure session sorting first must never win the anchor — that
	// was the slice-3 finding pinned by TestApplyEvent_TeamsChanged_SnapshotThreading).
	ix := buildSessionIndex(st)
	for _, name := range ix.sortedNames() {
		sess := ix.lookup(name)
		if sess == nil {
			continue
		}
		for _, w := range sess.windows {
			for pid := range w.panesIDs {
				if p, ok := st.panesByID[pid]; ok && p.Cwd == dir {
					for _, rowName := range st.projectListNames {
						if proto.SessionKey(rowName) == name {
							return rowName
						}
					}
					return name
				}
			}
		}
	}
	return ""
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
	if st.statusDwell.Milliseconds() <= 0 {
		return 0
	}
	// now for the hook-receipt freshness check: derive from the pending
	// stamp's own clock domain (unix ms → s). The deadline only needs to
	// be conservative — an early timer fire republished and re-arms.
	var earliest int64
	for _, pd := range st.projectData {
		if pd.PendingSinceMS == 0 {
			continue
		}
		// The candidate's REAL window is transition-aware: a pending
		// waiting candidate dwells for waitingDwell, not statusDwell.
		// Arming the timer at statusDwell for those spun a ~1ms wake
		// loop for the remaining ~6.75s (deadline already in the past
		// on every re-arm — caught by the invariants review).
		pdCopy := pd
		dwellMS := dwellWindowMS(st, &pdCopy, pd.PendingAttention, pd.PendingSinceMS/1000)
		if dwellMS <= 0 {
			dwellMS = st.statusDwell.Milliseconds()
		}
		deadline := pd.PendingSinceMS + dwellMS
		if earliest == 0 || deadline < earliest {
			earliest = deadline
		}
	}
	return earliest
}

// dwellWindowMS selects the dwell window for promoting `candidate` as
// the displayed attention — the single source of truth shared by the
// snapshot pass and the Run loop's timer arming (they MUST agree or the
// timer spins). Transitions into waiting take the long poll-aware
// window unless a fresh hook receipt confirms the wait; everything
// else takes the fast statusDwell.
func dwellWindowMS(st *state, pd *projectData, candidate proto.Attention, now int64) int64 {
	if candidate == proto.AttWaiting && pd.Attention != proto.AttWaiting {
		if pd.HookWaitTS > 0 && now-pd.HookWaitTS <= hookWaitFreshSec {
			return 0
		}
		if st.waitingDwell > 0 {
			return st.waitingDwell.Milliseconds()
		}
	}
	return st.statusDwell.Milliseconds()
}

// orderedRowNames returns the project row names in the exact order
// buildSnapshot lays them out: the deduped project-list names then the
// unmanaged session names, each block sorted. The single definition of that
// ordering, used by countVisibleProjects, projectNameAtRow, and cursorFlatRows
// so the cursor's row math can never disagree with what buildSnapshot
// published. Safe to call only from the hub goroutine (reads state maps).
func orderedRowNames(st *state) []string {
	seen := make(map[string]struct{}, len(st.projectListNames)+len(st.sessions))
	names := make([]string, 0, len(st.projectListNames))

	for _, n := range st.projectListNames {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		seen[proto.SessionKey(n)] = struct{}{}
		names = append(names, n)
	}

	var unmanagedNames []string
	for _, sess := range st.sessions {
		if skipForIndex(sess) {
			continue
		}
		if _, ok := seen[sess.Name]; ok {
			continue
		}
		seen[sess.Name] = struct{}{}
		if st.showUnmanaged {
			unmanagedNames = append(unmanagedNames, sess.Name)
		} else {
			names = append(names, sess.Name)
		}
	}
	sort.Strings(names)
	sort.Strings(unmanagedNames)

	return append(names, unmanagedNames...)
}

// cursorFlatRows builds the FLATTENED navigation row list for the cursor —
// the hub-side mirror of what the renderer flattens from the published
// snapshot — WITHOUT buildSnapshot's dwell/attention side effects. It
// assembles a minimal snapshot (ordered row names → Project skeletons + the
// live TeamGroups from teamGroupsFor) and runs it through the shared
// proto.FlatRows, gated on st.teamWindows exactly as the renderer gates on
// render.TeamRows. Because both sides flatten the same ordering through the
// same helper, cursorRow and the ▶ highlight index the same rows. Read-only;
// hub goroutine only.
func cursorFlatRows(st *state) []proto.FlatRow {
	names := orderedRowNames(st)
	projects := make([]proto.Project, len(names))
	for i, n := range names {
		projects[i] = proto.Project{Name: n}
	}
	snap := &proto.Snapshot{Projects: projects, TeamGroups: teamGroupsFor(st)}
	return proto.FlatRows(snap, st.teamWindows)
}

// projectNameAtRow returns the canonical name a select on the given flattened
// row jumps to (the project itself for a project row, the lead project for a
// member row), or "" when row is out of bounds. Retained as a thin wrapper
// over cursorFlatRows for the move-only cursor tests; the cursorRequests
// handler uses cursorFlatRows directly so it can also carry the member
// WindowID. Safe to call only from the hub goroutine.
func projectNameAtRow(st *state, row int) string {
	rows := cursorFlatRows(st)
	if row < 0 || row >= len(rows) {
		return ""
	}
	return rows[row].SwitchTo
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
	return sessionTitlesExcluding(st, s, nil)
}

// sessionTitlesExcluding is sessionTitles with an optional set of pane IDs to
// skip. buildSnapshot passes the Agent Teams member-pane set when teamWindows
// is on, so a relocated teammate's pane (which still lives in the lead's
// session) does not drive the LEAD's row attention — the lead row reflects
// the lead only, and teammate state surfaces on its own member row. A nil or
// empty exclude set reproduces sessionTitles exactly (the default path).
func sessionTitlesExcluding(st *state, s *session, exclude map[string]struct{}) []string {
	if s == nil || len(s.windows) == 0 {
		return nil
	}
	var titles []string
	for _, w := range s.windows {
		for paneID := range w.panesIDs {
			if _, skip := exclude[paneID]; skip {
				continue
			}
			if p, ok := st.panesByID[paneID]; ok {
				titles = append(titles, p.Title)
			}
		}
	}
	return titles
}

// teamMemberPaneIDs returns the set of tmux pane IDs claimed by Agent Teams
// members (the lead excluded — it IS the anchor; in-process members have no
// pane). Used by buildSnapshot under teamWindows to de-aggregate teammate
// panes out of the lead session's attention derivation. Returns nil when no
// team has a real-pane member, so the default (no-team) path allocates
// nothing. Read-only; hub goroutine only.
func teamMemberPaneIDs(st *state) map[string]struct{} {
	var out map[string]struct{}
	for _, t := range st.agentTeams {
		for _, m := range t.Members {
			if m.AgentType == "team-lead" {
				continue
			}
			if m.TmuxPaneID == "" || m.TmuxPaneID == teams.InProcessPaneID {
				continue
			}
			if out == nil {
				out = make(map[string]struct{})
			}
			out[m.TmuxPaneID] = struct{}{}
		}
	}
	return out
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
