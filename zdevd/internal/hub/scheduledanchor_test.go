// internal/hub/scheduledanchor_test.go
//
// The scheduled-anchor tier (design amendment, docs/design/command-centre.md
// — "The scheduled anchor and the push surface"): the Kind convention,
// eligibility, the tier's override semantics (never explicit, does auto),
// the pinned "once explicitly overridden, that block never re-anchors"
// rule, back-to-back blocks, the airlock gate, non-persistence, and the
// dwell/instant-anchor suppression while a scheduled anchor is active.
// Reuses notify_test.go's stateWithProject/makeRecorder and
// boundary_test.go's fullNotifRecorder, same package, same style as
// autoanchor_test.go.
package hub

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// --- Kind convention: isSchedulable / schedulableProject ---

func TestIsSchedulable(t *testing.T) {
	cases := []struct {
		name string
		c    proto.Commitment
		want bool
	}{
		{"task with project mapping", proto.Commitment{Kind: "task:marketplace/pay-ops"}, true},
		{"plain task, no mapping", proto.Commitment{Kind: "task"}, false},
		{"bare prefix, empty mapping", proto.Commitment{Kind: "task:"}, false},
		{"meeting", proto.Commitment{Kind: "meeting"}, false},
		{"empty kind", proto.Commitment{Kind: ""}, false},
		{"allday", proto.Commitment{Kind: "allday"}, false},
	}
	for _, c := range cases {
		if got := isSchedulable(c.c); got != c.want {
			t.Errorf("%s: isSchedulable(%+v) = %v, want %v", c.name, c.c, got, c.want)
		}
	}
}

func TestSchedulableProject(t *testing.T) {
	if got := schedulableProject("task:marketplace/pay-ops"); got != "marketplace/pay-ops" {
		t.Errorf("schedulableProject = %q, want %q", got, "marketplace/pay-ops")
	}
}

// --- isScheduledAnchor / isExplicitAnchor ---

func TestIsScheduledAnchor(t *testing.T) {
	cases := []struct {
		name string
		a    *proto.Anchor
		want bool
	}{
		{"nil", nil, false},
		{"scheduled convention", &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops"}, true},
		{"explicit pick", &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend"}, false},
		{"auto convention", &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend"}, true /* isAutoAnchor, not isScheduledAnchor — checked separately below */},
		{"listless explicit", &proto.Anchor{Title: "call the dentist"}, false},
	}
	for _, c := range cases {
		if c.name == "auto convention" {
			// isScheduledAnchor must be FALSE for an auto anchor (distinct
			// suffixes never overlap) — the table's "want" column above
			// documents isAutoAnchor's answer, not this function's; assert
			// this function's answer directly instead of via the shared table.
			if isScheduledAnchor(c.a) {
				t.Errorf("isScheduledAnchor(auto anchor) = true, want false")
			}
			continue
		}
		if got := isScheduledAnchor(c.a); got != c.want {
			t.Errorf("%s: isScheduledAnchor(%+v) = %v, want %v", c.name, c.a, got, c.want)
		}
	}
}

func TestIsExplicitAnchor(t *testing.T) {
	cases := []struct {
		name string
		a    *proto.Anchor
		want bool
	}{
		{"nil", nil, false},
		{"auto", &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend"}, false},
		{"scheduled", &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops"}, false},
		{"explicit with project", &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend"}, true},
		{"explicit listless", &proto.Anchor{Title: "call the dentist"}, true},
	}
	for _, c := range cases {
		if got := isExplicitAnchor(c.a); got != c.want {
			t.Errorf("%s: isExplicitAnchor(%+v) = %v, want %v", c.name, c.a, got, c.want)
		}
	}
}

// --- activeSchedulableCommitment ---

func TestActiveSchedulableCommitment(t *testing.T) {
	commitments := []proto.Commitment{
		{ID: "meeting1", Kind: "meeting", At: 1000, Until: 2000},
		{ID: "task1", Kind: "task:marketplace/pay-ops", At: 2000, Until: 3000},
		{ID: "plaintask", Kind: "task", At: 3000, Until: 4000},
	}
	cases := []struct {
		name   string
		now    int64
		wantID string
		wantOK bool
	}{
		{"inside a meeting — not eligible", 1500, "", false},
		{"inside the eligible task block", 2500, "task1", true},
		{"inside a plain-task block — not eligible", 3500, "", false},
		{"between everything", 500, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := activeSchedulableCommitment(commitments, c.now)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got.ID != c.wantID {
				t.Errorf("got.ID = %q, want %q", got.ID, c.wantID)
			}
		})
	}
}

// --- checkScheduledAnchor: arming, SinceTS, tier overrides ---

func schedulableCommitment(id string, at, until int64, project string) proto.Commitment {
	return proto.Commitment{ID: id, Title: id + " title", Source: "plan", At: at, Until: until, Kind: "task:" + project}
}

func TestCheckScheduledAnchor_ArmsWhenUnanchoredAndEligible(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}

	if !checkScheduledAnchor(1500, s, commitments) {
		t.Fatal("checkScheduledAnchor = false, want true (eligible commitment covers now)")
	}
	if s.anchor == nil {
		t.Fatal("anchor is nil after arming")
	}
	if want := "t1 title" + scheduledAnchorSuffix; s.anchor.Title != want {
		t.Errorf("Title = %q, want %q", s.anchor.Title, want)
	}
	if s.anchor.Project != "marketplace/pay-ops" {
		t.Errorf("Project = %q, want %q", s.anchor.Project, "marketplace/pay-ops")
	}
	// SinceTS is the BLOCK's start (1000), not `now` (1500) — elapsed must
	// read as time-into-block even when arming happens mid-block.
	if s.anchor.SinceTS != 1000 {
		t.Errorf("SinceTS = %d, want 1000 (block start, not now)", s.anchor.SinceTS)
	}
	if !isScheduledAnchor(s.anchor) {
		t.Error("isScheduledAnchor = false for a just-armed scheduled anchor")
	}
	if s.scheduledAnchorCommitmentID != "t1" {
		t.Errorf("scheduledAnchorCommitmentID = %q, want %q", s.scheduledAnchorCommitmentID, "t1")
	}
	if s.scheduledAnchorUntil != 2000 {
		t.Errorf("scheduledAnchorUntil = %d, want 2000", s.scheduledAnchorUntil)
	}
}

func TestCheckScheduledAnchor_PlainTaskNeverEligible(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{{ID: "p1", Title: "plain", At: 1000, Until: 2000, Kind: "task"}}
	if checkScheduledAnchor(1500, s, commitments) {
		t.Error("checkScheduledAnchor armed on a plain \"task\" kind (no project mapping), want false")
	}
	if s.anchor != nil {
		t.Errorf("anchor set from an ineligible commitment: %+v", s.anchor)
	}
}

func TestCheckScheduledAnchor_MeetingNeverEligible(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{{ID: "m1", Title: "standup", At: 1000, Until: 2000, Kind: "meeting"}}
	if checkScheduledAnchor(1500, s, commitments) {
		t.Error("checkScheduledAnchor armed on a \"meeting\" kind, want false")
	}
	if s.anchor != nil {
		t.Errorf("anchor set from a meeting: %+v", s.anchor)
	}
}

func TestCheckScheduledAnchor_OverridesAutoSilently(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 500}
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}

	if !checkScheduledAnchor(1500, s, commitments) {
		t.Fatal("checkScheduledAnchor did not override the auto anchor, want true")
	}
	if !isScheduledAnchor(s.anchor) {
		t.Errorf("anchor after override = %+v, want a scheduled anchor", s.anchor)
	}
	if s.anchor.Project != "marketplace/pay-ops" {
		t.Errorf("Project after override = %q, want the scheduled block's project", s.anchor.Project)
	}
}

func TestCheckScheduledAnchor_NeverOverridesExplicit(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend", SinceTS: 500}
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}

	if checkScheduledAnchor(1500, s, commitments) {
		t.Error("checkScheduledAnchor overrode an EXPLICIT anchor, want never")
	}
	if s.anchor.Title != "IMP-97 validate deploy" {
		t.Errorf("explicit anchor mutated: %+v", s.anchor)
	}
}

func TestCheckScheduledAnchor_DoesNotRederiveSameBlock(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}

	if !checkScheduledAnchor(1200, s, commitments) {
		t.Fatal("first arm failed")
	}
	firstSinceTS := s.anchor.SinceTS

	// A later pass, still inside the same block, with the SAME scheduled
	// anchor already active — must be a no-op (no redundant applyEvent /
	// SinceTS reset every heartbeat).
	if checkScheduledAnchor(1800, s, commitments) {
		t.Error("checkScheduledAnchor re-armed inside its own already-active block, want no-op")
	}
	if s.anchor.SinceTS != firstSinceTS {
		t.Errorf("SinceTS drifted on a redundant pass: got %d, want unchanged %d", s.anchor.SinceTS, firstSinceTS)
	}
}

func TestCheckScheduledAnchor_OverriddenBlockNeverRegrabs(t *testing.T) {
	s := newState()
	s.scheduledOverriddenBlocks = map[string]struct{}{"t1": {}}
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}

	if checkScheduledAnchor(1500, s, commitments) {
		t.Error("checkScheduledAnchor re-grabbed an explicitly-overridden block, want refused")
	}
	if s.anchor != nil {
		t.Errorf("anchor set for a blocked commitment ID: %+v", s.anchor)
	}
}

// --- pinned semantics: explicit-override-never-regrabbed, via the Hub's
// real anchorRequests "set"/"clear" paths (hub.go) ---

func TestMarkScheduledOverridden_SetPathBlocksReGrab(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}
	if !checkScheduledAnchor(1200, s, commitments) {
		t.Fatal("setup: scheduled anchor did not arm")
	}

	// Simulate hub.go's anchorRequests "set" branch: capture prev, apply the
	// explicit AnchorSet, then mark the override.
	prev := s.anchor
	applyEvent(s, tmuxctl.AnchorSet{Title: "IMP-97 something else", Project: "example/other", NowNanos: 1300e9}, nil)
	markScheduledOverridden(s, prev)

	if _, blocked := s.scheduledOverriddenBlocks["t1"]; !blocked {
		t.Fatal("scheduledOverriddenBlocks does not contain the overridden block's ID")
	}

	// The operator later clears the explicit override — s.anchor goes nil
	// — but `now` is STILL inside the block's window. The block must never
	// re-anchor.
	applyEvent(s, tmuxctl.AnchorClear{}, nil)
	if checkScheduledAnchor(1900, s, commitments) {
		t.Error("checkScheduledAnchor re-grabbed a block that was explicitly overridden earlier in its own window, want refused permanently")
	}
	if s.anchor != nil {
		t.Errorf("anchor set after the block was permanently overridden: %+v", s.anchor)
	}
}

func TestMarkScheduledOverridden_ClearPathAlsoBlocksReGrab(t *testing.T) {
	// An explicit CLEAR (not a replacing "set") of a scheduled anchor must
	// ALSO permanently block that block — otherwise the very next
	// publishPass would silently re-grab the anchor right back, directly
	// contradicting the operator's deliberate release.
	s := newState()
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}
	if !checkScheduledAnchor(1200, s, commitments) {
		t.Fatal("setup: scheduled anchor did not arm")
	}

	prev := s.anchor
	applyEvent(s, tmuxctl.AnchorClear{}, nil)
	markScheduledOverridden(s, prev)

	if checkScheduledAnchor(1500, s, commitments) {
		t.Error("checkScheduledAnchor re-grabbed a block the operator explicitly cleared, want refused")
	}
}

func TestMarkScheduledOverridden_NoOpForNonScheduledPrev(t *testing.T) {
	s := newState()
	explicit := &proto.Anchor{Title: "IMP-97", Project: "example/backend"}
	markScheduledOverridden(s, explicit)
	if len(s.scheduledOverriddenBlocks) != 0 {
		t.Errorf("scheduledOverriddenBlocks = %+v, want empty (prev wasn't scheduled)", s.scheduledOverriddenBlocks)
	}
	markScheduledOverridden(s, nil)
	if len(s.scheduledOverriddenBlocks) != 0 {
		t.Errorf("scheduledOverriddenBlocks = %+v after a nil prev, want still empty", s.scheduledOverriddenBlocks)
	}
}

// --- checkBoundary: block-end fires exactly once, incl. back-to-back ---

func TestCheckBoundary_ScheduledBlockEndFires(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}
	if !checkScheduledAnchor(1200, s, commitments) {
		t.Fatal("setup: scheduled anchor did not arm")
	}
	rec := &fullNotifRecorder{}

	if checkBoundary(1999, s, rec.fire) {
		t.Fatal("boundary fired before Until, want false")
	}
	if !checkBoundary(2000, s, rec.fire) {
		t.Fatal("boundary did not fire at exactly Until, want true")
	}
	if s.anchor != nil {
		t.Error("anchor not cleared at block end")
	}
	recs := rec.snapshot()
	if len(recs) != 1 || recs[0].Kind != "boundary" {
		t.Fatalf("got %+v, want exactly 1 boundary notification", recs)
	}
}

func TestCheckBoundary_ScheduledBlockEnd_SkippedIfAlreadyOverridden(t *testing.T) {
	// An explicit override already replaced the scheduled anchor via a
	// DIFFERENT code path (hub.go's anchorRequests "set"), so by the time
	// checkBoundary runs the anchor is no longer scheduled — the block-end
	// check must not fire a second, redundant boundary for it.
	s := newState()
	commitments := []proto.Commitment{schedulableCommitment("t1", 1000, 2000, "marketplace/pay-ops")}
	if !checkScheduledAnchor(1200, s, commitments) {
		t.Fatal("setup: scheduled anchor did not arm")
	}
	// Explicit override replaces it (scheduledAnchorUntil is still 2000 —
	// stale, but harmless, per the field's own doc comment).
	applyEvent(s, tmuxctl.AnchorSet{Title: "IMP-97 something else", Project: "example/other", NowNanos: 1300e9}, nil)

	rec := &fullNotifRecorder{}
	if checkBoundary(2000, s, rec.fire) {
		t.Error("checkBoundary fired the scheduled block-end boundary for an anchor that is no longer scheduled, want false")
	}
	if s.anchor == nil || s.anchor.Title != "IMP-97 something else" {
		t.Errorf("explicit anchor disturbed by the stale scheduledAnchorUntil: %+v", s.anchor)
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("got %+v, want no notifications", recs)
	}
}

// Back-to-back blocks: block A's end and block B's start coincide. The
// SAME publishPass must fire exactly ONE boundary notification (for A) and
// may immediately anchor to B — this test drives checkBoundary then
// checkScheduledAnchor in that exact order, mirroring hub.go's publishPass
// sequence.
func TestBackToBackBlocks_OneBoundaryThenImmediateNextAnchor(t *testing.T) {
	s := newState()
	commitments := []proto.Commitment{
		schedulableCommitment("A", 1000, 2000, "marketplace/pay-ops"),
		schedulableCommitment("B", 2000, 3000, "marketplace/reporting"),
	}
	if !checkScheduledAnchor(1200, s, commitments) {
		t.Fatal("setup: block A did not arm")
	}
	if s.scheduledAnchorCommitmentID != "A" {
		t.Fatalf("setup: armed the wrong block: %+v", s.anchor)
	}

	rec := &fullNotifRecorder{}
	now := int64(2000) // exactly A's Until AND B's At

	boundaryFired := checkBoundary(now, s, rec.fire)
	if !boundaryFired {
		t.Fatal("boundary did not fire for block A's end")
	}
	if s.anchor != nil {
		t.Fatal("anchor not cleared by A's boundary")
	}

	scheduledFired := checkScheduledAnchor(now, s, commitments)
	if !scheduledFired {
		t.Fatal("did not immediately anchor to block B in the same pass")
	}
	if s.scheduledAnchorCommitmentID != "B" {
		t.Errorf("scheduledAnchorCommitmentID = %q, want %q (block B)", s.scheduledAnchorCommitmentID, "B")
	}
	if s.anchor.Project != "marketplace/reporting" {
		t.Errorf("Project = %q, want block B's project", s.anchor.Project)
	}

	recs := rec.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d notifications for the back-to-back transition, want exactly 1 (for A's end only)", len(recs))
	}
}

// --- airlock: scheduled anchor lets notifications speak; explicit holds ---

func TestTierCheck_Airlock_ScheduledAnchorLetsNotificationsSpeak(t *testing.T) {
	s := stateWithProject("foreign-project", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(160, s, fire) // age = 60 → 1m tier on a DIFFERENT project

	if len(*recs) != 1 {
		t.Fatalf("expected the notification to SPEAK while scheduled-anchored (not held), got %d: %v", len(*recs), *recs)
	}
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty — scheduled anchors don't airlock foreign waits", s.heldItems)
	}
}

func TestTierCheck_Airlock_ExplicitAnchorStillHolds(t *testing.T) {
	// Contrast case, pinning that ONLY explicit still engages the airlock
	// after the design amendment.
	s := stateWithProject("foreign-project", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(160, s, fire)

	if len(*recs) != 0 {
		t.Fatalf("expected no notification while EXPLICITLY anchored on a different project, got %v", *recs)
	}
	if len(s.heldItems) != 1 {
		t.Errorf("heldItems = %+v, want exactly 1 captured item", s.heldItems)
	}
}

// --- persistence: scheduled anchors never persisted (mirrors
// TestSaveState_AutoAnchorNeverPersisted) ---

func TestSaveState_ScheduledAnchorNeverPersisted(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops", SinceTS: 1000}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps.Anchor != nil {
		t.Errorf("ps.Anchor = %+v, want nil — scheduled anchors must never reach disk", ps.Anchor)
	}
}

// --- dwell/instant-anchor suppressed while a scheduled anchor is active ---

func TestCheckAutoAnchorArm_NeverOverridesScheduledAnchor(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 600
	s.anchor = &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops", SinceTS: 0}
	updateDwell(s, 0)

	if checkAutoAnchorArm(1_000_000, s) {
		t.Error("checkAutoAnchorArm armed over a scheduled anchor, want false")
	}
	if !isScheduledAnchor(s.anchor) {
		t.Errorf("scheduled anchor replaced: %+v", s.anchor)
	}
}

func TestTryInstantAnchor_NeverOverridesScheduledAnchor(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 600
	s.anchor = &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops", SinceTS: 0}

	handleWorkingSignal(s, tmuxctl.NotifSeen{Session: "example-backend", Timestamp: 1000, Kind: proto.WaitKindWorking, Src: "prompt"})

	if !isScheduledAnchor(s.anchor) {
		t.Errorf("scheduled anchor replaced by an instant-anchor attempt: %+v", s.anchor)
	}
}

// --- end-to-end: a live Hub picks up a pushed, anchor-eligible commitment
// on its own publishPass heartbeat, proving hub.go's wiring (not just the
// pure functions above) — mirrors TestAutoAnchor_EndToEnd_ArmsThenAwayBoundary's
// style for the SAME kind of "real wall clock, poll don't sleep" test.
func TestScheduledAnchor_EndToEnd_ArmsFromPushedCommitment(t *testing.T) {
	h := NewHub(Config{Debounce: testDebounce})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	now := time.Now().Unix()
	if err := h.SubmitSchedulePush(context.Background(), "plan", []proto.Commitment{
		{ID: "t1", Title: "IMP-97 stand-up", At: now - 10, Until: now + 30, Kind: "task:marketplace/pay-ops"},
	}); err != nil {
		t.Fatalf("SubmitSchedulePush: %v", err)
	}

	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	deadline := time.Now().Add(5 * time.Second)
	var armed *proto.Anchor
	for time.Now().Before(deadline) && armed == nil {
		if _, _, err := h.SubmitCursor(context.Background(), 0); err != nil {
			t.Fatalf("SubmitCursor: %v", err)
		}
		select {
		case snap := <-snaps:
			if snap.Anchor != nil {
				armed = snap.Anchor
			}
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if armed == nil {
		t.Fatal("scheduled anchor never armed within 5s")
	}
	if want := "IMP-97 stand-up" + scheduledAnchorSuffix; armed.Title != want {
		t.Errorf("Title = %q, want %q", armed.Title, want)
	}
	if armed.Project != "marketplace/pay-ops" {
		t.Errorf("Project = %q, want %q", armed.Project, "marketplace/pay-ops")
	}
}

// The invariants review's reproduction (2026-08-04): a block longer than
// the idle-expiry window must NOT flood boundaries — expiry is exempt for
// scheduled anchors because the block's Until IS the expiry. Ten passes
// deep inside an over-long block: zero boundary notifications.
func TestScheduledAnchorExemptFromIdleExpiry(t *testing.T) {
	s := newState()
	s.anchorExpirySec = 90 * 60
	s.commitments = map[string][]proto.Commitment{"plan": {{
		ID: "blk-long", Source: "plan", Title: "deep block",
		Kind: "task:marketplace/pay-ops",
		At:   1000, Until: 1000 + 4*3600, // 4h block
	}}}
	rec := &fullNotifRecorder{}

	merged := mergedCommitments(s.commitments)
	// Arm at block start, then run passes from 2h in (past the 90m expiry).
	checkScheduledAnchor(1000, s, merged)
	if s.anchor == nil {
		t.Fatal("block must arm")
	}
	for now := int64(1000 + 2*3600); now < 1000+2*3600+10; now++ {
		checkBoundary(now, s, rec.fire)
		checkScheduledAnchor(now, s, merged)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("scheduled anchor inside its window fired %d boundaries, want 0", got)
	}
	if s.anchor == nil || !isScheduledAnchor(s.anchor) {
		t.Fatal("anchor must still be the scheduled block")
	}

	// The block's own end still bounds, exactly once.
	end := int64(1000 + 4*3600)
	checkBoundary(end, s, rec.fire)
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("block end must fire exactly one boundary, got %d", got)
	}
}

// A finish on a scheduled anchor CONSUMES its block: one boundary, and the
// same pass (or any later one inside the window) must not re-grab it.
func TestScheduledAnchorFinishConsumesBlock(t *testing.T) {
	s := stateWithProject("marketplace-pay-ops", projectData{Attention: proto.AttWorking})
	s.commitments = map[string][]proto.Commitment{"plan": {{
		ID: "blk-1", Source: "plan", Title: "IMP-97",
		Kind: "task:marketplace/pay-ops",
		At:   1000, Until: 1000 + 3600,
	}}}
	rec := &fullNotifRecorder{}
	merged := mergedCommitments(s.commitments)
	checkScheduledAnchor(1000, s, merged)
	if s.anchor == nil {
		t.Fatal("block must arm")
	}
	// checkBoundary observes the project live first (arms the finish edge),
	// then the work finishes.
	checkBoundary(1100, s, rec.fire)
	pd := s.projectData["marketplace-pay-ops"]
	pd.Attention = proto.AttFinished
	s.projectData["marketplace-pay-ops"] = pd

	checkBoundary(1200, s, rec.fire)
	checkScheduledAnchor(1200, s, merged) // same-pass re-grab attempt
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("finish must fire exactly one boundary, got %d", got)
	}
	if s.anchor != nil {
		t.Fatalf("consumed block must not re-grab, got %+v", s.anchor)
	}
}

// Scheduled bookkeeping dies with the anchor: after any boundary, an
// EXPLICIT anchor with a pathological "(scheduled)"-suffixed title must not
// be judged against a stale block's Until.
func TestStaleScheduledBookkeepingCannotClearExplicitAnchor(t *testing.T) {
	s := newState()
	s.commitments = map[string][]proto.Commitment{"plan": {{
		ID: "blk-old", Source: "plan", Title: "old block",
		Kind: "task:marketplace/pay-ops",
		At:   1000, Until: 2000,
	}}}
	rec := &fullNotifRecorder{}
	checkScheduledAnchor(1000, s, mergedCommitments(s.commitments))
	checkBoundary(2000, s, rec.fire) // block ends normally
	if s.scheduledAnchorUntil != 0 || s.scheduledAnchorCommitmentID != "" {
		t.Fatal("bookkeeping must be zeroed by the boundary")
	}

	// Operator explicitly anchors with a pathological title.
	applyEvent(s, tmuxctl.AnchorSet{Title: "review x (scheduled)", NowNanos: 5000e9}, nil)
	before := len(rec.snapshot())
	checkBoundary(6000, s, rec.fire)
	if s.anchor == nil {
		t.Fatal("explicit anchor with a pathological title must survive")
	}
	if got := len(rec.snapshot()); got != before {
		t.Fatal("no spurious boundary may fire from stale bookkeeping")
	}
}
