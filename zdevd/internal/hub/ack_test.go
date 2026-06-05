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

	"github.com/tristankenney/zdev/zdevd/internal/proto"
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
