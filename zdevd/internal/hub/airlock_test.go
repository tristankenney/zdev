// internal/hub/airlock_test.go
//
// Phase 3A of the focus loop (docs/design/command-centre.md — "the
// airlock" / "the pierce list"): tierCheck's anchored-gating behavior.
// Reuses notify_test.go's makeRecorder/stateWithProject helpers (same
// package) so these tests read like siblings of the existing tier-ladder
// suite, not a parallel dialect.
package hub

import (
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestTierCheck_Airlock_ForeignProjectHeld: anchored on one project, a tier
// crossing on a DIFFERENT project is captured into the held set instead of
// fired — the pierce list's one explicit behaviour change ("waits on other
// projects... speak aloud today").
func TestTierCheck_Airlock_ForeignProjectHeld(t *testing.T) {
	s := stateWithProject("foreign-project", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "IMP-97", Project: "anchor/project", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(160, s, fire) // age = 60 → 1m tier

	if len(*recs) != 0 {
		t.Fatalf("expected NO notification while anchored on a different project, got %v", *recs)
	}
	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want exactly 1 captured item", s.heldItems)
	}
	item := s.heldItems[0]
	if item.ID != "wait-foreign-project" {
		t.Errorf("ID = %q, want %q", item.ID, "wait-foreign-project")
	}
	if item.Kind != "wait" {
		t.Errorf("Kind = %q, want %q", item.Kind, "wait")
	}
	if item.Title != "waiting 1m" {
		t.Errorf("Title = %q, want %q", item.Title, "waiting 1m")
	}
	if item.Project != "foreign-project" {
		t.Errorf("Project = %q, want %q", item.Project, "foreign-project")
	}
	if item.SinceTS != 160 {
		t.Errorf("SinceTS = %d, want 160 (capture time)", item.SinceTS)
	}
	// Tier bit still marked even though suppressed — un-anchoring later must
	// not unleash a burst of stale tier notifications.
	pd := s.projectData["foreign-project"]
	if pd.WaitNotifiedTiers&0b001 == 0 {
		t.Errorf("WaitNotifiedTiers = %08b, want bit0 marked despite suppression", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_Airlock_EscalationUpdatesTitlePreservesSinceTS confirms a
// re-escalating held wait updates its Title but keeps the ORIGINAL SinceTS
// — the held set's age must reflect when the airlock first caught the
// wait, not when it last escalated.
func TestTierCheck_Airlock_EscalationUpdatesTitlePreservesSinceTS(t *testing.T) {
	s := stateWithProject("foreign-project", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "IMP-97", Project: "anchor/project", SinceTS: 0}
	_, fire := makeRecorder()

	tierCheck(160, s, fire) // age 60 → 1m tier, captured at now=160
	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v after first capture, want 1", s.heldItems)
	}
	firstSince := s.heldItems[0].SinceTS
	if s.heldItems[0].Title != "waiting 1m" {
		t.Fatalf("Title = %q after first capture, want %q", s.heldItems[0].Title, "waiting 1m")
	}

	tierCheck(400, s, fire) // age 300 → 5m tier, captured at now=400

	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v after escalation, want still exactly 1 (same ID updated, not duplicated)", s.heldItems)
	}
	if s.heldItems[0].ID != "wait-foreign-project" {
		t.Errorf("ID = %q, want stable %q", s.heldItems[0].ID, "wait-foreign-project")
	}
	if s.heldItems[0].Title != "still waiting (5m)" {
		t.Errorf("Title = %q, want escalated %q", s.heldItems[0].Title, "still waiting (5m)")
	}
	if s.heldItems[0].SinceTS != firstSince {
		t.Errorf("SinceTS = %d, want preserved original %d", s.heldItems[0].SinceTS, firstSince)
	}
}

// TestTierCheck_Airlock_OwnProjectFires: a tier crossing on the ANCHOR's
// own project fires exactly as it would unanchored — pierce list item (b).
func TestTierCheck_Airlock_OwnProjectFires(t *testing.T) {
	s := stateWithProject("example-agora", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "IMP-97", Project: "example/agora", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(160, s, fire) // age = 60

	if len(*recs) != 1 {
		t.Fatalf("expected 1 notification for the anchor's own project, got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Project != "example-agora" {
		t.Errorf("Project = %q, want %q", r.Project, "example-agora")
	}
	if r.Msg != "waiting 1m" {
		t.Errorf("Msg = %q, want %q", r.Msg, "waiting 1m")
	}
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty — the own-project crossing fired, it was not captured", s.heldItems)
	}
}

// TestTierCheck_Airlock_ListlessAnchorHoldsEverything: an anchor with no
// Project (listless work — a phone call) has no "own project", so every
// tier crossing anywhere is airlocked.
func TestTierCheck_Airlock_ListlessAnchorHoldsEverything(t *testing.T) {
	s := stateWithProject("some-project", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "phone call", Project: "", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(160, s, fire)

	if len(*recs) != 0 {
		t.Errorf("expected no notification under a listless anchor, got %v", *recs)
	}
	if len(s.heldItems) != 1 {
		t.Errorf("heldItems = %+v, want the crossing captured", s.heldItems)
	}
}

// TestTierCheck_Airlock_DeathFiresRegardless: a death fires exactly as
// today even while anchored on an unrelated project — pierce list item
// (a). Deaths never go through the held-capture path at all.
func TestTierCheck_Airlock_DeathFiresRegardless(t *testing.T) {
	s := stateWithProject("dead-project", projectData{DeadSinceTS: 100, DeadReason: "exit 1"})
	s.anchor = &proto.Anchor{Title: "IMP-97", Project: "anchor/project", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(200, s, fire)

	if len(*recs) != 1 {
		t.Fatalf("expected the death to fire regardless of anchor, got %d records: %v", len(*recs), *recs)
	}
	if (*recs)[0].Project != "dead-project" {
		t.Errorf("Project = %q, want %q", (*recs)[0].Project, "dead-project")
	}
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty — deaths are never held", s.heldItems)
	}
}

// TestTierCheck_Unanchored_Byte4Byte confirms tierCheck's behavior is
// completely unaffected when s.anchor is nil — the airlock gate must never
// engage for the (overwhelmingly common) unanchored case. This mirrors
// TestTierCheck_A (notify_test.go) but asserts explicitly against a state
// that has been through the same construction path as the airlock tests
// above, just without an anchor.
func TestTierCheck_Unanchored_Byte4Byte(t *testing.T) {
	s := stateWithProject("example-agora", projectData{WaitStartedTS: 100})
	// s.anchor is nil (newState()'s zero value) — no gating should apply.
	recs, fire := makeRecorder()

	tierCheck(160, s, fire)

	if len(*recs) != 1 {
		t.Fatalf("expected 1 notification (unanchored, unaffected by airlock), got %d: %v", len(*recs), *recs)
	}
	if (*recs)[0].Msg != "waiting 1m" {
		t.Errorf("Msg = %q, want %q", (*recs)[0].Msg, "waiting 1m")
	}
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty — unanchored never captures", s.heldItems)
	}
}
