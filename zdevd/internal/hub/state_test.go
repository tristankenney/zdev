package hub

import (
	"errors"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

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

// TestApplyNotifSeen verifies WaitStartedTS is set from NotifSeen.
func TestApplyNotifSeen(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%notif")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	const ts = int64(1714838460)
	mustSubmit(t, h, tmuxctl.NotifSeen{Session: "alpha", Timestamp: ts})
	snap := drainUntil(t, sub, 200*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.WaitStartedTS == ts
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.WaitStartedTS != ts {
		t.Errorf("WaitStartedTS = %d; want %d", proj.WaitStartedTS, ts)
	}
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

// TestApplyAgent_PaneTitleChanged_ClaudeWaiting verifies AgentClaude="waiting"
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
		return p != nil && p.AgentClaude == "waiting"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentClaude != "waiting" {
		t.Errorf("AgentClaude = %q; want %q", proj.AgentClaude, "waiting")
	}
	if proj.AgentPi != "" {
		t.Errorf("AgentPi = %q; want %q", proj.AgentPi, "")
	}
}

// TestApplyAgent_PaneTitleChanged_ClaudeIdle verifies that a pane titled
// "✳ Claude Code" (Claude Code v2.1+ idle prompt, no active task) does NOT
// set AgentClaude="waiting". Without this guard, every Claude session pulses
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
	if proj.AgentClaude != "" {
		t.Errorf("AgentClaude = %q; want \"\" (idle prompt should not pulse)", proj.AgentClaude)
	}
	if proj.AgentPi != "" {
		t.Errorf("AgentPi = %q; want \"\"", proj.AgentPi)
	}
}

// TestApplyAgent_PaneTitleChanged_ClaudeFinished verifies AgentClaude="finished"
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
		return p != nil && p.AgentClaude == "finished"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentClaude != "finished" {
		t.Errorf("AgentClaude = %q; want %q", proj.AgentClaude, "finished")
	}
	if proj.AgentPi != "" {
		t.Errorf("AgentPi = %q; want %q", proj.AgentPi, "")
	}
}

// TestApplyAgent_PaneTitleChanged_Pi verifies AgentPi="waiting" when a
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
		return p != nil && p.AgentPi == "waiting"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentClaude != "" {
		t.Errorf("AgentClaude = %q; want %q", proj.AgentClaude, "")
	}
	if proj.AgentPi != "waiting" {
		t.Errorf("AgentPi = %q; want %q", proj.AgentPi, "waiting")
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
	if proj.AgentClaude != "" {
		t.Errorf("AgentClaude = %q; want %q (no-agent title)", proj.AgentClaude, "")
	}
	if proj.AgentPi != "" {
		t.Errorf("AgentPi = %q; want %q (no-agent title)", proj.AgentPi, "")
	}
}

// TestApplyAgent_MultipleAgentsInSession verifies that a session with both a
// claude and a pi pane returns both AgentClaude and AgentPi populated.
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
		return p != nil && p.AgentClaude != "" && p.AgentPi != ""
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentClaude != "waiting" {
		t.Errorf("AgentClaude = %q; want %q", proj.AgentClaude, "waiting")
	}
	if proj.AgentPi != "finished" {
		t.Errorf("AgentPi = %q; want %q", proj.AgentPi, "finished")
	}
}

// TestApplyAgent_AgentClearsOnTitleChange verifies that after AgentClaude was
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
		return p != nil && p.AgentClaude == "waiting"
	})

	// Now change the title to "shell" — agent should clear.
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})
	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "alpha")
		return p != nil && p.AgentClaude == ""
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	if proj.AgentClaude != "" {
		t.Errorf("AgentClaude = %q; want %q (cleared after title change)", proj.AgentClaude, "")
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
		return p != nil && p.AgentClaude == "waiting"
	})
	proj := findProject(snap.Projects, "alpha")
	if proj == nil {
		t.Fatal("project 'alpha' not found in snapshot")
	}
	// Phase 2 VIS-01: Status must remain "waiting" from ClassifyPaneTitle path.
	if proj.Status != "waiting" {
		t.Errorf("Status = %q; want %q (Phase 2 VIS-01 regression — deriveStatus path broken)", proj.Status, "waiting")
	}
	// Phase 3 DATA-08: AgentClaude set via ClassifyAgent.
	if proj.AgentClaude != "waiting" {
		t.Errorf("AgentClaude = %q; want %q", proj.AgentClaude, "waiting")
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
	s.paneCapturer = func(paneID string) (string, error) { return "", nil }

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
		s.paneCapturer = func(paneID string) (string, error) {
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

	// Test B: clear on transition OUT of waiting.
	t.Run("B_clear_on_transition_out_of_waiting", func(t *testing.T) {
		s := buildTestState("example-agora", []string{"%1"}, []string{"● claude"})
		s.paneCapturer = func(paneID string) (string, error) {
			return "some captured context\n", nil
		}

		// First: transition into waiting to populate WaitContext.
		recomputeAgents(s, "example-agora")
		if s.projectData["example-agora"].WaitContext == "" {
			t.Fatal("pre-condition: WaitContext should be set after entering waiting")
		}

		// Now transition OUT of waiting.
		var clearCallCount int
		s.paneCapturer = func(paneID string) (string, error) {
			clearCallCount++ // should NOT be called on clear
			return "", nil
		}
		s.panesByID["%1"].Title = "shell"
		recomputeAgents(s, "example-agora")

		if clearCallCount != 0 {
			t.Errorf("paneCapturer should NOT be called on exit from waiting; got %d calls", clearCallCount)
		}
		if got := s.projectData["example-agora"].WaitContext; got != "" {
			t.Errorf("WaitContext = %q after exit from waiting; want empty", got)
		}
	})

	// Test C: no capture when already waiting (title text changes but status stays waiting).
	t.Run("C_no_capture_when_already_waiting", func(t *testing.T) {
		s := buildTestState("example-agora", []string{"%1"}, []string{"● claude"})

		var callCount int
		s.paneCapturer = func(paneID string) (string, error) {
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
		s.paneCapturer = func(paneID string) (string, error) {
			return "", errors.New("boom")
		}

		s.panesByID["%1"].Title = "● claude"
		// Must not panic.
		recomputeAgents(s, "example-agora")

		if got := s.projectData["example-agora"].WaitContext; got != "" {
			t.Errorf("WaitContext = %q after capturer error; want empty", got)
		}
		if s.projectData["example-agora"].AgentClaude != "waiting" {
			t.Errorf("AgentClaude = %q; want waiting (recomputeAgents must still complete)", s.projectData["example-agora"].AgentClaude)
		}
	})

	// Test E: claude wins over pi on tiebreak.
	t.Run("E_claude_wins_over_pi_tiebreak", func(t *testing.T) {
		// Session with both a waiting claude pane and a waiting pi pane.
		s := buildTestState("example-agora", []string{"%1", "%2"}, []string{"● claude", "● pi"})

		var capturedID string
		s.paneCapturer = func(paneID string) (string, error) {
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
			s.paneCapturer = func(paneID string) (string, error) {
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
		s := newState()
		s.paneCapturer = func(paneID string) (string, error) { return "", nil }
		s.sessions["$1"] = &session{ID: "$1", Name: "example-agora", windows: make(map[string]*window)}
		pd := s.projectData["example-agora"]
		pd.WaitContext = "foo\nbar"
		s.projectData["example-agora"] = pd

		snap := buildSnapshot(s, 1, time.Now(), time.Now().Unix())
		proj := findProject(snap.Projects, "example-agora")
		if proj == nil {
			t.Fatal("project 'example-agora' not found in snapshot")
		}
		if proj.WaitContext != "foo\nbar" {
			t.Errorf("WaitContext = %q; want %q", proj.WaitContext, "foo\nbar")
		}
	})

	// Test H: proto.SchemaVersion must be phase4-v7 (bumped by 260515 for
	// Snapshot.PaneVisible addition).
	t.Run("H_schema_version_is_phase4_v7", func(t *testing.T) {
		if proto.SchemaVersion != "phase4-v7" {
			t.Errorf("SchemaVersion = %q; want %q", proto.SchemaVersion, "phase4-v7")
		}
	})
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
	snap := buildSnapshot(s, 1, time.Now(), time.Now().Unix())
	if len(snap.Projects) != 1 {
		t.Fatalf("len(Projects)=%d; want 1", len(snap.Projects))
	}
	p := snap.Projects[0]
	if p.CIStatus != "completed" || p.CIConclusion != "failure" {
		t.Errorf("Project=%+v; want CIStatus=completed CIConclusion=failure", p)
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
	s := newState()
	s.paneCapturer = func(paneID string) (string, error) { return "", nil }
	s.projectListNames = []string{"foo/bar"}
	pd := s.projectData["foo-bar"]
	pd.WaitStartedTS = now - 600
	s.projectData["foo-bar"] = pd
	s.lastVisitTS["foo-bar"] = now - 60 // post-dates waitStartedTS+300 (= now-300)

	snap := buildSnapshot(s, 1, time.Now(), now)
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
	s.paneCapturer = func(paneID string) (string, error) { return "", nil }
	s.projectListNames = []string{"foo/bar"}
	pd := s.projectData["foo-bar"]
	pd.WaitStartedTS = now - 600
	s.projectData["foo-bar"] = pd
	// no entry in lastVisitTS

	snap := buildSnapshot(s, 1, time.Now(), now)
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
	s.paneCapturer = func(paneID string) (string, error) { return "", nil }
	// One real session and one empty-name session.
	s.sessions["$1"] = &session{ID: "$1", Name: "alpha", windows: make(map[string]*window)}
	s.sessions["$2"] = &session{ID: "$2", Name: "", windows: make(map[string]*window)}

	snap := buildSnapshot(s, 1, time.Now(), time.Now().Unix())

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

// TestBuildSnapshot_WaitAcknowledged_NotWaiting verifies that WaitAcknowledged
// is false when WaitStartedTS == 0 (project is not in a waiting state), even
// if lastVisitTS is populated.
func TestBuildSnapshot_WaitAcknowledged_NotWaiting(t *testing.T) {
	now := time.Now().Unix()
	s := newState()
	s.paneCapturer = func(paneID string) (string, error) { return "", nil }
	s.projectListNames = []string{"foo/bar"}
	// WaitStartedTS is 0 (the zero value — not waiting)
	s.lastVisitTS["foo-bar"] = now - 30 // has a visit entry, but irrelevant

	snap := buildSnapshot(s, 1, time.Now(), now)
	proj := findProject(snap.Projects, "foo/bar")
	if proj == nil {
		t.Fatal("project 'foo/bar' not found in snapshot")
	}
	if proj.WaitAcknowledged {
		t.Errorf("WaitAcknowledged = true; want false (WaitStartedTS == 0, not waiting)")
	}
}
