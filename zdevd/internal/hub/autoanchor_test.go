// internal/hub/autoanchor_test.go
//
// Phase 3D of the focus loop (docs/design/command-centre.md — "the dwell
// auto-anchor"): pure-function tests for the dwell clock, the arming and
// away-boundary derivations, plus Hub-level tests for the durability
// (persistence) and end-to-end wiring contracts. Reuses notify_test.go's
// stateWithProject/makeRecorder and boundary_test.go's fullNotifRecorder —
// same helpers, same package, so these read like siblings of the existing
// anchor/boundary/airlock suites rather than a parallel dialect.
package hub

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// --- isAutoAnchor ---

func TestIsAutoAnchor(t *testing.T) {
	cases := []struct {
		name string
		a    *proto.Anchor
		want bool
	}{
		{"nil", nil, false},
		{"explicit pick", &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend"}, false},
		{"auto convention", &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend"}, true},
		{"auto-shaped title but no project (impossible in practice, but must not misclassify)", &proto.Anchor{Title: " (auto)", Project: ""}, false},
		{"title names a DIFFERENT project than Project", &proto.Anchor{Title: "other/project (auto)", Project: "example/backend"}, false},
		{"listless explicit anchor", &proto.Anchor{Title: "call the dentist", Project: ""}, false},
	}
	for _, c := range cases {
		if got := isAutoAnchor(c.a); got != c.want {
			t.Errorf("%s: isAutoAnchor(%+v) = %v, want %v", c.name, c.a, got, c.want)
		}
	}
}

// --- soleAttendedManagedProject ---

func TestSoleAttendedManagedProject(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend", "example/frontend"}

	if got := soleAttendedManagedProject(s); got != "" {
		t.Errorf("no clients attended: got %q, want empty", got)
	}

	s.clientSessions["c1"] = "example-backend"
	if got := soleAttendedManagedProject(s); got != "example/backend" {
		t.Errorf("got %q, want example/backend", got)
	}

	// A second client attending a DIFFERENT session — disagreement, no
	// sole attendee.
	s.clientSessions["c2"] = "example-frontend"
	if got := soleAttendedManagedProject(s); got != "" {
		t.Errorf("clients disagree: got %q, want empty", got)
	}

	// A second client attending the SAME session — still unanimous.
	delete(s.clientSessions, "c2")
	s.clientSessions["c2"] = "example-backend"
	if got := soleAttendedManagedProject(s); got != "example/backend" {
		t.Errorf("both clients agree: got %q, want example/backend", got)
	}

	// Attended session isn't in the managed project list at all.
	s.clientSessions = map[string]string{"c1": "some-unmanaged-session"}
	if got := soleAttendedManagedProject(s); got != "" {
		t.Errorf("unmanaged session: got %q, want empty", got)
	}
}

// --- updateDwell: hop-interrupted dwell restarts ---

func TestUpdateDwell_ContinuedAttendanceLeavesClockUntouched(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"

	updateDwell(s, 0)
	if s.dwellProject != "example/backend" || s.dwellSinceTS != 0 {
		t.Fatalf("initial dwell = %q/%d, want example/backend/0", s.dwellProject, s.dwellSinceTS)
	}
	updateDwell(s, 500) // same project, later pass — clock must not move
	if s.dwellSinceTS != 0 {
		t.Errorf("dwellSinceTS = %d after continued attendance, want unchanged 0", s.dwellSinceTS)
	}
}

func TestUpdateDwell_HopInterruptedDwellRestarts(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend", "example/frontend"}
	s.clientSessions["c1"] = "example-backend"
	updateDwell(s, 0)
	updateDwell(s, 500) // still dwelling on backend

	// A brief hop to a different project.
	s.clientSessions["c1"] = "example-frontend"
	updateDwell(s, 550)
	if s.dwellProject != "example/frontend" || s.dwellSinceTS != 550 {
		t.Fatalf("after hop: dwellProject=%q dwellSinceTS=%d, want example/frontend/550", s.dwellProject, s.dwellSinceTS)
	}

	// Hop back to the original project — the clock RESTARTS; the pre-hop
	// accumulated dwell time (0..500) is not carried forward.
	s.clientSessions["c1"] = "example-backend"
	updateDwell(s, 560)
	if s.dwellProject != "example/backend" || s.dwellSinceTS != 560 {
		t.Fatalf("after hop back: dwellProject=%q dwellSinceTS=%d, want example/backend/560 (restarted)", s.dwellProject, s.dwellSinceTS)
	}
}

func TestUpdateDwell_DetachRestartsAtZero(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	updateDwell(s, 100)

	delete(s.clientSessions, "c1")
	updateDwell(s, 150)
	if s.dwellProject != "" || s.dwellSinceTS != 0 {
		t.Errorf("after full detach: dwellProject=%q dwellSinceTS=%d, want empty/0", s.dwellProject, s.dwellSinceTS)
	}
}

// --- checkAutoAnchorArm: below/at threshold, never overrides, disable knob ---

func TestCheckAutoAnchorArm_BelowThresholdDoesNotArm(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 600
	updateDwell(s, 100)

	if checkAutoAnchorArm(699, s) { // 599s elapsed, 1s short
		t.Error("armed below the dwell threshold, want false")
	}
	if s.anchor != nil {
		t.Errorf("anchor set early: %+v", s.anchor)
	}
}

func TestCheckAutoAnchorArm_AtThresholdArms(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 600
	updateDwell(s, 100)

	if !checkAutoAnchorArm(700, s) { // exactly 600s elapsed
		t.Fatal("did not arm at exactly the dwell threshold, want armed")
	}
	if s.anchor == nil {
		t.Fatal("anchor is nil after arming")
	}
	if want := "example/backend (auto)"; s.anchor.Title != want {
		t.Errorf("Title = %q, want %q", s.anchor.Title, want)
	}
	if s.anchor.Project != "example/backend" {
		t.Errorf("Project = %q, want %q", s.anchor.Project, "example/backend")
	}
	if s.anchor.SinceTS != 700 {
		t.Errorf("SinceTS = %d, want 700 (arm time)", s.anchor.SinceTS)
	}
	if !isAutoAnchor(s.anchor) {
		t.Error("isAutoAnchor = false for a just-armed anchor")
	}
}

func TestCheckAutoAnchorArm_NeverOverridesExistingAnchor(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 600
	s.anchor = &proto.Anchor{Title: "IMP-97 something else", Project: "example/other", SinceTS: 0}
	updateDwell(s, 0)

	if checkAutoAnchorArm(1_000_000, s) {
		t.Error("armed over an existing anchor (explicit or auto), want false")
	}
	if s.anchor.Title != "IMP-97 something else" {
		t.Errorf("existing anchor mutated: %+v", s.anchor)
	}
}

func TestCheckAutoAnchorArm_ZeroDisablesEntirely(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 0
	updateDwell(s, 0)

	if checkAutoAnchorArm(10_000_000, s) {
		t.Error("armed with autoAnchorMinSec=0, want disabled")
	}
}

// --- checkAutoAnchorAway: sustained vs brief hop, explicit anchors exempt ---

func TestCheckAutoAnchorAway_SustainedAbsenceFiresBoundary(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 0}
	s.autoAnchorAwayMinSec = 180
	rec := &fullNotifRecorder{}

	if checkAutoAnchorAway(50, s, rec.fire) {
		t.Fatal("fired before the away threshold, want false")
	}
	if s.autoAwaySinceTS != 50 {
		t.Errorf("autoAwaySinceTS = %d, want 50 (first-observed-away)", s.autoAwaySinceTS)
	}
	if !checkAutoAnchorAway(50+180, s, rec.fire) {
		t.Fatal("did not fire at the away threshold, want true")
	}
	if s.anchor != nil {
		t.Error("anchor not cleared by the away-boundary")
	}
	recs := rec.snapshot()
	if len(recs) != 1 || recs[0].Kind != "boundary" {
		t.Fatalf("want exactly 1 boundary notification, got %+v", recs)
	}
}

func TestCheckAutoAnchorAway_BriefHopHolds(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 0}
	s.autoAnchorAwayMinSec = 180
	rec := &fullNotifRecorder{}

	if checkAutoAnchorAway(10, s, rec.fire) {
		t.Fatal("fired immediately on the first away observation, want false")
	}
	// The operator returns well before the threshold — a brief hop.
	s.clientSessions["c1"] = "example-backend"
	if checkAutoAnchorAway(50, s, rec.fire) {
		t.Fatal("fired after the operator returned, want false")
	}
	if s.autoAwaySinceTS != 0 {
		t.Errorf("autoAwaySinceTS = %d after return, want reset to 0 (no memory of the hop)", s.autoAwaySinceTS)
	}
	if s.anchor == nil {
		t.Error("anchor cleared by a brief hop, want held (exactly like an explicit anchor)")
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("got notifications for a brief hop: %+v", recs)
	}
}

func TestCheckAutoAnchorAway_ExplicitAnchorNeverFires(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend", SinceTS: 0}
	s.autoAnchorAwayMinSec = 1 // tiny — would fire instantly if this applied
	rec := &fullNotifRecorder{}

	if checkAutoAnchorAway(1000, s, rec.fire) {
		t.Error("away-boundary fired for an EXPLICIT anchor, want never — only auto-anchors carry this exit")
	}
	if s.anchor == nil {
		t.Error("explicit anchor cleared, want held ('switching sessions does not move the anchor')")
	}
}

func TestCheckAutoAnchorAway_ZeroDisablesEntirely(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 0}
	s.autoAnchorAwayMinSec = 0
	rec := &fullNotifRecorder{}

	if checkAutoAnchorAway(1_000_000, s, rec.fire) {
		t.Error("away-boundary fired with autoAnchorAwayMinSec=0, want disabled")
	}
	if s.anchor == nil {
		t.Error("auto anchor cleared with the away-boundary disabled, want held")
	}
}

// --- checkBoundary's existing finish/expiry conditions apply, unchanged,
// to auto-anchors (they are anchor-kind-agnostic already) ---

func TestCheckBoundary_FinishAppliesToAutoAnchor(t *testing.T) {
	s := stateWithProject("example-agora", projectData{Attention: proto.AttFinished})
	s.anchor = &proto.Anchor{Title: "example/agora (auto)", Project: "example/agora", SinceTS: 100}
	s.anchorFinishArmed = true
	rec := &fullNotifRecorder{}

	if !checkBoundary(200, s, rec.fire) {
		t.Fatal("finish boundary did not fire for an auto anchor")
	}
	if s.anchor != nil {
		t.Error("auto anchor not cleared on finish")
	}
}

func TestCheckBoundary_ExpiryAppliesToAutoAnchor(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 0}
	s.anchorExpirySec = 90 * 60
	rec := &fullNotifRecorder{}

	if !checkBoundary(90*60, s, rec.fire) {
		t.Fatal("expiry boundary did not fire for an auto anchor")
	}
	if s.anchor != nil {
		t.Error("auto anchor not cleared on expiry")
	}
}

// --- re-arm hygiene: a boundary force-restarts the dwell clock ---

func TestReArmHygiene_RequiresFullFreshDwellPostBoundary(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.autoAnchorMinSec = 600
	updateDwell(s, 0)
	if !checkAutoAnchorArm(600, s) {
		t.Fatal("did not arm at the first dwell threshold")
	}

	// A boundary fires (expiry, here) while the operator is STILL sitting
	// in the very same session.
	s.anchorExpirySec = 1
	rec := &fullNotifRecorder{}
	if !checkBoundary(601, s, rec.fire) {
		t.Fatal("expiry boundary did not fire")
	}
	if s.anchor != nil {
		t.Fatal("anchor not cleared by the boundary")
	}

	// The dwell clock must have been force-restarted AT this pass's `now`
	// — not left holding its pre-boundary accumulated value.
	if s.dwellSinceTS != 601 {
		t.Errorf("dwellSinceTS = %d after the boundary, want reset to 601 (now)", s.dwellSinceTS)
	}
	if checkAutoAnchorArm(601, s) {
		t.Error("re-armed in the SAME instant as the boundary, want a fresh dwell required")
	}

	// Only after a full fresh dwell period does it re-arm.
	if !checkAutoAnchorArm(601+600, s) {
		t.Error("did not re-arm after a full fresh dwell following the boundary")
	}
}

func TestReArmHygiene_AwayBoundaryAlsoRestartsDwell(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"example/backend"}
	s.clientSessions["c1"] = "example-backend"
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 0}
	s.autoAnchorAwayMinSec = 100
	s.autoAnchorMinSec = 600
	updateDwell(s, 0) // dwellProject already == the anchor's project

	// Sustained absence fires the away-boundary...
	delete(s.clientSessions, "c1")
	rec := &fullNotifRecorder{}
	checkAutoAnchorAway(50, s, rec.fire)
	if !checkAutoAnchorAway(50+100, s, rec.fire) {
		t.Fatal("away-boundary did not fire")
	}

	// ...and the operator immediately returns to the SAME session. A naive
	// implementation would see dwellProject already == "example/backend"
	// and arm instantly; re-arm hygiene must force a fresh dwell instead.
	s.clientSessions["c1"] = "example-backend"
	updateDwell(s, 151)
	if checkAutoAnchorArm(151, s) {
		t.Error("re-armed instantly after the away-boundary, want a fresh dwell required")
	}
	if !checkAutoAnchorArm(151+600, s) {
		t.Error("did not re-arm after a full fresh dwell following the away-boundary")
	}
}

// --- persistence: auto-anchors never persisted, explicit ones still are ---

func TestSaveState_AutoAnchorNeverPersisted(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: 100}

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
		t.Errorf("ps.Anchor = %+v, want nil — auto-anchors must never reach disk", ps.Anchor)
	}
}

func TestSaveState_ExplicitAnchorStillPersisted(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend", SinceTS: 100}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps.Anchor == nil || ps.Anchor.Title != "IMP-97 validate deploy" {
		t.Errorf("ps.Anchor = %+v, want the explicit anchor preserved", ps.Anchor)
	}
}

// --- airlock: keyed on s.anchor alone, so an auto-anchor engages it free ---

func TestTierCheck_Airlock_EngagesUnderAutoAnchor(t *testing.T) {
	s := stateWithProject("foreign-project", projectData{WaitStartedTS: 100})
	s.anchor = &proto.Anchor{Title: "anchor-project (auto)", Project: "anchor/project", SinceTS: 0}
	recs, fire := makeRecorder()

	tierCheck(160, s, fire) // age = 60 → 1m tier on a DIFFERENT project

	if len(*recs) != 0 {
		t.Fatalf("expected no notification while auto-anchored on a different project, got %v", *recs)
	}
	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want exactly 1 captured item — the airlock is keyed on s.anchor alone, so an auto-anchor engages it for free", s.heldItems)
	}
}

// --- explicit anchor-set upgrades an auto-anchor without firing a boundary ---

// TestSubmitAnchorSet_UpgradesAutoAnchorWithoutBoundary is a Hub-level test:
// setting an auto-shaped anchor (simulating what checkAutoAnchorArm would
// have produced) and then an explicit pick over it must fire ZERO boundary
// notifications — "upgrading the tether is not ending work". applyEvent's
// AnchorSet case already just overwrites s.anchor unconditionally with no
// fire() call, so this is really confirming that mechanism end-to-end
// through the real Submit round trip, not a new code path.
func TestSubmitAnchorSet_UpgradesAutoAnchorWithoutBoundary(t *testing.T) {
	rec := &fullNotifRecorder{}
	h := NewHub(Config{Debounce: testDebounce, Notifier: rec.fire})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	if err := h.SubmitAnchorSet(context.Background(), "example/backend (auto)", "example/backend"); err != nil {
		t.Fatalf("SubmitAnchorSet (simulated auto): %v", err)
	}
	if err := h.SubmitAnchorSet(context.Background(), "IMP-97 validate deploy", "example/backend"); err != nil {
		t.Fatalf("SubmitAnchorSet (explicit upgrade): %v", err)
	}

	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("explicit upgrade over an auto anchor fired a boundary, want none: %+v", recs)
	}

	var last *proto.Snapshot
	select {
	case last = <-snaps:
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot received")
	}
	if last.Anchor == nil || last.Anchor.Title != "IMP-97 validate deploy" {
		t.Errorf("Anchor = %+v, want the upgraded explicit title", last.Anchor)
	}
}

// --- end-to-end: a live Hub arms the auto-anchor from real attendance
// events, then clears it on sustained away, proving hub.go's publishPass
// wiring (not just the pure functions above).

func TestAutoAnchor_EndToEnd_ArmsThenAwayBoundary(t *testing.T) {
	// autoAnchorMinSec/autoAnchorAwayMinSec are WHOLE-SECOND granularity
	// (int64(cfg.AutoAnchorMin.Seconds())) — same as AnchorExpiry in
	// TestBoundary_Expiry_EndToEnd — so a sub-second duration here would
	// truncate to 0 (disabled). 1 second each keeps this test's real-time
	// bound at a few seconds.
	rec := &fullNotifRecorder{}
	h := NewHub(Config{
		Debounce:          testDebounce,
		Notifier:          rec.fire,
		AutoAnchorMin:     time.Second,
		AutoAnchorAwayMin: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	if err := h.Submit(tmuxctl.ProjectListChanged{Names: []string{"example/backend"}}); err != nil {
		t.Fatalf("Submit ProjectListChanged: %v", err)
	}
	if err := h.Submit(tmuxctl.ClientListRefresh{ClientSessions: map[string]string{"c1": "example-backend"}}); err != nil {
		t.Fatalf("Submit ClientListRefresh: %v", err)
	}

	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	// AutoAnchorMin is real wall-clock duration (like AnchorExpiry in
	// TestBoundary_Expiry_EndToEnd) — no injectable clock exists at the
	// Hub-construction boundary, so poll with a bounded loop rather than a
	// fixed sleep, per project convention.
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
		t.Fatal("auto-anchor never armed within 5s")
	}
	if armed.Title != "example/backend (auto)" {
		t.Errorf("Title = %q, want %q", armed.Title, "example/backend (auto)")
	}

	// The operator leaves entirely — sustained absence must clear the
	// auto-anchor and fire exactly one boundary notification.
	if err := h.Submit(tmuxctl.ClientListRefresh{ClientSessions: map[string]string{}}); err != nil {
		t.Fatalf("Submit ClientListRefresh (detach): %v", err)
	}
	deadline2 := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline2) {
		if _, _, err := h.SubmitCursor(context.Background(), 0); err != nil {
			t.Fatalf("SubmitCursor: %v", err)
		}
		if len(rec.snapshot()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recs := rec.snapshot()
	if len(recs) != 1 || recs[0].Kind != "boundary" {
		t.Fatalf("got %+v, want exactly 1 boundary notification (away)", recs)
	}
}
