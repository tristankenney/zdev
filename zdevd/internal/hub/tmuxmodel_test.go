package hub

import (
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestApplyEvent_WindowsListedPrunesAfterMissedClose pins the OQ-3 fix:
// windows are add-only, so a %window-close missed across a control-mode
// reconnect leaves a stale window forever. A list-windows -a poll that omits
// a previously-confirmed window must tear it down through detachWindow (so
// its panesByID entries die too — finding F2), while a window still in the
// list survives.
func TestApplyEvent_WindowsListedPrunesAfterMissedClose(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj-a"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@2"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@2", PaneID: "%2"}, nil)
	applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%2", Title: "● claude"}, nil)

	// First list confirms both windows (promotes them to authoritative).
	applyEvent(s, tmuxctl.WindowsListed{IDs: []string{"@1", "@2"}}, nil)
	sess, ok := sessionByName(s, "proj-a")
	if !ok || len(sess.windows) != 2 {
		t.Fatalf("after confirming list: want 2 windows, got %d", len(sess.windows))
	}

	// @2's close was missed; the next poll omits it → prune @2, keep @1.
	applyEvent(s, tmuxctl.WindowsListed{IDs: []string{"@1"}}, nil)
	sess, _ = sessionByName(s, "proj-a")
	if _, ok := sess.windows["@2"]; ok {
		t.Error("@2 survived the WindowsListed prune after a missed close")
	}
	if _, ok := sess.windows["@1"]; !ok {
		t.Error("@1 (still listed) was wrongly pruned")
	}
	if _, ok := s.panesByID["%2"]; ok {
		t.Error("pruned window's pane %2 left a panesByID residue — stale-title resurrection hazard")
	}
	if _, ok := s.panesByID["%1"]; !ok {
		t.Error("surviving window's pane %1 was wrongly removed")
	}
}

// TestApplyEvent_WindowsListedSparesProvisional pins the just-added-window
// race: a window created via %window-add but not yet seen in any poll is
// provisional and must survive a WindowsListed that raced its creation —
// otherwise it would be pruned and re-added on the next poll, the exact
// absent↔present flap the authority marker exists to prevent.
func TestApplyEvent_WindowsListedSparesProvisional(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj-a"}, nil)
	applyEvent(s, tmuxctl.WindowAdd{ID: "@1"}, nil) // provisional, never list-confirmed

	// A poll that doesn't include @1 (it raced @1's creation) must NOT prune
	// the unconfirmed window.
	applyEvent(s, tmuxctl.WindowsListed{IDs: []string{"@7"}}, nil)
	sess, _ := sessionByName(s, "proj-a")
	if _, ok := sess.windows["@1"]; !ok {
		t.Fatal("provisional window @1 was pruned before any poll confirmed it")
	}
}

// TestApplyEvent_WindowsListedSparesUnlinked pins the $_unlinked survival
// rule: list-windows -a legitimately omits unlinked windows, so their
// absence proves nothing — the window prune must never touch the parking lot.
func TestApplyEvent_WindowsListedSparesUnlinked(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj-a"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$1", WindowID: "@1"}, nil)
	applyEvent(s, tmuxctl.UnlinkedWindowAdd{ID: "@9"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@9", PaneID: "%9"}, nil)

	applyEvent(s, tmuxctl.WindowsListed{IDs: []string{"@1"}}, nil)

	unlinked, ok := s.sessions["$_unlinked"]
	if !ok {
		t.Fatal("$_unlinked bucket vanished")
	}
	if _, ok := unlinked.windows["@9"]; !ok {
		t.Error("$_unlinked window @9 was pruned by WindowsListed — it is legitimately absent from list-windows -a")
	}
}

// TestApplyEvent_WindowsListedSocketScoping pins the socket-scoping
// discipline (mirrors TestApplyEvent_SessionsListedPruneScoping): a default-
// socket list must not prune a foreign-socket window, nor a never-named
// record whose socket attribution is unknown; it does prune its own
// authoritative-but-absent window.
func TestApplyEvent_WindowsListedSocketScoping(t *testing.T) {
	s := newState()
	// Default-socket session with two confirmed windows.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$3", Name: "shared"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$3", WindowID: "@3"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$3", WindowID: "@3b"}, nil)
	// Foreign-socket session + window.
	applyEvent(s, tmuxctl.SessionChanged{ID: "$4", Name: "other", SocketName: "gt"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$4", WindowID: "@4"}, nil)
	// Never-named record (attachWindow-created, Name == ID): unknown socket.
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$9", WindowID: "@90"}, nil)

	// Default-socket list names only @3.
	applyEvent(s, tmuxctl.WindowsListed{SocketName: "", IDs: []string{"@3"}}, nil)

	shared, _ := sessionByName(s, "shared")
	if _, ok := shared.windows["@3"]; !ok {
		t.Error("listed default-socket window @3 was wrongly pruned")
	}
	if _, ok := shared.windows["@3b"]; ok {
		t.Error("default-socket ghost @3b survived its own socket's prune")
	}
	if s4 := s.sessions["$4"]; s4 == nil {
		t.Error("foreign-socket session $4 vanished")
	} else if _, ok := s4.windows["@4"]; !ok {
		t.Error("foreign-socket window @4 was pruned by the default socket's list")
	}
	if s9 := s.sessions["$9"]; s9 == nil {
		t.Error("never-named record $9 vanished")
	} else if _, ok := s9.windows["@90"]; !ok {
		t.Error("never-named record's window @90 was pruned (socket unknown)")
	}
}

// TestApplyEvent_WindowAttachAuthorityRefusal pins the authority invariant: a
// provisional signal (UnlinkedWindowAdd parking) must never displace a
// confirmed (WindowAttach/list-derived) association into the parking lot —
// the WindowsListed/PanesListed reconcile owns removal now.
func TestApplyEvent_WindowAttachAuthorityRefusal(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "owner"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$1", WindowID: "@1"}, nil) // authoritative
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)

	// A provisional unlinked-parking signal for the same window must be refused.
	applyEvent(s, tmuxctl.UnlinkedWindowAdd{ID: "@1"}, nil)

	owner, _ := sessionByName(s, "owner")
	if _, ok := owner.windows["@1"]; !ok {
		t.Fatal("provisional UnlinkedWindowAdd yanked an authoritative window into $_unlinked")
	}
	if unlinked, ok := s.sessions["$_unlinked"]; ok {
		if _, still := unlinked.windows["@1"]; still {
			t.Error("@1 duplicated into the $_unlinked bucket")
		}
	}
}

// TestApplyEvent_PanesListedPrunesAndRetiresUnlinked pins the pane reconcile:
// a pane absent from list-panes -a is detached (no panesByID residue), and an
// $_unlinked window emptied by that prune is retired — the only authority
// allowed to prove a parked window gone.
func TestApplyEvent_PanesListedPrunesAndRetiresUnlinked(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj-a"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$1", WindowID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%2"}, nil)
	// A parked unlinked window with one pane.
	applyEvent(s, tmuxctl.UnlinkedWindowAdd{ID: "@9"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@9", PaneID: "%9"}, nil)

	// list-panes -a names only %1: %2 (real session) and %9 (unlinked) are gone.
	applyEvent(s, tmuxctl.PanesListed{IDs: []string{"%1"}}, nil)

	if _, ok := s.panesByID["%2"]; ok {
		t.Error("%2 left a panesByID residue after PanesListed prune")
	}
	if _, ok := s.panesByID["%9"]; ok {
		t.Error("%9 (unlinked) left a panesByID residue")
	}
	if _, ok := s.panesByID["%1"]; !ok {
		t.Error("listed pane %1 was wrongly removed")
	}
	sess, _ := sessionByName(s, "proj-a")
	if _, ok := sess.windows["@1"].panesIDs["%2"]; ok {
		t.Error("%2 still referenced by @1 after detach")
	}
	if unlinked, ok := s.sessions["$_unlinked"]; ok {
		if _, still := unlinked.windows["@9"]; still {
			t.Error("$_unlinked window @9 was not retired after losing its last pane")
		}
	}
}

// TestApplyEvent_ListedEmptyGuards pins the empty-list guard for both
// reconcilers: a transiently empty poll must mutate nothing (cf. the
// SessionsListed non-empty guard).
func TestApplyEvent_ListedEmptyGuards(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "proj-a"}, nil)
	applyEvent(s, tmuxctl.WindowAttach{SessionID: "$1", WindowID: "@1"}, nil)
	applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
	applyEvent(s, tmuxctl.WindowsListed{IDs: []string{"@1"}}, nil) // confirm

	applyEvent(s, tmuxctl.WindowsListed{IDs: nil}, nil)
	applyEvent(s, tmuxctl.PanesListed{IDs: nil}, nil)

	sess, _ := sessionByName(s, "proj-a")
	if _, ok := sess.windows["@1"]; !ok {
		t.Error("empty WindowsListed pruned a confirmed window")
	}
	if _, ok := s.panesByID["%1"]; !ok {
		t.Error("empty PanesListed pruned a live pane")
	}
}
