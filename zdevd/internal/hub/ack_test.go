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
	if g.Members[1].InProcess || g.Members[1].PaneID != "%42" {
		t.Errorf("tmux member = %+v; want pane %%42", g.Members[1])
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
