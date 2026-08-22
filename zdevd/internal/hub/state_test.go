package hub

import (
	"errors"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestRecomputeAgents_UsesConfiguredMarkerStatus(t *testing.T) {
	s := buildTestState("custom-session", []string{"%1"}, []string{"WAIT custom"})
	s.agents = agents.NewRegistry([]agents.Spec{{
		Name:           "custom",
		WaitingMarkers: []string{"WAIT custom"},
	}})
	recomputeAgents(s, "custom-session")
	if got := s.projectData["custom-session"].AgentStates["custom"]; got != "waiting" {
		t.Fatalf("custom AgentStates status = %q; want waiting", got)
	}
}

// TestApplyPaneCwdChanged verifies that PaneCwdChanged records the cwd on the
// pane struct — both when the pane was previously known (from
// WindowPaneChanged / PaneTitleChanged) and when the cwd arrives first
// (the bootstrap-path race in which a poll observes a freshly-spawned pane
// before any other event registers it). zd-bub.
func TestApplyPaneCwdChanged(t *testing.T) {
	t.Run("known pane updates cwd in place", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"}, nil)
		applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"}, nil)
		applyEvent(s, tmuxctl.PaneCwdChanged{SessionName: "alpha", PaneID: "%1", Cwd: "/home/me/repo"}, nil)
		p := s.panesByID["%1"]
		if p == nil {
			t.Fatal("pane %1 missing after PaneCwdChanged")
		}
		if p.Cwd != "/home/me/repo" {
			t.Errorf("Cwd = %q; want /home/me/repo", p.Cwd)
		}
		if p.Title != "shell" {
			t.Errorf("Title clobbered by PaneCwdChanged: got %q", p.Title)
		}
	})

	t.Run("unknown pane is registered with cwd only", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.PaneCwdChanged{SessionName: "alpha", PaneID: "%9", Cwd: "/srv/app"}, nil)
		p := s.panesByID["%9"]
		if p == nil {
			t.Fatal("PaneCwdChanged did not register an unknown pane")
		}
		if p.Cwd != "/srv/app" {
			t.Errorf("Cwd = %q; want /srv/app", p.Cwd)
		}
		if p.Title != "" {
			t.Errorf("Title = %q; want \"\" on a cwd-only registration", p.Title)
		}
	})
}

// TestApplyDataRefresh verifies that DataRefresh populates branch/dirty/shell
// fields on the matching project in the snapshot.
func TestApplyDataRefresh(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%data-refresh")
	defer unsub()

	// Create a session named "alpha" so buildSnapshot includes it.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.DataRefresh{
		Project:    "alpha",
		Branch:     "feature-x",
		Ahead:      2,
		Behind:     1,
		DirtyCount: 3,
		ShellCmd:   "npm test",
	})

	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.Branch == "feature-x"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.Branch != "feature-x" {
		t.Errorf("Branch = %q; want %q", proj.Branch, "feature-x")
	}
	if proj.Ahead != 2 {
		t.Errorf("Ahead = %d; want 2", proj.Ahead)
	}
	if proj.Behind != 1 {
		t.Errorf("Behind = %d; want 1", proj.Behind)
	}
	if proj.DirtyCount != 3 {
		t.Errorf("DirtyCount = %d; want 3", proj.DirtyCount)
	}
	if proj.ShellCmd != "npm test" {
		t.Errorf("ShellCmd = %q; want %q", proj.ShellCmd, "npm test")
	}
}

// TestApplyIntentRefresh verifies that IntentRefresh (phase4-v23) populates
// Intent/BdReady on the matching project in the snapshot, mirroring
// TestApplyDataRefresh's event→snapshot pattern.
func TestApplyIntentRefresh(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%intent-refresh")
	defer unsub()

	// Create a session named "marketplace" so buildSnapshot includes it.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "marketplace"})
	mustSubmit(t, h, tmuxctl.IntentRefresh{
		Project: "marketplace",
		Intent:  "ship the marketplace MVP.",
		BdReady: 4,
	})

	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "marketplace")
		return p != nil && p.Intent == "ship the marketplace MVP."
	})
	proj := findProject(snap.Projects, "marketplace")
	if proj == nil {
		t.Fatal("project 'marketplace' not found in snapshot")
	}
	if proj.Intent != "ship the marketplace MVP." {
		t.Errorf("Intent = %q; want %q", proj.Intent, "ship the marketplace MVP.")
	}
	if proj.BdReady != 4 {
		t.Errorf("BdReady = %d; want 4", proj.BdReady)
	}
}

// TestApplyPRRefresh_NoCelebration verifies no celebration when Open count
// increases (no drop detected).
func TestApplyPRRefresh_NoCelebration(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%pr-no-cel")
	defer unsub()

	// Establish session.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	// First PRRefresh: all zeros.
	mustSubmit(t, h, tmuxctl.PRRefresh{Project: "alpha", Open: 0, Fail: 0, Pend: 0})

	// Wait for the first snapshot to arrive.
	_ = drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "alpha") != nil
	})

	// Second PRRefresh: Open went UP (no drop).
	mustSubmit(t, h, tmuxctl.PRRefresh{Project: "alpha", Open: 2, Fail: 0, Pend: 1})
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.PROpen == 2
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.CelebrateUntil != 0 {
		t.Errorf("CelebrateUntil = %d; want 0 (no drop, no celebration)", proj.CelebrateUntil)
	}
	if proj.PROpen != 2 {
		t.Errorf("PROpen = %d; want 2", proj.PROpen)
	}
}

// TestApplyPRRefresh_CelebrationOnDrop verifies PR celebration is set when
// Open count drops.
func TestApplyPRRefresh_CelebrationOnDrop(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%pr-cel")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	// First: Open=3.
	mustSubmit(t, h, tmuxctl.PRRefresh{Project: "alpha", Open: 3})
	_ = drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.PROpen == 3
	})

	// Second: Open=1 (dropped from 3 → 1).
	mustSubmit(t, h, tmuxctl.PRRefresh{Project: "alpha", Open: 1})
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.PROpen == 1
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	now := time.Now().Unix()
	if proj.CelebrateUntil <= now {
		t.Errorf("CelebrateUntil = %d; want > now (%d) — celebration not set on Open drop", proj.CelebrateUntil, now)
	}
}

// TestApplyPortsRefresh verifies that ports are reflected in the snapshot.
func TestApplyPortsRefresh(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%ports")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.PortsRefresh{Project: "alpha", Ports: []int{3000, 8080}})
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && len(p.ListeningPorts) == 2
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if len(proj.ListeningPorts) != 2 || proj.ListeningPorts[0] != 3000 || proj.ListeningPorts[1] != 8080 {
		t.Errorf("ListeningPorts = %v; want [3000 8080]", proj.ListeningPorts)
	}
}

// TestApplyNotifSeen_WorkingDone verifies the hook-driven working lifecycle:
// a `working` NotifSeen stamps HookWorkTS and clears any pending wait; a real
// wait or a `done` turn-end zeroes HookWorkTS again.
func TestApplyNotifSeen_WorkingDone(t *testing.T) {
	t.Run("working stamps HookWorkTS and clears a pending wait", func(t *testing.T) {
		s := newState()
		// A wait is pending from a prior turn.
		applyEvent(s, tmuxctl.NotifSeen{Session: "alpha", Timestamp: 100, Kind: proto.WaitKindPermission}, nil)
		if s.projectData["alpha"].WaitStartedTS == 0 {
			t.Fatal("precondition: wait not recorded")
		}
		// New turn begins.
		applyEvent(s, tmuxctl.NotifSeen{Session: "alpha", Timestamp: 200, Kind: proto.WaitKindWorking}, nil)
		pd := s.projectData["alpha"]
		if pd.HookWorkTS != 200 {
			t.Errorf("HookWorkTS = %d; want 200", pd.HookWorkTS)
		}
		if pd.WaitStartedTS != 0 || pd.HookWaitTS != 0 || pd.WaitKind != "" {
			t.Errorf("working did not clear the pending wait: %+v", pd)
		}
	})

	t.Run("done zeroes HookWorkTS", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.NotifSeen{Session: "alpha", Timestamp: 200, Kind: proto.WaitKindWorking}, nil)
		applyEvent(s, tmuxctl.NotifSeen{Session: "alpha", Timestamp: 300, Kind: proto.WaitKindDone}, nil)
		if got := s.projectData["alpha"].HookWorkTS; got != 0 {
			t.Errorf("HookWorkTS after done = %d; want 0", got)
		}
	})

	t.Run("a real wait supersedes working", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.NotifSeen{Session: "alpha", Timestamp: 200, Kind: proto.WaitKindWorking}, nil)
		applyEvent(s, tmuxctl.NotifSeen{Session: "alpha", Timestamp: 300, Kind: proto.WaitKindPermission}, nil)
		pd := s.projectData["alpha"]
		if pd.HookWorkTS != 0 {
			t.Errorf("HookWorkTS after a wait = %d; want 0", pd.HookWorkTS)
		}
		if pd.WaitStartedTS != 300 {
			t.Errorf("WaitStartedTS = %d; want 300", pd.WaitStartedTS)
		}
	})
}

// TestApplyNotifSeen verifies WaitStartedTS is set from NotifSeen.
func TestApplyNotifSeen(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%notif")
	defer unsub()

	// Window + pane so the session derives PRESENT: hooks fire from inside
	// panes, and an absent row suppresses wait fields on the wire by
	// design (ghost-wait fix, 2026-08-09 — see absent_wait_test.go).
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	// zdev-notify writes the notif file AND sets the ● pane title in the
	// same breath — a hook wait without its waiting title is not a state
	// that occurs in production, and the lifecycle rightly clears it.
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})
	// Recent timestamp: a present session's wait lifecycle rejects
	// ancient stamps by design (stale-replay protection) — the old
	// fixed 2024 constant only ever survived because the windowless
	// session skipped the lifecycle entirely.
	ts := time.Now().Unix()
	mustSubmit(t, h, tmuxctl.NotifSeen{Session: "alpha", Timestamp: ts})
	// Near-equality, not exact: the title pass may open the wait a moment
	// before the hook stamp lands, and a continuing wait correctly keeps
	// its original start time. The assertion's intent is "the wait
	// reached the wire", not "the hook owns the clock".
	near := func(got int64) bool { return got != 0 && got >= ts-5 && got <= ts+5 }
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && near(p.WaitStartedTS)
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if !near(proj.WaitStartedTS) {
		t.Errorf("WaitStartedTS = %d; want ~%d", proj.WaitStartedTS, ts)
	}
}

// TestApplyNotifSeen_Kind verifies the wait cost-class flows from
// NotifSeen.Kind onto the wire (Project.WaitKind), and that the
// buildSnapshot wait-clear cascade wipes it alongside WaitContext /
// WaitNotifiedTiers when the wait lifecycle ends.
func TestApplyNotifSeen_Kind(t *testing.T) {
	t.Run("kind_reaches_wire", func(t *testing.T) {
		h, cleanup := startHub(t)
		defer cleanup()
		sub, unsub := mustSubscribe(t, h, "%notifkind")
		defer unsub()

		// Present session (window + pane) — same reasoning as
		// TestApplyNotifSeen: absent rows suppress wait wire fields.
		mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
		mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
		mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
		mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})
		mustSubmit(t, h, tmuxctl.NotifSeen{Session: "alpha", Timestamp: time.Now().Unix(), Kind: proto.WaitKindPermission})
		snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
			p := findProject(s.Projects, "alpha")
			return p != nil && p.WaitKind == proto.WaitKindPermission
		})
		if p := findProject(snap.Projects, "alpha"); p.WaitKind != proto.WaitKindPermission {
			t.Errorf("WaitKind = %q; want %q", p.WaitKind, proto.WaitKindPermission)
		}
	})

	t.Run("cleared_on_wait_exit", func(t *testing.T) {
		// Direct-state variant (no socket round-trip): enter waiting with a
		// tagged kind, visit so the latch releases, leave waiting, and
		// verify the cascade wiped the kind.
		s := buildTestState("proj", []string{"%1"}, []string{"● claude"})
		s.projectListNames = []string{"proj"}
		now := time.Now().Unix()

		pd := s.projectData["proj"]
		pd.WaitKind = proto.WaitKindDecision
		s.projectData["proj"] = pd

		_ = buildSnapshot(s, 1, time.Now(), now, now*1000)
		if got := s.projectData["proj"].WaitKind; got != proto.WaitKindDecision {
			t.Fatalf("pre-condition: WaitKind = %q; want decision while waiting", got)
		}

		// Visit (releases the no-visit latch), then the title leaves waiting.
		s.lastVisitTS["proj"] = s.projectData["proj"].WaitStartedTS + 1
		s.lastTitleChangeTS["proj"] = s.lastVisitTS["proj"] - 1
		s.panesByID["%1"].Title = "shell"
		_ = buildSnapshot(s, 2, time.Now(), now+2, (now+2)*1000)

		pd = s.projectData["proj"]
		if pd.WaitStartedTS != 0 {
			t.Fatalf("WaitStartedTS = %d after wait exit; want 0", pd.WaitStartedTS)
		}
		if pd.WaitKind != "" {
			t.Errorf("WaitKind = %q after wait exit; want cleared", pd.WaitKind)
		}
	})
}

// TestApplyProjectListChanged verifies that projects without tmux sessions
// still appear in the snapshot after ProjectListChanged.
func TestApplyProjectListChanged(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%projlist")
	defer unsub()

	// Pre-state: two tmux sessions.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "beta"})
	_ = drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "alpha") != nil && findProject(s.Projects, "beta") != nil
	})

	// ProjectListChanged adds "gamma" (no tmux session).
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"alpha", "beta", "gamma"}})
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "gamma") != nil
	})
	if findProject(snap.Projects, "alpha") == nil {
		t.Error("project 'alpha' missing from snapshot")
	}
	if findProject(snap.Projects, "beta") == nil {
		t.Error("project 'beta' missing from snapshot")
	}
	if findProject(snap.Projects, "gamma") == nil {
		t.Error("project 'gamma' missing from snapshot — ProjectListChanged not applied to buildSnapshot")
	}
}

// TestPRCelebrationOutlivesDebounce verifies that even when the hub coalesces
// multiple PRRefresh events in one debounce window (5→4→3→2→1), the single
// emitted snapshot still has CelebrateUntil set (Pitfall G — celebration is
// in state, not snapshot, so debounce can't lose it).
func TestPRCelebrationOutlivesDebounce(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%pr-cel-debounce")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	// First: establish Open=5 count in separate debounce window.
	mustSubmit(t, h, tmuxctl.PRRefresh{Project: "alpha", Open: 5})
	_ = drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.PROpen == 5
	})

	// Second burst: rapid-fire 5→4→3→2→1 within a single debounce window.
	for _, open := range []int{5, 4, 3, 2, 1} {
		mustSubmit(t, h, tmuxctl.PRRefresh{Project: "alpha", Open: open})
		time.Sleep(1 * time.Millisecond)
	}
	// Wait past debounce window — coalesced into one snapshot.
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.PROpen == 1
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	now := time.Now().Unix()
	if proj.CelebrateUntil <= now {
		t.Errorf("CelebrateUntil = %d; want > now (%d) — celebration lost during debounce coalescing (Pitfall G)", proj.CelebrateUntil, now)
	}
}

// --- Task 6.3: ShellCmd tests (DATA-03, PaneCommandChanged) ---

// TestApplyShellCmd_Populates verifies that PaneCommandChanged populates ShellCmd
// when the pane title is "shell" and the cmd is not a default shell.
func TestApplyShellCmd_Populates(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%shellcmd-pop")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})
	mustSubmit(t, h, tmuxctl.PaneCommandChanged{PaneID: "%1", Cmd: "npm test"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.ShellCmd == "npm test"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.ShellCmd != "npm test" {
		t.Errorf("ShellCmd = %q; want %q", proj.ShellCmd, "npm test")
	}
}

// TestApplyShellCmd_SuppressedForDefaultShell verifies that ShellCmd is NOT
// set when the pane title is "shell" but cmd is a default shell (e.g., "bash").
func TestApplyShellCmd_SuppressedForDefaultShell(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%shellcmd-suppress")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})
	mustSubmit(t, h, tmuxctl.PaneCommandChanged{PaneID: "%1", Cmd: "bash"})

	// Wait for snapshot with the session present.
	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "alpha") != nil
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.ShellCmd != "" {
		t.Errorf("ShellCmd = %q; want %q (default shell suppressed)", proj.ShellCmd, "")
	}
}

// TestApplyShellCmd_SuppressedForNonShellTitle verifies that ShellCmd is NOT
// set when the pane title is NOT "shell" (e.g., "● claude").
func TestApplyShellCmd_SuppressedForNonShellTitle(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%shellcmd-title")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})
	mustSubmit(t, h, tmuxctl.PaneCommandChanged{PaneID: "%1", Cmd: "claude"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "alpha") != nil
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.ShellCmd != "" {
		t.Errorf("ShellCmd = %q; want %q (non-shell title suppressed)", proj.ShellCmd, "")
	}
}

// TestApplyShellCmd_ClearsWhenCmdReturnsToShell verifies that after ShellCmd="npm test",
// a PaneCommandChanged with cmd="zsh" clears ShellCmd to "".
func TestApplyShellCmd_ClearsWhenCmdReturnsToShell(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%shellcmd-clear")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})
	mustSubmit(t, h, tmuxctl.PaneCommandChanged{PaneID: "%1", Cmd: "npm test"})

	// Wait for ShellCmd to be set.
	_ = drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.ShellCmd == "npm test"
	})

	// Return to shell.
	mustSubmit(t, h, tmuxctl.PaneCommandChanged{PaneID: "%1", Cmd: "zsh"})
	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.ShellCmd == ""
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.ShellCmd != "" {
		t.Errorf("ShellCmd = %q; want %q (cleared when cmd returns to shell)", proj.ShellCmd, "")
	}
}

// --- Task 6.4: LastActivityTS tests (DATA-07, VIS-12, ActivityRefresh) ---

// TestApplyActivityRefresh_PopulatesLastActivityTS verifies that ActivityRefresh
// sets LastActivityTS on the matching project.
func TestApplyActivityRefresh_PopulatesLastActivityTS(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%activity")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	const ts = int64(1714838400)
	// Submit with the tmux session ID ($1), not the name — that's what the
	// supervisor's parseSubscriptionChanged emits in production. The hub
	// resolves ID→name via s.sessions before writing projectData.
	mustSubmit(t, h, tmuxctl.ActivityRefresh{Session: "$1", ActivityTS: ts})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.LastActivityTS == ts
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.LastActivityTS != ts {
		t.Errorf("LastActivityTS = %d; want %d", proj.LastActivityTS, ts)
	}
}

// TestApplyActivityRefresh_MonotonicUpdate verifies that LastActivityTS never
// regresses — a lower timestamp arriving after a higher one is ignored.
func TestApplyActivityRefresh_MonotonicUpdate(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%activity-mono")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	const highTS = int64(1714838400)
	const lowTS = int64(1714838000)

	mustSubmit(t, h, tmuxctl.ActivityRefresh{Session: "$1", ActivityTS: highTS})
	_ = drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.LastActivityTS == highTS
	})

	// Lower timestamp should NOT regress LastActivityTS. The applyEvent
	// path correctly drops the lower timestamp, leaving state unchanged —
	// which means the publish-path short-circuit (snapshot-equality) will
	// (correctly) NOT republish, since nothing observable changed. To
	// observe state through a subscriber we trigger an unrelated mutation
	// (a second SessionChanged) and verify the alpha row in the resulting
	// snapshot still has highTS.
	mustSubmit(t, h, tmuxctl.ActivityRefresh{Session: "$1", ActivityTS: lowTS})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "beta"})
	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "beta") != nil
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.LastActivityTS != highTS {
		t.Errorf("LastActivityTS = %d; want %d (monotonic: lower timestamp must not regress)", proj.LastActivityTS, highTS)
	}
}

// TestApplyActivityRefresh_QueuedBeforeSessionKnown locks 260511-d3p: an
// ActivityRefresh whose session ID is not yet registered when the event
// lands is queued in state.pendingActivityTS and applied as soon as a
// SessionChanged for that ID assigns a name. Without the queue, the first
// ActivityRefresh after a session is discovered would silently drop and
// the age chip would wait an extra ~1s for the next poll cycle.
func TestApplyActivityRefresh_QueuedBeforeSessionKnown(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%activity-queued")
	defer unsub()

	const ts = int64(1714838400)
	// ActivityRefresh arrives FIRST, before any SessionChanged.
	mustSubmit(t, h, tmuxctl.ActivityRefresh{Session: "$7", ActivityTS: ts})

	// Now the SessionChanged arrives — the queued ts must drain onto projectData.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$7", Name: "late-named"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "late-named")
		return p != nil && p.LastActivityTS == ts
	})
	proj := findProject(snap.Projects, "late-named")
	if proj == nil {
		t.Fatal("project 'late-named' not in snapshot")
	}
	if proj.LastActivityTS != ts {
		t.Errorf("LastActivityTS = %d; want %d — queued ActivityRefresh did not drain on SessionChanged (260511-d3p)",
			proj.LastActivityTS, ts)
	}
}

// --- Task 6.2: Agent attribution tests (DATA-08, recomputeAgents) ---

// TestApplyAgent_PaneTitleChanged_ClaudeWaiting verifies AgentStates["claude"]="waiting"
// when a pane title is set to "● claude".
func TestApplyAgent_PaneTitleChanged_ClaudeWaiting(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-claude-waiting")
	defer unsub()

	// Bootstrap: session $1=alpha, window @1, pane %1.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["claude"] == "waiting"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "waiting" {
		t.Errorf("AgentStates[claude] = %q; want %q", proj.AgentStates["claude"], "waiting")
	}
	if proj.AgentStates["pi"] != "" {
		t.Errorf("AgentStates[pi] = %q; want %q", proj.AgentStates["pi"], "")
	}
}

// TestApplyAgent_PaneTitleChanged_ClaudeIdle verifies that a pane titled
// "✳ Claude Code" (Claude Code v2.1+ idle prompt, no active task) does NOT
// set AgentStates["claude"]="waiting". Without this guard, every Claude session pulses
// "needs input" on daemon restart simply because every idle Claude pane has
// that title.
func TestApplyAgent_PaneTitleChanged_ClaudeIdle(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-claude-idle")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "✳ Claude Code"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "alpha") != nil
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "" {
		t.Errorf("AgentStates[claude] = %q; want \"\" (idle prompt should not pulse)", proj.AgentStates["claude"])
	}
	if proj.AgentStates["pi"] != "" {
		t.Errorf("AgentStates[pi] = %q; want \"\"", proj.AgentStates["pi"])
	}
}

// TestApplyAgent_PaneTitleChanged_ClaudeFinished verifies AgentStates["claude"]="finished"
// when a pane title is "◆ claude --help".
func TestApplyAgent_PaneTitleChanged_ClaudeFinished(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-claude-done")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "◆ claude --help"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["claude"] == "finished"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "finished" {
		t.Errorf("AgentStates[claude] = %q; want %q", proj.AgentStates["claude"], "finished")
	}
	if proj.AgentStates["pi"] != "" {
		t.Errorf("AgentStates[pi] = %q; want %q", proj.AgentStates["pi"], "")
	}
}

// TestApplyAgent_PaneTitleChanged_Pi verifies AgentStates["pi"]="waiting" when a
// pane title is "● pi bench".
func TestApplyAgent_PaneTitleChanged_Pi(t *testing.T) {
	t.Skip("260519-hww: pi.dev integration temporarily disabled")
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-pi")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● pi bench"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["pi"] == "waiting"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "" {
		t.Errorf("AgentStates[claude] = %q; want %q", proj.AgentStates["claude"], "")
	}
	if proj.AgentStates["pi"] != "waiting" {
		t.Errorf("AgentStates[pi] = %q; want %q", proj.AgentStates["pi"], "waiting")
	}
}

// TestApplyAgent_PaneTitleChanged_NoAgent verifies both Agent fields are ""
// when the pane title is "shell" (no agent marker).
func TestApplyAgent_PaneTitleChanged_NoAgent(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-none")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "alpha") != nil
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "" {
		t.Errorf("AgentStates[claude] = %q; want %q (no-agent title)", proj.AgentStates["claude"], "")
	}
	if proj.AgentStates["pi"] != "" {
		t.Errorf("AgentStates[pi] = %q; want %q (no-agent title)", proj.AgentStates["pi"], "")
	}
}

// TestApplyAgent_MultipleAgentsInSession verifies that a session with both a
// claude and a pi pane returns both claude and pi entries populated.
func TestApplyAgent_MultipleAgentsInSession(t *testing.T) {
	t.Skip("260519-hww: pi.dev integration temporarily disabled")
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-multi")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%2"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%2", Title: "◆ pi"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["claude"] != "" && p.AgentStates["pi"] != ""
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "waiting" {
		t.Errorf("AgentStates[claude] = %q; want %q", proj.AgentStates["claude"], "waiting")
	}
	if proj.AgentStates["pi"] != "finished" {
		t.Errorf("AgentStates[pi] = %q; want %q", proj.AgentStates["pi"], "finished")
	}
}

// TestApplyAgent_AgentClearsOnTitleChange verifies that after the claude entry was
// "waiting", changing the pane title to "shell" clears it to "".
func TestApplyAgent_AgentClearsOnTitleChange(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-clear")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})

	// Wait for agent to be "waiting".
	_ = drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["claude"] == "waiting"
	})

	// Now change the title to "shell" — agent should clear.
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})
	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["claude"] == ""
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentStates["claude"] != "" {
		t.Errorf("AgentStates[claude] = %q; want %q (cleared after title change)", proj.AgentStates["claude"], "")
	}
}

// TestApplyAgent_PreservesPhase2Status verifies that the phase 3 chip extension
// does NOT regress VIS-01: after submitting "● claude", Project.Status must
// still be "waiting" via the unchanged deriveStatus path.
func TestApplyAgent_PreservesPhase2Status(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%agent-status")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentStates["claude"] == "waiting"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	// Phase 2 VIS-01: Status must remain "waiting" from ClassifyPaneTitle path.
	if proj.Status != "waiting" {
		t.Errorf("Status = %q; want %q (Phase 2 VIS-01 regression — deriveStatus path broken)", proj.Status, "waiting")
	}
	// Phase 3 DATA-08: the claude state set via the registry.
	if proj.AgentStates["claude"] != "waiting" {
		t.Errorf("AgentStates[claude] = %q; want %q", proj.AgentStates["claude"], "waiting")
	}
}

// ===== phase4-v2 (260508-vm2): WaitContext capture tests =====
//
// These tests operate directly on *state + recomputeAgents to get precise
// control over paneCapturer call counts without the hub goroutine overhead.
// No subprocesses are spawned; paneCapturer is always overridden with a stub.

// buildTestState constructs a *state with a session named sessionName that
// owns one window (@1) with panes whose titles come from paneTitles.
// paneCapturer is set to a stub that returns ("", nil) — override after calling.
func buildTestState(sessionName string, paneIDs []string, paneTitles []string) *state {
	s := newState()
	s.paneCapturer = func(paneID, socketName string) (string, error) { return "", nil }

	s.sessions["$1"] = &session{
		ID:   "$1",
		Name: sessionName,
		windows: map[string]*window{
			"@1": {
				ID:       "@1",
				panesIDs: make(map[string]struct{}),
			},
		},
	}
	for i, pid := range paneIDs {
		title := ""
		if i < len(paneTitles) {
			title = paneTitles[i]
		}
		s.sessions["$1"].windows["@1"].panesIDs[pid] = struct{}{}
		s.panesByID[pid] = &pane{ID: pid, Title: title}
	}
	return s
}

// TestRecomputeAgents_CapturesOnTransition verifies that transitioning a pane
// title from a non-waiting to waiting glyph causes paneCapturer to be called
// exactly once with the pane's ID and the result stored in WaitContext.
func TestRecomputeAgents_CapturesOnTransition(t *testing.T) {
	// Test A: capture on transition INTO waiting.
	t.Run("A_capture_on_transition_into_waiting", func(t *testing.T) {
		s := buildTestState("example-agora", []string{"%1"}, []string{"claude"})

		var capturedID string
		var callCount int
		stubResult := "waiting because of tool use\npermission required\n"
		s.paneCapturer = func(paneID, socketName string) (string, error) {
			callCount++
			capturedID = paneID
			return stubResult, nil
		}

		// Transition to waiting.
		s.panesByID["%1"].Title = "● claude"
		recomputeAgents(s, "example-agora")

		if callCount != 1 {
			t.Errorf("paneCapturer call count = %d; want 1", callCount)
		}
		if capturedID != "%1" {
			t.Errorf("paneCapturer called with paneID %q; want %%1", capturedID)
		}
		if got := s.projectData["example-agora"].WaitContext; got != stubResult {
			t.Errorf("WaitContext = %q; want %q", got, stubResult)
		}
	})

	// Test B: clear-on-exit lives in buildSnapshot now (it cascades the
	// dependent fields off the WaitStartedTS lifecycle owned by
	// DeriveAttention). Verify the latch (no visit → stay set) and the
	// clear (visit + title moves → wipe) at that layer.
	t.Run("B_clear_lifecycle_lives_in_buildSnapshot", func(t *testing.T) {
		t.Run("no_visit_latches_wait_state", func(t *testing.T) {
			s := buildTestState("example-agora", []string{"%1"}, []string{"● claude"})
			s.projectListNames = []string{"example-agora"}
			s.paneCapturer = func(paneID, socketName string) (string, error) {
				return "some captured context\n", nil
			}

			// Enter waiting via the event path.
			recomputeAgents(s, "example-agora")
			// Bake the wait into the snapshot lifecycle.
			_ = buildSnapshot(s, 1, time.Now(), time.Now().Unix(), time.Now().UnixMilli())
			pd := s.projectData["example-agora"]
			if pd.WaitContext == "" {
				t.Fatal("pre-condition: WaitContext should be set after entering waiting")
			}
			if pd.WaitStartedTS == 0 {
				t.Fatal("pre-condition: WaitStartedTS should be non-zero after entering waiting")
			}
			waitStartedAt := pd.WaitStartedTS

			// Agent self-exits waiting without a visit — latch path.
			s.panesByID["%1"].Title = "shell"
			s.lastTitleChangeTS["example-agora"] = time.Now().Unix() + 1
			recomputeAgents(s, "example-agora")
			_ = buildSnapshot(s, 2, time.Now(), time.Now().Unix()+1, time.Now().UnixMilli()+1000)
			pd = s.projectData["example-agora"]

			if pd.WaitStartedTS != waitStartedAt {
				t.Errorf("WaitStartedTS = %d after no-visit exit; want %d (latched)", pd.WaitStartedTS, waitStartedAt)
			}
			if pd.WaitContext == "" {
				t.Errorf("WaitContext cleared after no-visit exit; want latched")
			}
		})

		t.Run("visit_then_title_change_clears", func(t *testing.T) {
			s := buildTestState("example-agora", []string{"%1"}, []string{"● claude"})
			s.projectListNames = []string{"example-agora"}
			s.paneCapturer = func(paneID, socketName string) (string, error) {
				return "some captured context\n", nil
			}

			recomputeAgents(s, "example-agora")
			_ = buildSnapshot(s, 1, time.Now(), time.Now().Unix(), time.Now().UnixMilli())
			if s.projectData["example-agora"].WaitContext == "" {
				t.Fatal("pre-condition: WaitContext should be set after entering waiting")
			}

			// Visit, then agent transitions out of waiting.
			s.lastVisitTS["example-agora"] = s.projectData["example-agora"].WaitStartedTS + 1

			var clearCallCount int
			s.paneCapturer = func(paneID, socketName string) (string, error) {
				clearCallCount++ // should NOT be called on clear
				return "", nil
			}
			s.panesByID["%1"].Title = "shell"
			s.lastTitleChangeTS["example-agora"] = s.lastVisitTS["example-agora"] - 1
			recomputeAgents(s, "example-agora")
			_ = buildSnapshot(s, 2, time.Now(), time.Now().Unix(), time.Now().UnixMilli())
			pd := s.projectData["example-agora"]

			if clearCallCount != 0 {
				t.Errorf("paneCapturer should NOT be called on exit from waiting; got %d calls", clearCallCount)
			}
			if pd.WaitContext != "" {
				t.Errorf("WaitContext = %q after visited exit; want empty", pd.WaitContext)
			}
			if pd.WaitStartedTS != 0 {
				t.Errorf("WaitStartedTS = %d after visited exit; want 0", pd.WaitStartedTS)
			}
		})
	})

	// Test C: no capture when already waiting (title text changes but status stays waiting).
	t.Run("C_no_capture_when_already_waiting", func(t *testing.T) {
		s := buildTestState("example-agora", []string{"%1"}, []string{"● claude"})

		var callCount int
		s.paneCapturer = func(paneID, socketName string) (string, error) {
			callCount++
			return "context v1\n", nil
		}

		// First call: transitions into waiting — captures once.
		recomputeAgents(s, "example-agora")
		if callCount != 1 {
			t.Fatalf("pre-condition: expected 1 capture on first enter-waiting; got %d", callCount)
		}

		// Second call: still waiting (title text changed but glyph remains ●).
		s.panesByID["%1"].Title = "● claude (some new prompt)"
		recomputeAgents(s, "example-agora")

		if callCount != 1 {
			t.Errorf("paneCapturer call count = %d after already-waiting recompute; want 1 (no additional capture)", callCount)
		}
	})

	// Test D: capturer error path — WaitContext remains empty, no panic.
	t.Run("D_capturer_error_leaves_wait_context_empty", func(t *testing.T) {
		s := buildTestState("example-agora", []string{"%1"}, []string{"claude"})
		s.paneCapturer = func(paneID, socketName string) (string, error) {
			return "", errors.New("boom")
		}

		s.panesByID["%1"].Title = "● claude"
		// Must not panic.
		recomputeAgents(s, "example-agora")

		if got := s.projectData["example-agora"].WaitContext; got != "" {
			t.Errorf("WaitContext = %q after capturer error; want empty", got)
		}
		if s.projectData["example-agora"].AgentStates["claude"] != "waiting" {
			t.Errorf("AgentStates[claude] = %q; want waiting (recomputeAgents must still complete)", s.projectData["example-agora"].AgentStates["claude"])
		}
	})

	// Test E: claude wins over pi on tiebreak.
	t.Run("E_claude_wins_over_pi_tiebreak", func(t *testing.T) {
		// Session with both a waiting claude pane and a waiting pi pane.
		s := buildTestState("example-agora", []string{"%1", "%2"}, []string{"● claude", "● pi"})

		var capturedID string
		s.paneCapturer = func(paneID, socketName string) (string, error) {
			capturedID = paneID
			return "captured\n", nil
		}

		recomputeAgents(s, "example-agora")

		if capturedID != "%1" {
			t.Errorf("paneCapturer called with paneID %q; want %%1 (claude pane, not pi)", capturedID)
		}
	})

	// Test F: sessions to skip must NOT invoke paneCapturer.
	t.Run("F_skip_sessions_do_not_capture", func(t *testing.T) {
		for _, sessName := range []string{
			"zdevd-watcher",
			"raw-events-abc",
			"sub-test-xyz",
			"test-control-foo",
		} {
			s := buildTestState(sessName, []string{"%1"}, []string{"● claude"})
			var called bool
			s.paneCapturer = func(paneID, socketName string) (string, error) {
				called = true
				return "captured\n", nil
			}

			recomputeAgents(s, sessName)

			if called {
				t.Errorf("session %q: paneCapturer should NOT be called for skipped sessions", sessName)
			}
		}
	})

	// Test G: buildSnapshot wires WaitContext from projectData to proto.Project.
	t.Run("G_buildSnapshot_wires_wait_context", func(t *testing.T) {
		// Present session (window + pane): absent rows suppress wait wire
		// fields by design (ghost-wait fix 2026-08-09), so the old
		// windowless setup would assert the suppressed value.
		s := buildTestState("example-agora", []string{"%1"}, []string{"shell"})
		pd := s.projectData["example-agora"]
		pd.WaitContext = "foo\nbar"
		s.projectData["example-agora"] = pd

		snap := buildSnapshot(s, 1, time.Now(), time.Now().Unix(), time.Now().UnixMilli())
		proj := findProject(snap.Projects, "example-agora")
		if proj == nil {
			t.Fatal("project 'example-agora' not found in snapshot")
		}
		if proj.WaitContext != "foo\nbar" {
			t.Errorf("WaitContext = %q; want %q", proj.WaitContext, "foo\nbar")
		}
	})

	// Test H: proto.SchemaVersion must be phase4-v26 (focus-loop wire
	// metadata — Project.Intent/BdReady, ZDEV_SIDEBAR_INITIATIVE).
	t.Run("H_schema_version_is_phase4_v26", func(t *testing.T) {
		if proto.SchemaVersion != "phase4-v26" {
			t.Errorf("SchemaVersion = %q; want %q", proto.SchemaVersion, "phase4-v26")
		}
	})
}

// TestApplyEvent_ProjectListChanged_StoresRepos verifies the consumer end of
// the S3 repo-threading chain (Lister.Refresh → ProjectListChanged →
// applyEvent → st.projectRepos): the resolved owner/repo map lands in state,
// and a later Refresh that drops a project replaces the map wholesale rather
// than leaving a stale entry behind.
func TestApplyEvent_ProjectListChanged_StoresRepos(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.ProjectListChanged{
		Names: []string{"zitcha/agora-a", "zitcha/agora-b"},
		Repos: map[string]string{
			"zitcha/agora-a": "zitcha/agora",
			"zitcha/agora-b": "zitcha/agora",
		},
	}, nil)
	if s.projectRepos["zitcha/agora-a"] != "zitcha/agora" ||
		s.projectRepos["zitcha/agora-b"] != "zitcha/agora" {
		t.Errorf("projectRepos = %v; want both agora-a/b → zitcha/agora", s.projectRepos)
	}

	// agora-b drops out of the workspace; the new event omits it. The map must
	// not retain the stale "agora-b → zitcha/agora" entry.
	applyEvent(s, tmuxctl.ProjectListChanged{
		Names: []string{"zitcha/agora-a"},
		Repos: map[string]string{"zitcha/agora-a": "zitcha/agora"},
	}, nil)
	if _, ok := s.projectRepos["zitcha/agora-b"]; ok {
		t.Errorf("projectRepos retained stale agora-b entry after it dropped: %v", s.projectRepos)
	}
}

// --- 260509-gfz: CIRefresh tests ---

// TestApplyEvent_CIRefresh_WritesProjectData verifies that CIRefresh populates
// CIStatus and CIConclusion on the project with normalized key (slash→dash).
func TestApplyEvent_CIRefresh_WritesProjectData(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.CIRefresh{
		Project: "example/backend", Status: "completed", Conclusion: "success",
	}, nil)
	pd := s.projectData["example-backend"]
	if pd.CIStatus != "completed" || pd.CIConclusion != "success" {
		t.Errorf("got %+v; want CIStatus=completed CIConclusion=success", pd)
	}
}

// TestApplyEvent_CIRefresh_ClearsStaleData verifies that a CIRefresh with
// empty Status/Conclusion clears prior state (the "no runs / branch gone" case).
func TestApplyEvent_CIRefresh_ClearsStaleData(t *testing.T) {
	s := newState()
	// Pre-populate with a prior value.
	pd := s.projectData["example-backend"]
	pd.CIStatus = "completed"
	pd.CIConclusion = "failure"
	s.projectData["example-backend"] = pd

	applyEvent(s, tmuxctl.CIRefresh{
		Project: "example/backend", Status: "", Conclusion: "",
	}, nil)
	got := s.projectData["example-backend"]
	if got.CIStatus != "" || got.CIConclusion != "" {
		t.Errorf("got %+v; want empty CIStatus and CIConclusion after clear", got)
	}
}

// TestBuildSnapshot_PropagatesCI verifies that projectData.CIStatus and
// CIConclusion appear in the built proto.Project.
func TestBuildSnapshot_PropagatesCI(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	applyEvent(s, tmuxctl.CIRefresh{
		Project: "example/backend", Status: "completed", Conclusion: "failure",
	}, nil)
	snap := buildSnapshot(s, 1, time.Now(), time.Now().Unix(), time.Now().UnixMilli())
	if len(snap.Projects) != 1 {
		t.Fatalf("len(Projects)=%d; want 1", len(snap.Projects))
	}
	p := snap.Projects[0]
	if p.CIStatus != "completed" || p.CIConclusion != "failure" {
		t.Errorf("Project=%+v; want CIStatus=completed CIConclusion=failure", p)
	}
}

func TestPaneRequestChangedProjectsThroughSnapshotAndClears(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	applyEvent(s, tmuxctl.PaneRequestChanged{Session: "example-backend", Requested: true, Title: "tests", Timestamp: 123}, nil)
	snap := buildSnapshot(s, 1, time.Unix(200, 0), 200, 200000)
	if len(snap.PaneRequests) != 1 || snap.PaneRequests[0] != (proto.PaneRequest{Session: "example-backend", Title: "tests", TS: 123}) {
		t.Fatalf("pane request projection = %+v", snap.PaneRequests)
	}
	applyEvent(s, tmuxctl.PaneRequestChanged{Session: "example-backend"}, nil)
	snap = buildSnapshot(s, 2, time.Unix(201, 0), 201, 201000)
	if len(snap.PaneRequests) != 0 {
		t.Fatalf("cleared pane request projection = %+v", snap.PaneRequests)
	}
}

func TestPaneRequestChangedSkipsSyntheticSessions(t *testing.T) {
	s := newState()
	for _, session := range []string{"", "zdevd-watcher", "raw-events-1", "sub-test-1", "test-control-1", "$_unlinked-1"} {
		applyEvent(s, tmuxctl.PaneRequestChanged{Session: session, Requested: true}, nil)
	}
	if len(s.projectData) != 0 {
		t.Fatalf("synthetic requests entered state: %v", s.projectData)
	}
}

// --- helpers ---

// mustSubmit calls h.Submit and fails the test on error.
func mustSubmit(t *testing.T, h *Hub, ev tmuxctl.Event) {
	t.Helper()
	if err := h.Submit(ev); err != nil {
		t.Fatalf("Submit(%T): %v", ev, err)
	}
}

// mustSubscribe registers a subscriber and returns it with an unsubscribe func.
func mustSubscribe(t *testing.T, h *Hub, pane string) (*Subscriber, func()) {
	t.Helper()
	sub := NewSubscriber(pane, "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone
	return sub, func() { h.Unregister(sub) }
}

// drainUntil reads snapshots until pred returns true or timeout elapses.
// Returns the first snapshot satisfying pred; fatals if timeout reached.
func drainUntil(t *testing.T, sub *Subscriber, timeout time.Duration, pred func(*proto.Snapshot) bool) *proto.Snapshot {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case snap := <-sub.Snaps():
			if pred(snap) {
				return snap
			}
		case <-deadline:
			t.Fatalf("drainUntil: no satisfying snapshot within %v", timeout)
			return nil
		}
	}
}

// findProject returns the proto.Project with the given name, or nil.
func findProject(projects []proto.Project, name string) *proto.Project {
	for i := range projects {
		if projects[i].Name == name {
			return &projects[i]
		}
	}
	return nil
}

// --- 260511-o8g: WaitAcknowledged propagation tests ---

// TestBuildSnapshot_WaitAcknowledged_VisitPostDatesHighestTier verifies that
// WaitAcknowledged is true when the user visited the session after the highest
// crossed wait-tier threshold. waitStartedTS = now-600 crosses the 300s tier;
// visitTS = now-60 > waitStartedTS+300 = now-300, so it acknowledges.
func TestBuildSnapshot_WaitAcknowledged_VisitPostDatesHighestTier(t *testing.T) {
	now := time.Now().Unix()
	// Present, actively waiting session — absent rows suppress wait wire
	// fields by design (ghost-wait fix 2026-08-09). The title change
	// post-dates the visit so the visited-since-wait demotion stays out
	// of this test's way; the visit still post-dates the highest crossed
	// tier, which is what the assertion is about.
	s := buildTestState("foo-bar", []string{"%1"}, []string{"● claude"})
	s.projectListNames = []string{"foo/bar"}
	pd := s.projectData["foo-bar"]
	pd.WaitStartedTS = now - 600
	s.projectData["foo-bar"] = pd
	s.lastVisitTS["foo-bar"] = now - 60 // post-dates waitStartedTS+300 (= now-300)
	s.lastTitleChangeTS["foo-bar"] = now - 50

	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	proj := findProject(snap.Projects, "foo/bar")
	if proj == nil {
		t.Fatal("project 'foo/bar' not found in snapshot")
	}
	if !proj.WaitAcknowledged {
		t.Errorf("WaitAcknowledged = false; want true (visit post-dates highest crossed tier)")
	}
}

// TestBuildSnapshot_WaitAcknowledged_NoVisit verifies that WaitAcknowledged
// is false when the project is waiting but lastVisitTS has no entry.
func TestBuildSnapshot_WaitAcknowledged_NoVisit(t *testing.T) {
	now := time.Now().Unix()
	s := newState()
	s.paneCapturer = func(paneID, socketName string) (string, error) { return "", nil }
	s.projectListNames = []string{"foo/bar"}
	pd := s.projectData["foo-bar"]
	pd.WaitStartedTS = now - 600
	s.projectData["foo-bar"] = pd
	// no entry in lastVisitTS

	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	proj := findProject(snap.Projects, "foo/bar")
	if proj == nil {
		t.Fatal("project 'foo/bar' not found in snapshot")
	}
	if proj.WaitAcknowledged {
		t.Errorf("WaitAcknowledged = true; want false (no visit recorded)")
	}
}

// TestBuildSnapshot_FiltersEmptyNameSession verifies that a session with an
// empty Name in state.sessions does NOT surface as a blank row in the
// snapshot. Empty-name sessions can be created when a SessionChanged event
// arrives during bootstrap before SessionRenamed binds a name; the record
// must persist in state (keyed by ID for later rename) but must not appear
// in the wire snapshot.
func TestBuildSnapshot_FiltersEmptyNameSession(t *testing.T) {
	s := newState()
	s.paneCapturer = func(paneID, socketName string) (string, error) { return "", nil }
	// One real session and one empty-name session.
	s.sessions["$1"] = &session{ID: "$1", Name: "alpha", windows: make(map[string]*window)}
	s.sessions["$2"] = &session{ID: "$2", Name: "", windows: make(map[string]*window)}

	snap := buildSnapshot(s, 1, time.Now(), time.Now().Unix(), time.Now().UnixMilli())

	for _, n := range snap.Sessions {
		if n == "" {
			t.Errorf("Sessions contains empty-string entry: %v", snap.Sessions)
		}
	}
	for _, p := range snap.Projects {
		if p.Name == "" {
			t.Errorf("Projects contains entry with empty Name: %+v", p)
		}
	}
	if findProject(snap.Projects, "alpha") == nil {
		t.Errorf("project 'alpha' missing from snapshot (real session should not be dropped)")
	}
}

// TestBuildSnapshot_StaleWaitingTitleDemotedAfterVisit covers the live
// scenario where Claude Code leaves a `✳ <task>` pane title behind when it
// returns to its idle prompt. Without the demoter, deriveStatus would keep
// reporting the session as `waiting` forever and visits would never quiet
// the chip. With the demoter, a visit that post-dates the most recent pane-
// title change demotes `waiting` to `alive`; a fresh title change re-elevates.
func TestBuildSnapshot_StaleWaitingTitleDemotedAfterVisit(t *testing.T) {
	now := time.Now().Unix()
	build := func() *state {
		s := buildTestState("example-agora-c", []string{"%15"}, []string{"✳ Check CI for PR #784"})
		s.projectListNames = []string{"example/agora-c"}
		return s
	}

	t.Run("no_visit_keeps_waiting", func(t *testing.T) {
		s := build()
		s.lastTitleChangeTS["example-agora-c"] = now - 60
		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		proj := findProject(snap.Projects, "example/agora-c")
		if proj == nil {
			t.Fatal("project missing")
		}
		if proj.Status != tmuxctl.StatusWaiting {
			t.Errorf("Status = %q; want waiting (no visit yet)", proj.Status)
		}
	})

	t.Run("visit_after_last_title_change_demotes", func(t *testing.T) {
		s := build()
		s.lastTitleChangeTS["example-agora-c"] = now - 60
		s.lastVisitTS["example-agora-c"] = now - 30 // post-dates the title
		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		proj := findProject(snap.Projects, "example/agora-c")
		if proj == nil {
			t.Fatal("project missing")
		}
		if proj.Status != tmuxctl.StatusAlive {
			t.Errorf("Status = %q; want alive (visited since title last moved)", proj.Status)
		}
	})

	t.Run("title_change_after_visit_re_elevates", func(t *testing.T) {
		s := build()
		s.lastVisitTS["example-agora-c"] = now - 60
		s.lastTitleChangeTS["example-agora-c"] = now - 30 // post-dates the visit
		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		proj := findProject(snap.Projects, "example/agora-c")
		if proj == nil {
			t.Fatal("project missing")
		}
		if proj.Status != tmuxctl.StatusWaiting {
			t.Errorf("Status = %q; want waiting (title moved after the last visit)", proj.Status)
		}
	})
}

// TestBuildSnapshot_WaitAcknowledged_NotWaiting verifies that WaitAcknowledged
// is false when WaitStartedTS == 0 (project is not in a waiting state), even
// if lastVisitTS is populated.
func TestBuildSnapshot_WaitAcknowledged_NotWaiting(t *testing.T) {
	now := time.Now().Unix()
	s := newState()
	s.paneCapturer = func(paneID, socketName string) (string, error) { return "", nil }
	s.projectListNames = []string{"foo/bar"}
	// WaitStartedTS is 0 (the zero value — not waiting)
	s.lastVisitTS["foo-bar"] = now - 30 // has a visit entry, but irrelevant

	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	proj := findProject(snap.Projects, "foo/bar")
	if proj == nil {
		t.Fatal("project 'foo/bar' not found in snapshot")
	}
	if proj.WaitAcknowledged {
		t.Errorf("WaitAcknowledged = true; want false (WaitStartedTS == 0, not waiting)")
	}
}

// TestPaneCaptureFailed_EvictsAfterThreshold verifies that the ghost-pane
// guard removes a pane from state tracking once consecutive PaneCaptureFailed
// events reach maxConsecutiveCaptureFailures. Reproduces bug 260528: a
// session killed externally left a pane reference in panesByID that
// recomputeAgents kept selecting for capture, flooding the eventlog channel.
func TestPaneCaptureFailed_EvictsAfterThreshold(t *testing.T) {
	t.Run("evicts_after_threshold_failures", func(t *testing.T) {
		s := buildTestState("ghost-sess", []string{"%83"}, []string{"● claude"})

		// First N-1 failures only increment the counter.
		for i := 1; i < maxConsecutiveCaptureFailures; i++ {
			applyEvent(s, tmuxctl.PaneCaptureFailed{Session: "ghost-sess", PaneID: "%83"}, nil)
			if _, ok := s.panesByID["%83"]; !ok {
				t.Fatalf("pane evicted prematurely after %d failures", i)
			}
			if got := s.paneCaptureFailures["%83"]; got != i {
				t.Errorf("failure count after %d events = %d; want %d", i, got, i)
			}
		}

		// The threshold-th failure triggers eviction.
		applyEvent(s, tmuxctl.PaneCaptureFailed{Session: "ghost-sess", PaneID: "%83"}, nil)

		if _, ok := s.panesByID["%83"]; ok {
			t.Errorf("pane %%83 still in panesByID after threshold; want evicted")
		}
		if _, ok := s.paneCaptureFailures["%83"]; ok {
			t.Errorf("failure counter for %%83 not cleared after eviction")
		}
		// Also gone from the window's panesIDs map.
		if _, ok := s.sessions["$1"].windows["@1"].panesIDs["%83"]; ok {
			t.Errorf("pane %%83 still in window.panesIDs after eviction")
		}
	})

	t.Run("success_resets_counter", func(t *testing.T) {
		s := buildTestState("live-sess", []string{"%99"}, []string{"● claude"})
		// Pretend the pane is in waiting state so PaneCaptureReady applies.
		s.projectData["live-sess"] = projectData{AgentStates: map[string]string{"claude": "waiting"}}

		// Two failures, then a success.
		applyEvent(s, tmuxctl.PaneCaptureFailed{Session: "live-sess", PaneID: "%99"}, nil)
		applyEvent(s, tmuxctl.PaneCaptureFailed{Session: "live-sess", PaneID: "%99"}, nil)
		if got := s.paneCaptureFailures["%99"]; got != 2 {
			t.Fatalf("setup: failure count = %d; want 2", got)
		}

		applyEvent(s, tmuxctl.PaneCaptureReady{Session: "live-sess", Text: "stdout"}, nil)
		if _, ok := s.paneCaptureFailures["%99"]; ok {
			t.Errorf("PaneCaptureReady did not clear failure counter for %%99")
		}

		// A subsequent failure should restart counting from 1, not pick up at 3.
		applyEvent(s, tmuxctl.PaneCaptureFailed{Session: "live-sess", PaneID: "%99"}, nil)
		if got := s.paneCaptureFailures["%99"]; got != 1 {
			t.Errorf("after success+failure, count = %d; want 1", got)
		}
		if _, ok := s.panesByID["%99"]; !ok {
			t.Errorf("pane evicted after only 1 failure post-reset")
		}
	})
}

// TestCursorMove verifies the sidebar cursor state machine implemented in
// applyEvent(CursorMove) and the projectNameAtRow helper.
func TestCursorMove(t *testing.T) {
	makeState := func(projectNames ...string) *state {
		s := newState()
		s.projectListNames = projectNames
		return s
	}

	t.Run("first_press_activates_at_row_zero", func(t *testing.T) {
		s := makeState("alpha", "beta", "gamma")
		if s.cursorActive {
			t.Fatal("cursor should start inactive")
		}
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil)
		if !s.cursorActive {
			t.Error("cursor should be active after first press")
		}
		if s.cursorRow != 0 {
			t.Errorf("cursorRow = %d; want 0 (first press always activates at row 0)", s.cursorRow)
		}
	})

	t.Run("moves_down", func(t *testing.T) {
		s := makeState("alpha", "beta", "gamma")
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // activate at 0
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // move to 1
		if s.cursorRow != 1 {
			t.Errorf("cursorRow = %d; want 1", s.cursorRow)
		}
	})

	t.Run("wraps_down_past_end", func(t *testing.T) {
		s := makeState("alpha", "beta", "gamma")          // n=3
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // activate 0
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // 1
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // 2
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // wraps to 0
		if s.cursorRow != 0 {
			t.Errorf("cursorRow = %d; want 0 (wrap-around)", s.cursorRow)
		}
	})

	t.Run("wraps_up_past_start", func(t *testing.T) {
		s := makeState("alpha", "beta", "gamma")          // n=3
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // activate 0
		applyEvent(s, tmuxctl.CursorMove{Delta: -1}, nil) // wraps to 2
		if s.cursorRow != 2 {
			t.Errorf("cursorRow = %d; want 2 (reverse wrap)", s.cursorRow)
		}
	})

	t.Run("select_delta_zero_does_not_move", func(t *testing.T) {
		s := makeState("alpha", "beta", "gamma")
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // activate at 0
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil) // move to 1
		applyEvent(s, tmuxctl.CursorMove{Delta: 0}, nil)  // select — must not move
		if s.cursorRow != 1 {
			t.Errorf("cursorRow = %d after delta=0; want 1 (select must not move)", s.cursorRow)
		}
	})

	t.Run("noop_when_no_projects", func(t *testing.T) {
		s := makeState()
		applyEvent(s, tmuxctl.CursorMove{Delta: +1}, nil)
		if s.cursorActive {
			t.Error("cursor must stay inactive when project list is empty")
		}
	})

	t.Run("projectNameAtRow_returns_correct_name", func(t *testing.T) {
		s := makeState("alpha", "beta", "gamma")
		for i, want := range []string{"alpha", "beta", "gamma"} {
			got := projectNameAtRow(s, i)
			if got != want {
				t.Errorf("projectNameAtRow(%d) = %q; want %q", i, got, want)
			}
		}
		if got := projectNameAtRow(s, -1); got != "" {
			t.Errorf("projectNameAtRow(-1) = %q; want \"\"", got)
		}
		if got := projectNameAtRow(s, 99); got != "" {
			t.Errorf("projectNameAtRow(99) = %q; want \"\"", got)
		}
	})
}

// TestApplyEvent_PaneCwdChanged verifies that PaneCwdChanged records the
// pane's working directory onto the pane struct in state (zd-bub). Both
// existing-pane and discover-on-cwd paths must populate Cwd so consumers
// reading from the snapshot don't need a second tmux query.
func TestApplyEvent_PaneCwdChanged(t *testing.T) {
	t.Run("updates_existing_pane", func(t *testing.T) {
		s := newState()
		s.panesByID["%5"] = &pane{ID: "%5", Title: "● claude"}
		applyEvent(s, tmuxctl.PaneCwdChanged{
			SessionName: "gt-zdev-obsidian",
			PaneID:      "%5",
			Cwd:         "/work/zdev/polecats/obsidian",
		}, nil)
		got := s.panesByID["%5"]
		if got.Cwd != "/work/zdev/polecats/obsidian" {
			t.Errorf("pane.Cwd = %q; want %q", got.Cwd, "/work/zdev/polecats/obsidian")
		}
		if got.Title != "● claude" {
			t.Errorf("pane.Title clobbered: got %q; want %q", got.Title, "● claude")
		}
	})

	t.Run("creates_pane_when_unknown", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.PaneCwdChanged{
			SessionName: "gt-zdev-obsidian",
			PaneID:      "%9",
			Cwd:         "/work/zdev",
		}, nil)
		got, ok := s.panesByID["%9"]
		if !ok {
			t.Fatal("pane %9 should have been created from PaneCwdChanged")
		}
		if got.Cwd != "/work/zdev" {
			t.Errorf("pane.Cwd = %q; want /work/zdev", got.Cwd)
		}
		if got.ID != "%9" {
			t.Errorf("pane.ID = %q; want %%9", got.ID)
		}
	})
}

// TestSessionSocketAttribution_zd47u covers the socket-aware paneCapturer
// plumbing: a SessionChanged event carrying SocketName tags the session in
// sessionSocket, a subsequent recomputeAgents call must thread that socket
// name through paneCapturer, and a default-socket session yields the empty
// socket string (no -L flag).
func TestSessionSocketAttribution_zd47u(t *testing.T) {
	t.Run("gt_socket_session_routes_capture", func(t *testing.T) {
		s := newState()
		// Capture the socket name observed by the capturer so we can assert
		// the lookup against sessionSocket actually plumbed through.
		var observedSocket string
		var calls int
		s.paneCapturer = func(paneID, socketName string) (string, error) {
			calls++
			observedSocket = socketName
			return "ctx\n", nil
		}

		// Daemon emits SessionChanged tagged with the GT socket name.
		applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "hq-mayor", SocketName: "gt-abc123"}, nil)
		if got := s.sessionSocket["hq-mayor"]; got != "gt-abc123" {
			t.Fatalf("sessionSocket[hq-mayor] = %q; want gt-abc123", got)
		}

		// Wire a window+pane so recomputeAgents has something to walk.
		s.sessions["$1"].windows["@1"] = &window{ID: "@1", panesIDs: map[string]struct{}{"%1": {}}}
		s.panesByID["%1"] = &pane{ID: "%1", Title: "● claude"}

		recomputeAgents(s, "hq-mayor")

		if calls != 1 {
			t.Fatalf("paneCapturer calls = %d; want 1", calls)
		}
		if observedSocket != "gt-abc123" {
			t.Errorf("paneCapturer socketName = %q; want gt-abc123 — GT panes would fail without this", observedSocket)
		}
	})

	t.Run("default_socket_session_yields_empty_socket", func(t *testing.T) {
		s := newState()
		var observedSocket string
		s.paneCapturer = func(paneID, socketName string) (string, error) {
			observedSocket = socketName
			return "", nil
		}

		applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "example-agora", SocketName: ""}, nil)
		s.sessions["$1"].windows["@1"] = &window{ID: "@1", panesIDs: map[string]struct{}{"%1": {}}}
		s.panesByID["%1"] = &pane{ID: "%1", Title: "● claude"}

		recomputeAgents(s, "example-agora")

		if observedSocket != "" {
			t.Errorf("paneCapturer socketName = %q; want empty (default socket)", observedSocket)
		}
	})

	t.Run("rename_retags_and_drops_old_key", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.SessionChanged{ID: "$1", Name: "old-name", SocketName: "gt-abc"}, nil)
		applyEvent(s, tmuxctl.SessionRenamed{ID: "$1", NewName: "new-name", SocketName: "gt-abc"}, nil)

		if _, stale := s.sessionSocket["old-name"]; stale {
			t.Error("sessionSocket[old-name] should be cleared after rename")
		}
		if got := s.sessionSocket["new-name"]; got != "gt-abc" {
			t.Errorf("sessionSocket[new-name] = %q; want gt-abc", got)
		}
	})
}
