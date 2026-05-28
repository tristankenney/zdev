package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestBuildSnapshot_SessionNameNormalization locks D-01/D-02: a project
// name in slash form ("zitcha/backend") MUST resolve to a tmux session
// whose Name is the dash form ("zitcha-backend") via lookup-time
// normalization in buildSnapshot. Without the fix, nameToSession[n]
// returns nil and deriveStatus returns "absent" for every sub-project.
func TestBuildSnapshot_SessionNameNormalization(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%norm-1")
	defer unsub()

	// Establish a tmux session whose Name is the dash form. Add a window
	// and pane so deriveStatus has something to inspect.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "zitcha-backend"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})

	// Project list arrives in slash form, as `zdev --list-projects` emits.
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"zitcha/backend"}})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "zitcha/backend")
		return p != nil && p.Status != "absent"
	})
	proj := findProject(snap.Projects, "zitcha/backend")
	if proj == nil {
		t.Fatal("project 'zitcha/backend' not found in snapshot")
	}
	if proj.Status == "absent" {
		t.Errorf("Status = %q; want non-absent — slash/dash normalization missing in buildSnapshot", proj.Status)
	}
}

// TestBuildSnapshot_SessionNameNormalization_NoMatchStaysAbsent locks the
// negative case: a project listed without any matching tmux session MUST
// remain absent. The fix is lookup-time only; it must not invent sessions.
func TestBuildSnapshot_SessionNameNormalization_NoMatchStaysAbsent(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%norm-2")
	defer unsub()

	// No SessionChanged for "phantom-repo". Only the project list.
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"phantom/repo"}})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "phantom/repo") != nil
	})
	proj := findProject(snap.Projects, "phantom/repo")
	if proj == nil {
		t.Fatal("project 'phantom/repo' not in snapshot")
	}
	if proj.Status != "absent" {
		t.Errorf("Status = %q; want \"absent\" (no tmux session exists)", proj.Status)
	}
}

// TestBuildSnapshot_ProjectListNameStaysCanonical locks D-03: the
// proto.Project.Name field is the canonical slash form, never the
// normalized dash form. Normalization is lookup-key only.
func TestBuildSnapshot_ProjectListNameStaysCanonical(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%norm-3")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "zitcha-backend"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"zitcha/backend"}})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "zitcha/backend") != nil
	})
	proj := findProject(snap.Projects, "zitcha/backend")
	if proj == nil {
		t.Fatal("project 'zitcha/backend' not in snapshot")
	}
	if proj.Name != "zitcha/backend" {
		t.Errorf("Name = %q; want canonical slash form %q", proj.Name, "zitcha/backend")
	}
}

// TestBuildSnapshot_NoDuplicateRows locks the deduplication fix: when a
// project appears in both the project list (slash form, e.g. "zitcha/backend")
// and as a tmux session (dash form, e.g. "zitcha-backend"), the snapshot MUST
// contain exactly ONE row for that project, not two.
func TestBuildSnapshot_NoDuplicateRows(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%norm-4")
	defer unsub()

	// Session exists with dash-form name.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "zitcha-backend"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "shell"})

	// Project list arrives with slash-form name for the same project.
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"zitcha/backend"}})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "zitcha/backend") != nil
	})

	// Must appear exactly once as the slash-form canonical name.
	count := 0
	for _, p := range snap.Projects {
		if p.Name == "zitcha/backend" || p.Name == "zitcha-backend" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d rows for zitcha/* project; want exactly 1 (no duplicate dash+slash rows)", count)
	}

	// Also assert the Sessions wire-format field is deduplicated.
	sessionCount := 0
	for _, s := range snap.Sessions {
		if s == "zitcha/backend" || s == "zitcha-backend" {
			sessionCount++
		}
	}
	if sessionCount != 1 {
		t.Errorf("found %d entries in Sessions for zitcha/* project; want exactly 1", sessionCount)
	}
}

// TestSnapWithCurrentSession_SlashProjectHighlight locks the highlight fix:
// when a renderer pane belongs to a tmux session named "zitcha-backend"
// (dash-form), and the project list contains the canonical slash-form name
// "zitcha/backend", the resulting snapshot's CurrentSession MUST be
// "zitcha/backend" (slash-form) so the renderer's p.Name == CurrentSession
// comparison resolves correctly and the project row is highlighted.
//
// Without the fix snapWithCurrentSession sets CurrentSession = "zitcha-backend"
// (raw session name), which never matches p.Name = "zitcha/backend", so no
// project is ever highlighted after selection.
func TestSnapWithCurrentSession_SlashProjectHighlight(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	// Register renderer pane %42 before state is loaded.
	sub, unsub := mustSubscribe(t, h, "%42")
	defer unsub()

	// Tmux session "zitcha-backend" owns pane %42.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "zitcha-backend"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%42"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%42", Title: "shell"})

	// Project list uses slash-form canonical name.
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"zitcha/backend"}})

	// Wait for a snapshot where the project is non-absent (session is live).
	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "zitcha/backend")
		return p != nil && p.Status != "absent"
	})

	// CurrentSession must be the canonical slash-form name so the renderer
	// highlight works. Dash-form ("zitcha-backend") indicates the bug is present.
	if snap.CurrentSession != "zitcha/backend" {
		t.Errorf("CurrentSession = %q; want %q — slash/dash normalization missing in snapWithCurrentSession",
			snap.CurrentSession, "zitcha/backend")
	}
}

// TestSnapWithCurrentSession_NoDashSlashInName locks the no-regression case:
// a project whose name has no slash (e.g. "dotfiles") must still produce a
// matching CurrentSession when the pane belongs to the "dotfiles" session.
func TestSnapWithCurrentSession_NoDashSlashInName(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%10")
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "dotfiles"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@2"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@2", PaneID: "%10"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%10", Title: "shell"})
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"dotfiles"}})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "dotfiles")
		return p != nil && p.Status != "absent"
	})

	if snap.CurrentSession != "dotfiles" {
		t.Errorf("CurrentSession = %q; want %q — no-slash project name should pass through unchanged",
			snap.CurrentSession, "dotfiles")
	}
}

// TestSnapWithCurrentSession_WaitAckSuppressesRowMarker locks the
// suppression-after-visit contract: when a project's waiting state has
// been acknowledged (lastVisitTS >= WaitStartedTS), BOTH the agent chip
// AND the row-marker Status are demoted from "waiting" to "alive" in the
// per-subscriber snapshot. The pulse only re-fires when the agent
// transitions to waiting AGAIN, advancing WaitStartedTS past the
// recorded visit.
func TestSnapWithCurrentSession_WaitAckSuppressesRowMarker(t *testing.T) {
	base := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:           "zitcha/agora",
				Status:         "waiting",
				AgentClaude:    "waiting",
				WaitStartedTS:  100,
				LastActivityTS: 100,
			},
		},
	}

	st := newState()
	// User acknowledged the waiting state by visiting later than WaitStartedTS.
	st.lastVisitTS["zitcha-agora"] = 200 // >= 100

	// Subscriber lives in a DIFFERENT session (so case 1 does NOT match).
	sub := NewSubscriber("", "dotfiles")

	// now = 250 → age = 150 → only warn tier (60s) crossed → tierFloor = 60 →
	// threshold = 100 + 60 = 160. visit (200) ≥ 160 → ack'd.
	got := snapWithCurrentSession(base, st, sub, 250)

	p := findProject(got.Projects, "zitcha/agora")
	if p == nil {
		t.Fatal("project 'zitcha/agora' missing from snapshot")
	}
	if p.Status != "alive" {
		t.Errorf("Status = %q; want %q — wait-ack must demote the row marker",
			p.Status, "alive")
	}
	if p.AgentClaude != "" {
		t.Errorf("AgentClaude = %q; want empty — wait-ack must suppress the agent chip",
			p.AgentClaude)
	}
}

// TestSnapWithCurrentSession_UnackedWaitKeepsRowMarker locks the
// counterpart: when a project's waiting state has NOT been acknowledged
// (lastVisitTS < WaitStartedTS, or lastVisitTS unset), the row-marker
// Status stays "waiting" so the renderer's MarkerFor() emits the
// red-pulse glyph. Without this, the user has no visual to match the
// tier-crossing audio cue when the wait first escalates.
func TestSnapWithCurrentSession_UnackedWaitKeepsRowMarker(t *testing.T) {
	base := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:           "zitcha/agora",
				Status:         "waiting",
				AgentClaude:    "waiting",
				WaitStartedTS:  300, // newer than any lastVisitTS below
				LastActivityTS: 300,
			},
		},
	}

	st := newState()
	// User last visited BEFORE this wait cycle started, OR never.
	st.lastVisitTS["zitcha-agora"] = 100 // < 300

	sub := NewSubscriber("", "dotfiles")

	// now = 350 → age = 50 → no tier crossed → tierFloor = 0 →
	// threshold = 300. visit (100) < 300 → NOT ack'd.
	got := snapWithCurrentSession(base, st, sub, 350)

	p := findProject(got.Projects, "zitcha/agora")
	if p == nil {
		t.Fatal("project 'zitcha/agora' missing from snapshot")
	}
	if p.Status != "waiting" {
		t.Errorf("Status = %q; want %q — un-acknowledged wait MUST keep the pulse marker",
			p.Status, "waiting")
	}
	if p.AgentClaude != "waiting" {
		t.Errorf("AgentClaude = %q; want %q — un-acknowledged wait MUST keep the agent chip",
			p.AgentClaude, "waiting")
	}
}

// TestSnapWithCurrentSession_StaleAckExpiresAtTierCrossing locks 260511-c9s:
// a visit that ack'd the wait at an earlier tier MUST stop counting as
// acknowledgment once the wait age has crossed a higher tier. The user's
// framing: "I have visited, but not as recently as attention was demanded."
//
// Scenario: agent starts waiting at T=0. User visits at T=120s (would have
// ack'd warn-tier crossing at T=60s). Agent keeps waiting. At T=350s the
// urgent tier (300s) has crossed — visit (120) < threshold (0 + 300 = 300).
// Ack expires. Row marker re-pulses.
func TestSnapWithCurrentSession_StaleAckExpiresAtTierCrossing(t *testing.T) {
	base := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:           "zitcha/agora",
				Status:         "waiting",
				AgentClaude:    "waiting",
				WaitStartedTS:  0,
				LastActivityTS: 0,
			},
		},
	}

	st := newState()
	st.lastVisitTS["zitcha-agora"] = 120 // visited during the warn tier window

	sub := NewSubscriber("", "dotfiles")

	// now = 350 → age = 350 → urgent (300s) crossed; warn (60s) crossed →
	// tierFloor = 300 → threshold = 0 + 300 = 300. visit (120) < 300 → NOT ack'd.
	got := snapWithCurrentSession(base, st, sub, 350)

	p := findProject(got.Projects, "zitcha/agora")
	if p == nil {
		t.Fatal("project 'zitcha/agora' missing from snapshot")
	}
	if p.Status != "waiting" {
		t.Errorf("Status = %q; want %q — stale ack must expire at next tier crossing (260511-c9s)",
			p.Status, "waiting")
	}
	if p.AgentClaude != "waiting" {
		t.Errorf("AgentClaude = %q; want %q — stale ack must expire for agent chip too",
			p.AgentClaude, "waiting")
	}
}
