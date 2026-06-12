// internal/hub/ack_test.go
//
// Mark-all-read (`zdev ack`, roadmap NOW#7) and the restart pulse-wave
// regression it surfaced: ack is a notif-channel kind that clears
// hook-recorded waits/deaths AND stamps a synthetic visit so the
// title-derived wait machinery (latch, stale-✳ demoter, notification
// tier-ack) releases too; the wave fix makes initial title discovery
// (bootstrap scan after a daemon restart) not count as a title CHANGE,
// so persisted demoter stamps survive restarts.
package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/teams"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestApplyEvent_AckKind: ack clears a pending wait and a death record,
// and stamps lastVisitTS at the ack's timestamp (the synthetic visit).
func TestApplyEvent_AckKind(t *testing.T) {
	t.Run("clears wait and stamps visit", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 100, Kind: proto.WaitKindDecision, Summary: "which db?"}, nil)

		applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 200, Kind: proto.WaitKindAck}, nil)
		pd := s.projectData["proj"]
		if pd.WaitStartedTS != 0 || pd.WaitKind != "" || pd.WaitSummary != "" || pd.WaitNotifiedTiers != 0 {
			t.Errorf("ack must wipe the pending wait: %+v", pd)
		}
		if s.lastVisitTS["proj"] != 200 {
			t.Errorf("lastVisitTS = %d; want 200 (synthetic visit)", s.lastVisitTS["proj"])
		}
	})

	t.Run("clears death", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 100, Kind: proto.WaitKindDead, Summary: "exited: other"}, nil)

		applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 200, Kind: proto.WaitKindAck}, nil)
		pd := s.projectData["proj"]
		if pd.DeadSinceTS != 0 || pd.DeadReason != "" || pd.DeadNotified {
			t.Errorf("ack must clear death: %+v", pd)
		}
	})

	t.Run("never regresses a newer visit", func(t *testing.T) {
		s := newState()
		s.lastVisitTS["proj"] = 500 // user genuinely visited after the ack file's ts
		applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 200, Kind: proto.WaitKindAck}, nil)
		if s.lastVisitTS["proj"] != 500 {
			t.Errorf("lastVisitTS = %d; want 500 (older ack must not regress)", s.lastVisitTS["proj"])
		}
	})

	t.Run("releases a title-derived wait via the demoter", func(t *testing.T) {
		// The integration that makes ack actually clear a ✳ wait: the
		// synthetic visit post-dates the title change, so DeriveAttention's
		// stale-waiting demoter treats the title as seen.
		in := AttentionInputs{
			Titles:            []string{"✳ some task"},
			LastVisitTS:       200, // the ack stamp
			LastTitleChangeTS: 150, // ✳ appeared before the ack
			WaitStartedTS:     150,
			PrevAttention:     proto.AttWaiting,
		}
		got := DeriveAttention(in, 300)
		if got.Attention != proto.AttIdle {
			t.Errorf("Attention = %q; want idle (ack's visit demotes the stale ✳)", got.Attention)
		}
	})
}

// TestApplyEvent_TitleDiscoveryIsNotAChange pins the restart pulse-wave
// fix: the bootstrap scan's WindowPaneChanged + PaneTitleChanged pair
// (pane known, title empty) and the bare unknown-pane title must NOT
// stamp lastTitleChangeTS — only a real nonempty→different retitle does.
func TestApplyEvent_TitleDiscoveryIsNotAChange(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)

	// Bootstrap title population (pane known, empty title) — discovery.
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "✳ leftover task"}, nil)
	if ts := s.lastTitleChangeTS["proj"]; ts != 0 {
		t.Fatalf("bootstrap title population stamped lastTitleChangeTS=%d; want 0 — this re-enables the restart pulse wave", ts)
	}

	// Unknown pane entirely (no WindowPaneChanged first) — also discovery.
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%2"}, nil)
	delete(s.panesByID, "%2") // simulate title arriving before pane discovery
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%2", Title: "✳ another"}, nil)
	if ts := s.lastTitleChangeTS["proj"]; ts != 0 {
		t.Fatalf("unknown-pane title stamped lastTitleChangeTS=%d; want 0", ts)
	}

	// A real retitle (nonempty → different) MUST stamp.
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "✳ new actual wait"}, nil)
	if ts := s.lastTitleChangeTS["proj"]; ts == 0 {
		t.Fatal("real title change did not stamp lastTitleChangeTS — demoter would never re-arm")
	}

	// An identical re-send (poll echo) must NOT re-stamp.
	s.lastTitleChangeTS["proj"] = 42
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "✳ new actual wait"}, nil)
	if ts := s.lastTitleChangeTS["proj"]; ts != 42 {
		t.Errorf("identical title re-send re-stamped lastTitleChangeTS=%d; want 42", ts)
	}
}

// TestApplyEvent_WindowAttachMovesPanes pins the late-session association
// fix: a window discovered cross-session parks in "$_unlinked" with its
// panes; the poll's WindowAttach re-association must MOVE that window
// object — panes and all — into the real session. The old code created a
// second EMPTY window there, so sessionTitles read no titles and a
// session created after daemon start never derived attention (except
// when findWindow's random map order happened to route the pane into the
// right copy — a literal coin flip per run, caught by CI's agent-smoke).
func TestApplyEvent_WindowAttachMovesPanes(t *testing.T) {
	s := newState()
	// The exact arrival order from a CI/fresh-boot daemon: the window
	// shows up unlinked, its pane and waiting title arrive via the poll,
	// and only then does the re-association land.
	applyEvent(s, tmuxctl.UnlinkedWindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"}, nil)
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj-a"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$1", WindowID: "@1"}, nil)

	sess, ok := sessionByName(s, "proj-a")
	if !ok {
		t.Fatal("session proj-a not found")
	}
	w, ok := sess.windows["@1"]
	if !ok {
		t.Fatal("window @1 not attached to proj-a")
	}
	if _, ok := w.panesIDs["%1"]; !ok {
		t.Fatalf("pane %%1 missing from proj-a's window — attach duplicated instead of moving (panes: %v)", w.panesIDs)
	}
	if got := sessionTitles(s, sess); len(got) != 1 || got[0] != "● claude" {
		t.Fatalf("sessionTitles = %v; want the waiting title — attention can't derive without it", got)
	}
	// The unlinked bucket must no longer hold the window.
	if unlinked, ok := s.sessions["$_unlinked"]; ok {
		if _, still := unlinked.windows["@1"]; still {
			t.Error("window @1 still in $_unlinked after attach — duplicated, not moved")
		}
	}
}

// TestApplyEvent_TeamsChanged_SnapshotThreading (slice 3): the TeamsChanged
// map swap reaches the wire as sorted TeamGroups, the lead anchors to the
// session owning the pane whose cwd matches, in-process members carry no
// pane id, and an empty map clears everything.
func TestApplyEvent_TeamsChanged_SnapshotThreading(t *testing.T) {
	now := int64(1714838460)
	s := buildTestState("proj-a", []string{"%1"}, []string{"shell"})
	s.projectListNames = []string{"proj-a"}
	s.panesByID["%1"].Cwd = "/ws/proj-a"

	applyEvent(s, tmuxctl.TeamsChanged{Teams: map[string]*teams.Team{
		"alpha": {
			Name: "alpha",
			Members: []teams.Member{
				{Name: "team-lead", AgentType: "team-lead", CWD: "/ws/proj-a"},
				{Name: "worker-ip", AgentType: "general-purpose", Color: "blue", TmuxPaneID: teams.InProcessPaneID},
				{Name: "worker-tm", AgentType: "general-purpose", Color: "green", TmuxPaneID: "%42"},
			},
			// worker-ip declared idle in the lead's inbox (Tier 2a).
			MemberIdle: map[string]bool{"worker-ip": true},
		},
	}}, nil)

	snap := buildSnapshot(s, 1, time.Time{}, now, now*1000)
	if len(snap.TeamGroups) != 1 {
		t.Fatalf("TeamGroups = %+v; want 1 group", snap.TeamGroups)
	}
	g := snap.TeamGroups[0]
	if g.Name != "alpha" || g.LeadProject != "proj-a" {
		t.Fatalf("group = %+v; want alpha anchored to proj-a", g)
	}
	if len(g.Members) != 2 {
		t.Fatalf("Members = %+v; want 2 (lead excluded)", g.Members)
	}
	if !g.Members[0].InProcess || g.Members[0].PaneID != "" {
		t.Errorf("in-process member = %+v; want InProcess, no pane id", g.Members[0])
	}
	// v20: an in-process member with an idle_notification carries Status "idle".
	if g.Members[0].Status != "idle" {
		t.Errorf("in-process idle member Status = %q; want idle", g.Members[0].Status)
	}
	if g.Members[1].InProcess || g.Members[1].PaneID != "%42" {
		t.Errorf("tmux member = %+v; want pane %%42", g.Members[1])
	}
	// v20: the tmux member's pane shows a waiting title → Status "waiting"
	// and its WindowID resolves to the window owning the pane; the
	// in-process member (no pane) stays "" with no window.
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%42"}, nil)
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%42", Title: "● claude"}, nil)
	snap = buildSnapshot(s, 10, time.Time{}, now, now*1000)
	g = snap.TeamGroups[0]
	if g.Members[1].Status != "waiting" {
		t.Errorf("tmux member with waiting pane title = %+v; want Status waiting", g.Members[1])
	}
	if g.Members[1].WindowID != "@1" {
		t.Errorf("tmux member WindowID = %q; want @1 (window owning %%42)", g.Members[1].WindowID)
	}
	if g.Members[0].Status == "waiting" {
		t.Errorf("in-process member must never be waiting: %+v", g.Members[0])
	}
	if g.Members[0].WindowID != "" {
		t.Errorf("in-process member must have no WindowID: %+v", g.Members[0])
	}

	// Slash-form canonicalization (invariants review finding 2): a lead
	// in a managed project must anchor to the SLASH-form row name, not
	// the dash-form session name — the renderer compares against
	// Project.Name.
	s2 := buildTestState("zitcha-agora", []string{"%9"}, []string{"shell"})
	s2.projectListNames = []string{"zitcha/agora"}
	s2.panesByID["%9"].Cwd = "/ws/zitcha/agora"
	// A filtered infrastructure session sharing the cwd and sorting
	// FIRST must not steal the anchor (finding 1).
	applyEvent(s2, tmuxctl.SessionChanged{ID: "$9", Name: "zdevd-watcher"}, nil)
	applyEvent(s2, tmuxctl.WindowAdd{ID: "@9"}, nil)
	applyEvent(s2, tmuxctl.WindowPaneChanged{WindowID: "@9", PaneID: "%8"}, nil)
	s2.panesByID["%8"].Cwd = "/ws/zitcha/agora"
	applyEvent(s2, tmuxctl.TeamsChanged{Teams: map[string]*teams.Team{
		"beta": {Name: "beta", Members: []teams.Member{
			{Name: "team-lead", AgentType: "team-lead", CWD: "/ws/zitcha/agora"},
		}},
	}}, nil)
	snap2 := buildSnapshot(s2, 1, time.Time{}, now, now*1000)
	if len(snap2.TeamGroups) != 1 || snap2.TeamGroups[0].LeadProject != "zitcha/agora" {
		t.Fatalf("TeamGroups = %+v; want beta anchored to slash-form zitcha/agora", snap2.TeamGroups)
	}

	// Empty map clears (team dir removed).
	applyEvent(s, tmuxctl.TeamsChanged{Teams: nil}, nil)
	snap = buildSnapshot(s, 2, time.Time{}, now, now*1000)
	if len(snap.TeamGroups) != 0 {
		t.Fatalf("TeamGroups after clear = %+v; want empty", snap.TeamGroups)
	}
}

// TestBuildSnapshot_LeadDeAggregation (Agent Teams slice B, step 4): a team
// member's pane lives in the lead's session (team-sweep relocates it to its
// own WINDOW, not its own session). With teamWindows OFF the member's waiting
// title aggregates into the lead's project row exactly as before; with
// teamWindows ON the member pane is excluded from the lead session's attention
// derivation, so the lead row reflects the lead's own (alive) pane only.
func TestBuildSnapshot_LeadDeAggregation(t *testing.T) {
	now := int64(1714838460)

	// %1 is the lead's shell (alive); %42 is a team member's waiting pane,
	// both owned by session proj-a.
	newStateWithTeam := func() *state {
		s := buildTestState("proj-a", []string{"%1", "%42"}, []string{"shell", "● claude"})
		s.projectListNames = []string{"proj-a"}
		s.agentTeams = map[string]*teams.Team{
			"alpha": {Name: "alpha", Members: []teams.Member{
				{Name: "team-lead", AgentType: "team-lead"},
				{Name: "worker", AgentType: "general-purpose", Color: "green", TmuxPaneID: "%42"},
			}},
		}
		return s
	}

	projStatus := func(snap *proto.Snapshot) string {
		for _, p := range snap.Projects {
			if p.Name == "proj-a" {
				return p.Status
			}
		}
		return "<missing>"
	}

	cases := []struct {
		name        string
		teamWindows bool
		wantStatus  string
	}{
		// Off: the member's waiting title wins the session derivation.
		{"knob_off_aggregates", false, tmuxctl.StatusWaiting},
		// On: the member pane is excluded, leaving only the lead's shell.
		{"knob_on_de_aggregates", true, tmuxctl.StatusAlive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStateWithTeam()
			s.teamWindows = tc.teamWindows
			snap := buildSnapshot(s, 1, time.Time{}, now, now*1000)
			if got := projStatus(snap); got != tc.wantStatus {
				t.Errorf("proj-a status = %q; want %q (teamWindows=%v)", got, tc.wantStatus, tc.teamWindows)
			}
		})
	}
}

// TestCursorFlatRows_MemberJump (Agent Teams slice C): with teamWindows on the
// flattened cursor row list nests the member under its lead, the cursor wrap
// bounds against the flattened length so it can reach the member row, and a
// select on that row resolves to the lead project name + the member's window.
func TestCursorFlatRows_MemberJump(t *testing.T) {
	// proj-a owns the lead's shell (%1) and the member's pane (%42), both in
	// window @1; the lead anchors to proj-a by cwd.
	mk := func() *state {
		s := buildTestState("proj-a", []string{"%1", "%42"}, []string{"shell", "● claude"})
		s.projectListNames = []string{"proj-a"}
		s.panesByID["%1"].Cwd = "/ws/proj-a"
		s.agentTeams = map[string]*teams.Team{
			"alpha": {Name: "alpha", Members: []teams.Member{
				{Name: "team-lead", AgentType: "team-lead", CWD: "/ws/proj-a"},
				{Name: "worker", AgentType: "general-purpose", Color: "green", TmuxPaneID: "%42"},
			}},
		}
		return s
	}

	// Knob off: one flattened row (the project), member not navigable.
	off := mk()
	if got := countVisibleProjects(off); got != 1 {
		t.Errorf("teamWindows off: countVisibleProjects = %d; want 1", got)
	}

	// Knob on: two flattened rows — proj-a then proj-a/worker.
	on := mk()
	on.teamWindows = true
	rows := cursorFlatRows(on)
	if len(rows) != 2 {
		t.Fatalf("flattened rows = %d; want 2 (%+v)", len(rows), rows)
	}
	if rows[0].IsMember() || rows[0].SwitchTo != "proj-a" {
		t.Errorf("row 0 = %+v; want proj-a project row", rows[0])
	}
	if !rows[1].IsMember() || rows[1].SwitchTo != "proj-a" || rows[1].WindowID != "@1" {
		t.Errorf("row 1 = %+v; want member row switching to proj-a, window @1", rows[1])
	}
	if got := countVisibleProjects(on); got != 2 {
		t.Errorf("teamWindows on: countVisibleProjects = %d; want 2", got)
	}

	// The cursor reaches the member row: +1 activates row 0, +1 moves to row 1.
	applyEvent(on, tmuxctl.CursorMove{Delta: +1}, nil)
	applyEvent(on, tmuxctl.CursorMove{Delta: +1}, nil)
	if on.cursorRow != 1 {
		t.Fatalf("cursorRow = %d; want 1 (member row)", on.cursorRow)
	}
	if name := projectNameAtRow(on, on.cursorRow); name != "proj-a" {
		t.Errorf("projectNameAtRow(member row) = %q; want proj-a (lead session)", name)
	}
}

// TestApplyEvent_WindowAddDoesNotSteal pins the ping-pong fix (dogfood
// 2026-06-11: thousands of alive↔absent flips/hour): WindowAdd attaches
// via the racy currentSessionID pin, so it must NEVER move a window a
// real session already owns — only the explicit WindowAttach may.
func TestApplyEvent_WindowAddDoesNotSteal(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "owner"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)

	// Another session becomes "current"; a racy WindowAdd for @1 fires.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$2", Name: "thief"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)

	owner, _ := sessionByName(s, "owner")
	if _, ok := owner.windows["@1"]; !ok {
		t.Fatal("WindowAdd stole @1 from its owner — alive/absent ping-pong regression")
	}
	thief, _ := sessionByName(s, "thief")
	if thief != nil {
		if _, ok := thief.windows["@1"]; ok {
			t.Fatal("@1 duplicated into the thief session")
		}
	}

	// The explicit, authoritative path still moves.
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$2", WindowID: "@1"}, nil)
	thief, _ = sessionByName(s, "thief")
	if _, ok := thief.windows["@1"]; !ok {
		t.Fatal("explicit WindowAttach must still move the window")
	}
	// And the $_unlinked adoption path still works through WindowAdd.
	applyEvent(s, tmuxctl.UnlinkedWindowAdd{ID: "@9"}, nil)
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "owner"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@9"}, nil)
	owner, _ = sessionByName(s, "owner")
	if _, ok := owner.windows["@9"]; !ok {
		t.Fatal("WindowAdd must still adopt from $_unlinked")
	}
}

// TestApplyEvent_SessionsListedPrunesGhosts covers the ghost-session prune
// (dogfood 2026-06-12): killed sessions previously lingered in
// state.sessions forever because nothing ever removed them, and recreating
// a session with the same name yielded two records sharing one name.
func TestApplyEvent_SessionsListedPrunesGhosts(t *testing.T) {
	s := newState()
	// Ghost: an old zitcha-infra ($5) whose tmux session has been killed.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$5", Name: "zitcha-infra"}, nil)
	// Recreate: the live session ($7), plus an unrelated survivor ($6).
	applyEvent(s, tmuxctl.SessionChanged{ID: "$6", Name: "other"}, nil)
	applyEvent(s, tmuxctl.SessionChanged{ID: "$7", Name: "zitcha-infra"}, nil)
	// $_unlinked parking lot must never be pruned.
	applyEvent(s, tmuxctl.UnlinkedWindowAdd{ID: "@9"}, nil)

	applyEvent(s, tmuxctl.SessionsListed{IDs: []string{"$6", "$7"}}, nil)

	if _, ok := s.sessions["$5"]; ok {
		t.Fatal("ghost $5 survived the SessionsListed prune")
	}
	if _, ok := s.sessions["$6"]; !ok {
		t.Fatal("listed session $6 was wrongly pruned")
	}
	if _, ok := s.sessions["$7"]; !ok {
		t.Fatal("listed session $7 was wrongly pruned")
	}
	if _, ok := s.sessions["$_unlinked"]; !ok {
		t.Fatal("$_unlinked parking lot was pruned")
	}

	// Socket scoping: a list from another socket must not prune default-
	// socket sessions.
	applyEvent(s, tmuxctl.SessionsListed{SocketName: "elsewhere", IDs: []string{"$99"}}, nil)
	if _, ok := s.sessions["$7"]; !ok {
		t.Fatal("a foreign socket's SessionsListed pruned a default-socket session")
	}
}

// TestSnapshotStatuses_GhostCollisionDeterministic pins the deterministic
// same-name winner (dogfood 2026-06-12): with a windowless ghost and a
// live waiting session both named zitcha-infra, the derived status must be
// stable across repeated derivations — the random map-iteration winner
// flipped absent↔waiting thousands of times per hour in the eventlog and
// bounced the sidebar row.
func TestSnapshotStatuses_GhostCollisionDeterministic(t *testing.T) {
	s := newState()
	// Live session first or last must not matter; build ghost AFTER the
	// live one so insertion order can't mask a fix that relies on it.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$7", Name: "zitcha-infra"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "✳ Thinking…"}, nil)
	applyEvent(s, tmuxctl.SessionChanged{ID: "$5", Name: "zitcha-infra"}, nil)

	want := snapshotStatuses(s)["zitcha-infra"]
	if want == "absent" {
		t.Fatalf("collision winner derived %q — the windowless ghost won", want)
	}
	for i := 0; i < 100; i++ {
		got := snapshotStatuses(s)["zitcha-infra"]
		if got != want {
			t.Fatalf("iteration %d: status flapped %q -> %q (nondeterministic collision winner)", i, want, got)
		}
	}

	// buildSnapshot must agree with itself too (same collision, separate code path).
	now := int64(1700000000)
	first := buildSnapshot(s, 1, time.Unix(now, 0), now, now*1000)
	var firstStatus string
	for _, p := range first.Projects {
		if p.Name == "zitcha-infra" {
			firstStatus = p.Status
		}
	}
	if firstStatus == "" || firstStatus == "absent" {
		t.Fatalf("buildSnapshot derived %q for the collided name", firstStatus)
	}
	for i := 0; i < 100; i++ {
		snap := buildSnapshot(s, int64(i+2), time.Unix(now, 0), now, now*1000)
		for _, p := range snap.Projects {
			if p.Name == "zitcha-infra" && p.Status != firstStatus {
				t.Fatalf("iteration %d: buildSnapshot status flapped %q -> %q", i, firstStatus, p.Status)
			}
		}
	}
}

// TestApplyEvent_SessionsListedPruneScoping covers the invariants-review F1
// paths: (a) a never-named record (attachWindow-created, Name == ID) has
// unknown socket attribution and must survive any prune; (b) a live
// foreign-socket session sharing a NAME with a default-socket session must
// survive the default socket's SessionsListed (the name-keyed
// sessionSocket map is last-writer-wins and would have mispruned it).
// Also pins F2: pruning detaches windows through detachWindow so
// panesByID entries die with the record instead of resurrecting a stale
// title onto a recycled pane ID.
func TestApplyEvent_SessionsListedPruneScoping(t *testing.T) {
	s := newState()
	// (a) attachWindow-shaped record: window references session $9 before
	// its socket's list names it.
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$9", WindowID: "@90"}, nil)
	// (b) same name on two sockets; the foreign SessionChanged arrives
	// LAST so the name-keyed sessionSocket map points at "gt" — the exact
	// last-writer-wins ordering that broke the name-keyed scoping.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$3", Name: "shared"}, nil)
	applyEvent(s, tmuxctl.SessionChanged{ID: "$4", Name: "shared", SocketName: "gt"}, nil)
	// A default-socket ghost that SHOULD be pruned, with a titled pane
	// whose panesByID entry must die with it.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$5", Name: "ghosted"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@50"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@50", PaneID: "%50"}, nil)
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%50", Title: "✳ stale"}, nil)

	// Default-socket list: $3 lives, $5 gone, $9/$4 not its business.
	applyEvent(s, tmuxctl.SessionsListed{IDs: []string{"$3"}}, nil)

	if _, ok := s.sessions["$9"]; !ok {
		t.Fatal("never-named record $9 (unknown socket) was pruned")
	}
	if _, ok := s.sessions["$4"]; !ok {
		t.Fatal("live foreign-socket session $4 was pruned by the default socket's list")
	}
	if _, ok := s.sessions["$3"]; !ok {
		t.Fatal("listed default-socket session $3 was wrongly pruned")
	}
	if _, ok := s.sessions["$5"]; ok {
		t.Fatal("default-socket ghost $5 survived the prune")
	}
	if _, ok := s.panesByID["%50"]; ok {
		t.Fatal("pruned session's pane %50 left a panesByID residue — stale-title resurrection hazard")
	}

	// The foreign socket's own list prunes its own ghost.
	applyEvent(s, tmuxctl.SessionsListed{SocketName: "gt", IDs: []string{"$99"}}, nil)
	if _, ok := s.sessions["$4"]; ok {
		t.Fatal("foreign-socket list failed to prune its own vanished session $4")
	}
	if _, ok := s.sessions["$3"]; !ok {
		t.Fatal("foreign-socket list pruned a default-socket session")
	}
}
