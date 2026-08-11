package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// A wait cannot outlive its session. Found live 2026-08-09: two member
// clones killed mid-wait sat urgent-red for 45 hours — buildSnapshot's
// wait lifecycle only runs for present sessions, so the frozen stamp kept
// aging through isUrgent and the triage rank. Two layers fix it:
//
//  1. wire suppression — an absent row never emits wait fields (pd kept:
//     a hook's NotifSeen legitimately precedes WindowAdd, and that
//     transient wait must survive to display a pass later)
//  2. authoritative teardown — the SessionsListed reconcile clears the pd
//     cascade when tmux confirms the session is gone, guarded on no
//     same-name survivor

func TestAbsentRowSuppressesGhostWaitOnWire(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"ghost"}
	pd := s.projectData["ghost"]
	pd.WaitStartedTS = 1000
	pd.WaitKind = proto.WaitKindDecision
	pd.WaitSummary = "a question nobody can answer anymore"
	s.projectData["ghost"] = pd

	now := int64(1000 + 200000) // ~55h later, far past every urgent tier
	snap := buildSnapshot(s, 1, time.Unix(now, 0), now, now*1000)

	var row *proto.Project
	for i := range snap.Projects {
		if snap.Projects[i].Name == "ghost" {
			row = &snap.Projects[i]
		}
	}
	if row == nil {
		t.Fatal("ghost row missing from snapshot")
	}
	if row.Status != "absent" {
		t.Fatalf("ghost status = %q, want absent", row.Status)
	}
	if row.WaitStartedTS != 0 || row.WaitKind != "" || row.WaitSummary != "" {
		t.Errorf("absent row leaked ghost wait onto the wire: ts=%d kind=%q summary=%q",
			row.WaitStartedTS, row.WaitKind, row.WaitSummary)
	}
	// pd deliberately intact — wire-only suppression.
	if got := s.projectData["ghost"]; got.WaitStartedTS != 1000 {
		t.Errorf("wire suppression must not mutate projectData: %+v", got)
	}
}

func TestSessionsListedTeardownClearsWait(t *testing.T) {
	s := newState()
	// Session with a window so it is fully modeled, then a hook wait.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "doomed"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.NotifSeen{Session: "doomed", Timestamp: 5000, Kind: proto.WaitKindDecision, Summary: "q"}, nil)
	if s.projectData["doomed"].WaitStartedTS == 0 {
		t.Fatal("setup: wait stamp missing")
	}

	// Authoritative list that no longer contains the session.
	applyEvent(s, tmuxctl.SessionsListed{SocketName: ""}, nil)

	pd := s.projectData["doomed"]
	if pd.WaitStartedTS != 0 || pd.WaitKind != "" || pd.WaitSummary != "" || pd.HookWaitTS != 0 || pd.WaitNotifiedTiers != 0 {
		t.Errorf("teardown did not clear the wait cascade: %+v", pd)
	}
}

func TestSessionsListedTeardownSparesSameNameSurvivor(t *testing.T) {
	s := newState()
	// Two records sharing a name (collision), each modeled with a window.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "twin"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.SessionChanged{ID: "$2", Name: "twin"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@2"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@2", PaneID: "%2"}, nil)
	applyEvent(s, tmuxctl.NotifSeen{Session: "twin", Timestamp: 5000, Kind: proto.WaitKindDecision, Summary: "live q"}, nil)

	// Authoritative list keeps $2, drops $1 — the survivor owns the wait.
	applyEvent(s, tmuxctl.SessionsListed{SocketName: "", IDs: []string{"$2"}}, nil)

	if s.projectData["twin"].WaitStartedTS == 0 {
		t.Error("ghost teardown wiped the surviving twin's wait")
	}
}
