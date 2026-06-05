// internal/hub/death_test.go
//
// Agent-death lifecycle tests (roadmap NOW#3): the hook-confirmed
// WaitKindDead marker routes into its own DeadSinceTS lifecycle, masks
// the displayed attention, tops the triage queue, fires exactly one
// presence-bypassing notification, and round-trips through persistence.
package hub

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestApplyEvent_DeadKind: the dead marker stamps the death lifecycle
// and wipes any pending wait; any live notif clears the death.
func TestApplyEvent_DeadKind(t *testing.T) {
	s := newState()
	// Pending wait first — death must wipe it.
	applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 100, Kind: proto.WaitKindDecision, Summary: "which db?"}, nil)

	applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 200, Kind: proto.WaitKindDead, Summary: "exited: other"}, nil)
	pd := s.projectData["proj"]
	if pd.DeadSinceTS != 200 || pd.DeadReason != "exited: other" || pd.DeadNotified {
		t.Fatalf("death not recorded: %+v", pd)
	}
	if pd.WaitStartedTS != 0 || pd.WaitKind != "" || pd.WaitSummary != "" || pd.WaitNotifiedTiers != 0 {
		t.Errorf("death must wipe the pending wait: %+v", pd)
	}

	// Live evidence (a new wait fire) clears the death.
	applyEvent(s, tmuxctl.NotifSeen{Session: "proj", Timestamp: 300, Kind: "", Summary: "back online"}, nil)
	pd = s.projectData["proj"]
	if pd.DeadSinceTS != 0 || pd.DeadReason != "" {
		t.Errorf("live notif must clear death: %+v", pd)
	}
	if pd.WaitStartedTS != 300 {
		t.Errorf("WaitStartedTS = %d; want 300", pd.WaitStartedTS)
	}
}

// TestBuildSnapshot_DeadMasksAttention: an unresolved death overrides
// the displayed attention and carries death time/reason on the wait
// wire fields; a title-derived live agent clears it.
func TestBuildSnapshot_DeadMasksAttention(t *testing.T) {
	now := time.Now().Unix()

	t.Run("masks idle session and rides wire fields", func(t *testing.T) {
		// Shell-only titles → derived attention idle; death masks it.
		s := buildTestState("proj", []string{"%1"}, []string{"shell"})
		s.projectListNames = []string{"proj"}
		pd := s.projectData["proj"]
		pd.DeadSinceTS = now - 120
		pd.DeadReason = "exited: other"
		s.projectData["proj"] = pd

		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		p := findProject(snap.Projects, "proj")
		if p.Attention != proto.AttDead {
			t.Fatalf("Attention = %q; want dead", p.Attention)
		}
		if p.Status != "dead" {
			t.Errorf("Status = %q; want dead", p.Status)
		}
		if p.WaitStartedTS != now-120 {
			t.Errorf("WaitStartedTS = %d; want death time %d", p.WaitStartedTS, now-120)
		}
		if p.WaitSummary != "exited: other" {
			t.Errorf("WaitSummary = %q; want the exit reason", p.WaitSummary)
		}
	})

	t.Run("title newer than death clears it (restarted agent)", func(t *testing.T) {
		s := buildTestState("proj", []string{"%1"}, []string{"⠂ claude"}) // working
		s.projectListNames = []string{"proj"}
		pd := s.projectData["proj"]
		pd.DeadSinceTS = now - 120
		pd.DeadReason = "exited: other"
		pd.DeadNotified = true
		s.projectData["proj"] = pd
		s.lastTitleChangeTS["proj"] = now - 60 // title moved AFTER the death

		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		p := findProject(snap.Projects, "proj")
		if p.Attention != proto.AttWorking {
			t.Fatalf("Attention = %q; want working (death cleared by newer title)", p.Attention)
		}
		pd = s.projectData["proj"]
		if pd.DeadSinceTS != 0 || pd.DeadReason != "" || pd.DeadNotified {
			t.Errorf("death record must clear on live evidence: %+v", pd)
		}
	})

	t.Run("stale title from the dying pane does NOT clear (hook/pane-close race)", func(t *testing.T) {
		// The SessionEnd hook fires while the corpse's ✳ title still
		// exists; the pane dies milliseconds later. A snapshot pass in
		// that window must keep the death.
		s := buildTestState("proj", []string{"%1"}, []string{"✳ stale task"}) // waiting-shaped title
		s.projectListNames = []string{"proj"}
		pd := s.projectData["proj"]
		pd.DeadSinceTS = now - 1
		pd.DeadReason = "exited: other"
		s.projectData["proj"] = pd
		s.lastTitleChangeTS["proj"] = now - 300 // title last moved BEFORE the death

		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		p := findProject(snap.Projects, "proj")
		if p.Attention != proto.AttDead {
			t.Fatalf("Attention = %q; want dead (stale title must not read as life)", p.Attention)
		}
		if s.projectData["proj"].DeadSinceTS == 0 {
			t.Error("death record wrongly cleared by stale title")
		}
	})
}

// TestRankTriage_DeadTopsQueue: dead outranks even a permission wait.
func TestRankTriage_DeadTopsQueue(t *testing.T) {
	got := rankTriage([]proto.Project{
		{Name: "perm", Attention: proto.AttWaiting, WaitKind: proto.WaitKindPermission, WaitStartedTS: 1900},
		{Name: "corpse", Attention: proto.AttDead, WaitStartedTS: 1990, WaitSummary: "exited: other"},
		{Name: "done", Attention: proto.AttFinished, LastActivityTS: 100},
	}, 2000)
	want := []string{"corpse", "perm", "done"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("rankTriage = %v; want %v", got, want)
		}
	}
}

// TestTierCheck_Death covers the notification policy: fires once,
// bypasses presence, suppressed while attended, leads over tier
// crossings, and the once-bit means no re-fire.
func TestTierCheck_Death(t *testing.T) {
	t.Run("fires despite presence, exactly once, leads digest", func(t *testing.T) {
		s := newState()
		s.projectData["corpse"] = projectData{DeadSinceTS: 900, DeadReason: "exited: other"}
		s.projectData["waiter"] = projectData{WaitStartedTS: 940} // crosses 60s at now=1000
		s.clientSessions["c1"] = "somewhere-else"                 // present — must NOT defer death

		var got []Notification
		if !tierCheck(1000, s, func(n Notification) { got = append(got, n) }) {
			t.Fatal("expected fired=true")
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 digest, got %d: %v", len(got), got)
		}
		n := got[0]
		if n.Project != "corpse" || n.Kind != proto.WaitKindDead {
			t.Errorf("digest leader = %+v; want the death", n)
		}
		if n.Message != "agent died (exited: other) · 1 waiting" {
			t.Errorf("Message = %q", n.Message)
		}
		if !s.projectData["corpse"].DeadNotified {
			t.Error("DeadNotified bit not set")
		}

		// Second pass: death already notified → no re-fire. The waiter's
		// 60s tier was presence-deferred (bit unset), so still nothing.
		got = nil
		tierCheck(1001, s, func(n Notification) { got = append(got, n) })
		if len(got) != 0 {
			t.Errorf("death re-fired: %v", got)
		}
	})

	t.Run("attended session suppresses without marking", func(t *testing.T) {
		s := newState()
		s.projectData["corpse"] = projectData{DeadSinceTS: 900}
		s.clientSessions["c1"] = "corpse" // looking at it

		var got []Notification
		tierCheck(1000, s, func(n Notification) { got = append(got, n) })
		if len(got) != 0 {
			t.Fatalf("attended death must not banner: %v", got)
		}
		if s.projectData["corpse"].DeadNotified {
			t.Error("bit must stay unset so detaching without relaunch fires")
		}

		// Detach → fires.
		delete(s.clientSessions, "c1")
		tierCheck(1001, s, func(n Notification) { got = append(got, n) })
		if len(got) != 1 {
			t.Errorf("expected fire after detach, got %v", got)
		}
	})
}

// TestPersist_DeathRoundTrip: a 3am death survives a daemon restart
// with its once-bit intact.
func TestPersist_DeathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := newState()
	s.projectData["corpse"] = projectData{
		DeadSinceTS:  1714838460,
		DeadReason:   "exited: other",
		DeadNotified: true,
	}
	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	ps, err := loadState(path)
	if err != nil || ps == nil {
		t.Fatalf("loadState: %v %v", ps, err)
	}
	s2 := newState()
	applyPersistedState(s2, ps)
	pd := s2.projectData["corpse"]
	if pd.DeadSinceTS != 1714838460 || pd.DeadReason != "exited: other" || !pd.DeadNotified {
		t.Errorf("death round-trip lost data: %+v", pd)
	}
}
