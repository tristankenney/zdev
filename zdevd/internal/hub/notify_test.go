// internal/hub/notify_test.go
//
// Pure-logic tests for tierCheck. No subprocesses, no sleeps, no filesystem.
// All tests build *state via newState() and mutate maps directly.
package hub

import (
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// notifRecord captures a single fire() call.
type notifRecord struct {
	Project string
	Msg     string
	Sound   string
}

// makeRecorder returns a fire func and a pointer to the slice it appends
// to. The recorder flattens the structured Notification back to the
// (project, msg, sound) triple the assertion tables predate — Kind/AgeSec
// are covered by dedicated structured-payload tests below.
func makeRecorder() (*[]notifRecord, func(Notification)) {
	recs := &[]notifRecord{}
	fire := func(n Notification) {
		*recs = append(*recs, notifRecord{n.Project, n.Message, n.Sound})
	}
	return recs, fire
}

// stateWithProject constructs a *state with a single project pre-populated.
func stateWithProject(name string, pd projectData) *state {
	s := newState()
	s.projectData[name] = pd
	return s
}

// TestTierCheck_A fires Glass at 60s.
func TestTierCheck_A(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})
	recs, fire := makeRecorder()

	tierCheck(160, s, fire) // age = 60

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Project != "example-agora" {
		t.Errorf("Project = %q, want %q", r.Project, "example-agora")
	}
	if r.Msg != "waiting 1m" {
		t.Errorf("Msg = %q, want %q", r.Msg, "waiting 1m")
	}
	if r.Sound != "Glass" {
		t.Errorf("Sound = %q, want %q", r.Sound, "Glass")
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0b001 {
		t.Errorf("WaitNotifiedTiers = %08b, want 0b001", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_B does not fire below 60s.
func TestTierCheck_B(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})
	recs, fire := makeRecorder()

	tierCheck(159, s, fire) // age = 59

	if len(*recs) != 0 {
		t.Errorf("expected 0 records, got %d: %v", len(*recs), *recs)
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0 {
		t.Errorf("WaitNotifiedTiers changed: got %08b, want 0", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_C fires Ping at 5m only once (bit0 already set).
func TestTierCheck_C(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS:     100,
		WaitNotifiedTiers: 0b001, // bit0 already set
	})
	recs, fire := makeRecorder()

	tierCheck(400, s, fire) // age = 300

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Sound != "Ping" {
		t.Errorf("Sound = %q, want %q", r.Sound, "Ping")
	}
	if r.Msg != "still waiting (5m)" {
		t.Errorf("Msg = %q, want %q", r.Msg, "still waiting (5m)")
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0b011 {
		t.Errorf("WaitNotifiedTiers = %08b, want 0b011", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_D fires Sosumi at 15m.
func TestTierCheck_D(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS:     100,
		WaitNotifiedTiers: 0b011, // bits 0+1 already set
	})
	recs, fire := makeRecorder()

	tierCheck(1000, s, fire) // age = 900

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Sound != "Sosumi" {
		t.Errorf("Sound = %q, want %q", r.Sound, "Sosumi")
	}
	if r.Msg != "STUCK (15m)" {
		t.Errorf("Msg = %q, want %q", r.Msg, "STUCK (15m)")
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0b111 {
		t.Errorf("WaitNotifiedTiers = %08b, want 0b111", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_E multi-tier collapse on delayed wakeup.
// Daemon offline 30m: age=900s, no bits set. Expect EXACTLY ONE record —
// the highest tier (Sosumi). All three bits set after.
func TestTierCheck_E(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS:     100,
		WaitNotifiedTiers: 0b000, // no bits set
	})
	recs, fire := makeRecorder()

	tierCheck(1000, s, fire) // age = 900 — crosses all three tiers

	if len(*recs) != 1 {
		t.Fatalf("expected exactly 1 record (highest tier only), got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Sound != "Sosumi" {
		t.Errorf("Sound = %q, want Sosumi (highest tier)", r.Sound)
	}
	if r.Msg != "STUCK (15m)" {
		t.Errorf("Msg = %q, want STUCK (15m)", r.Msg)
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0b111 {
		t.Errorf("WaitNotifiedTiers = %08b, want 0b111 (all bits suppressed-after-the-fact)", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_F all bits set: skip cheaply.
func TestTierCheck_F(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS:     100,
		WaitNotifiedTiers: 0b111, // all set
	})
	recs, fire := makeRecorder()

	tierCheck(2000, s, fire) // age = 1900 — way past all tiers

	if len(*recs) != 0 {
		t.Errorf("expected 0 records (all bits set), got %d: %v", len(*recs), *recs)
	}
}

// TestTierCheck_G WaitStartedTS == 0: skip.
func TestTierCheck_G(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 0, // not waiting
	})
	recs, fire := makeRecorder()

	tierCheck(1000, s, fire)

	if len(*recs) != 0 {
		t.Errorf("expected 0 records (not waiting), got %d: %v", len(*recs), *recs)
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0 {
		t.Errorf("WaitNotifiedTiers mutated: got %08b, want 0", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_H client attended: suppress.
func TestTierCheck_H(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})
	// Simulate client attending this session.
	s.clientSessions["c1"] = "example-agora"
	recs, fire := makeRecorder()

	tierCheck(1000, s, fire) // age = 900 — crosses all tiers

	if len(*recs) != 0 {
		t.Errorf("expected 0 records (client attended), got %d: %v", len(*recs), *recs)
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0 {
		t.Errorf("WaitNotifiedTiers mutated: got %08b, want 0", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_I_FreshAck a visit recent enough to ack the highest-crossed
// tier suppresses notification. WaitStartedTS=100, age now 350 → urgent (300)
// crossed, threshold = 100 + 300 = 400; visit 450 ≥ 400 → ack'd → no fire.
func TestTierCheck_I_FreshAck(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})
	s.lastVisitTS["example-agora"] = 450 // ≥ 100 + 300 (urgent tier floor)
	recs, fire := makeRecorder()

	tierCheck(450, s, fire) // age = 350; urgent tier crossed

	if len(*recs) != 0 {
		t.Errorf("expected 0 records (fresh ack at urgent tier), got %d: %v", len(*recs), *recs)
	}
	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0 {
		t.Errorf("WaitNotifiedTiers mutated: got %08b, want 0", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_I_StaleAckExpires locks 260511-c9s: a visit that ack'd an
// earlier tier MUST stop counting as acknowledgment when the wait age
// crosses a higher tier. User's framing: "I have visited, but not as
// recently as attention was demanded."
//
// Scenario: WaitStartedTS=100, visit at 150 (would have ack'd warn tier at
// 60s). At now=1000, age=900 → stuck tier (900s) crossed → threshold =
// 100 + 900 = 1000. visit (150) < 1000 → ack expires → stuck fires.
func TestTierCheck_I_StaleAckExpires(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})
	s.lastVisitTS["example-agora"] = 150 // post-WaitStartedTS but pre-warn-floor
	recs, fire := makeRecorder()

	tierCheck(1000, s, fire) // age = 900; stuck tier crossed

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record (stale ack expires at stuck tier), got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Project != "example-agora" {
		t.Errorf("Project = %q, want example-agora", r.Project)
	}
	if r.Msg != "STUCK (15m)" {
		t.Errorf("Msg = %q, want STUCK (15m)", r.Msg)
	}
}

// TestTierCheck_J nil fire is safe (no panic, no bitmap mutation).
func TestTierCheck_J(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})

	// Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("tierCheck with nil fire panicked: %v", r)
		}
	}()

	tierCheck(1000, s, nil)

	pd := s.projectData["example-agora"]
	if pd.WaitNotifiedTiers != 0 {
		t.Errorf("WaitNotifiedTiers mutated with nil fire: got %08b, want 0", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_K slash-form vs dash-form key: fire receives the verbatim map key.
func TestTierCheck_K(t *testing.T) {
	s := stateWithProject("example-agora", projectData{
		WaitStartedTS: 100,
	})
	recs, fire := makeRecorder()

	tierCheck(161, s, fire) // age = 61

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(*recs))
	}
	if (*recs)[0].Project != "example-agora" {
		t.Errorf("Project = %q, want dash-form %q", (*recs)[0].Project, "example-agora")
	}
}

// --- fleet router tests (triage slice 3) ---

// TestTierCheck_PresenceDefersLowestTier: while ANY tmux client is
// attached (to a different session), the 60s tier neither fires nor
// marks its bit — and fires the moment the client detaches.
func TestTierCheck_PresenceDefersLowestTier(t *testing.T) {
	s := stateWithProject("proj-a", projectData{WaitStartedTS: 100})
	s.clientSessions["c1"] = "somewhere-else"
	recs, fire := makeRecorder()

	if tierCheck(170, s, fire) { // age = 70 — 60s crossed, but user present
		t.Error("tierCheck returned true while deferring; nothing mutated")
	}
	if len(*recs) != 0 {
		t.Fatalf("expected 0 records while present, got %d: %v", len(*recs), *recs)
	}
	if pd := s.projectData["proj-a"]; pd.WaitNotifiedTiers != 0 {
		t.Fatalf("deferral must not mark bits; got %08b", pd.WaitNotifiedTiers)
	}

	// Detach → the deferred 60s tier fires on the next pass.
	delete(s.clientSessions, "c1")
	if !tierCheck(180, s, fire) {
		t.Fatal("expected fire after detach")
	}
	if len(*recs) != 1 || (*recs)[0].Msg != "waiting 1m" {
		t.Errorf("after detach: recs = %v; want one 'waiting 1m'", *recs)
	}
}

// TestTierCheck_PresenceDoesNotDeferHigherTiers: a 5m wait notifies even
// while the user is present — present-but-not-looking is exactly what an
// unnoticed 5-minute wait means.
func TestTierCheck_PresenceDoesNotDeferHigherTiers(t *testing.T) {
	s := stateWithProject("proj-a", projectData{WaitStartedTS: 100})
	s.clientSessions["c1"] = "somewhere-else"
	recs, fire := makeRecorder()

	tierCheck(400, s, fire) // age = 300 — 5m tier

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record (5m fires despite presence), got %d: %v", len(*recs), *recs)
	}
	if (*recs)[0].Sound != "Ping" {
		t.Errorf("Sound = %q, want Ping", (*recs)[0].Sound)
	}
	// The deferred-then-absorbed 60s bit is set too (multi-tier collapse).
	if pd := s.projectData["proj-a"]; pd.WaitNotifiedTiers != 0b011 {
		t.Errorf("WaitNotifiedTiers = %08b, want 0b011", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_DigestCollapsesBurst: two projects crossing in ONE pass
// (the wake-from-sleep replay) produce a single banner led by the most
// urgent crossing — highest tier first, then oldest — with fleet context,
// while BOTH projects' bitmaps advance so neither re-fires.
func TestTierCheck_DigestCollapsesBurst(t *testing.T) {
	s := newState()
	s.projectData["proj-old"] = projectData{WaitStartedTS: 100} // age 900 → STUCK tier
	s.projectData["proj-new"] = projectData{WaitStartedTS: 930} // age 70 → 60s tier
	recs, fire := makeRecorder()

	if !tierCheck(1000, s, fire) {
		t.Fatal("expected fired=true")
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 digest record, got %d: %v", len(*recs), *recs)
	}
	r := (*recs)[0]
	if r.Project != "proj-old" {
		t.Errorf("digest leader = %q, want proj-old (highest tier)", r.Project)
	}
	if r.Msg != "STUCK (15m) · 1 more waiting" {
		t.Errorf("Msg = %q, want 'STUCK (15m) · 1 more waiting'", r.Msg)
	}
	if r.Sound != "Sosumi" {
		t.Errorf("Sound = %q, want Sosumi (leader's tier)", r.Sound)
	}
	// Both bitmaps advanced — the collapsed crossing never re-fires.
	if pd := s.projectData["proj-old"]; pd.WaitNotifiedTiers != 0b111 {
		t.Errorf("proj-old bits = %08b, want 0b111", pd.WaitNotifiedTiers)
	}
	if pd := s.projectData["proj-new"]; pd.WaitNotifiedTiers != 0b001 {
		t.Errorf("proj-new bits = %08b, want 0b001 (absorbed into digest)", pd.WaitNotifiedTiers)
	}
}

// TestTierCheck_DigestLeaderTieBreaksByAge: equal tiers → oldest wait leads.
func TestTierCheck_DigestLeaderTieBreaksByAge(t *testing.T) {
	s := newState()
	s.projectData["proj-young"] = projectData{WaitStartedTS: 935} // age 65
	s.projectData["proj-elder"] = projectData{WaitStartedTS: 900} // age 100
	recs, fire := makeRecorder()

	tierCheck(1000, s, fire) // both cross 60s only

	if len(*recs) != 1 || (*recs)[0].Project != "proj-elder" {
		t.Errorf("recs = %v; want single record led by proj-elder (oldest)", *recs)
	}
}

// TestTierCheck_PermissionKindInMessage: a permission-class wait says so
// in the banner — that's the "5 seconds of you unblocks an agent" cue.
func TestTierCheck_PermissionKindInMessage(t *testing.T) {
	s := stateWithProject("proj-a", projectData{
		WaitStartedTS: 100,
		WaitKind:      proto.WaitKindPermission,
	})
	recs, fire := makeRecorder()

	tierCheck(160, s, fire) // age = 60

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(*recs))
	}
	if (*recs)[0].Msg != "waiting 1m (permission)" {
		t.Errorf("Msg = %q, want 'waiting 1m (permission)'", (*recs)[0].Msg)
	}
}

// TestTierCheck_FleetSuffixCountsNonCrossingWaits: the "N more waiting"
// context counts every notification-worthy wait, not just this pass's
// crossings — an already-notified wait still demands the user.
func TestTierCheck_FleetSuffixCountsNonCrossingWaits(t *testing.T) {
	s := newState()
	s.projectData["proj-a"] = projectData{WaitStartedTS: 100}                          // crosses 60s now
	s.projectData["proj-b"] = projectData{WaitStartedTS: 50, WaitNotifiedTiers: 0b001} // fired earlier, still waiting
	recs, fire := makeRecorder()

	tierCheck(161, s, fire) // proj-a age 61; proj-b age 111 (bit0 done, 5m not crossed)

	if len(*recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(*recs), *recs)
	}
	if (*recs)[0].Msg != "waiting 1m · 1 more waiting" {
		t.Errorf("Msg = %q, want 'waiting 1m · 1 more waiting'", (*recs)[0].Msg)
	}
}

// TestTierCheck_L multiple projects in one tick: only the un-notified one fires.
func TestTierCheck_L(t *testing.T) {
	s := newState()
	// Project A: no bits set — should fire.
	s.projectData["proj-a"] = projectData{WaitStartedTS: 100, WaitNotifiedTiers: 0b000}
	// Project B: bit0 already set — should NOT fire at 60s age (bit0 done, bit1 not crossed yet).
	s.projectData["proj-b"] = projectData{WaitStartedTS: 100, WaitNotifiedTiers: 0b001}
	recs, fire := makeRecorder()

	tierCheck(161, s, fire) // age = 61 — only 60s tier crossed

	// Only proj-a should fire (bit0 not set); proj-b already has bit0.
	if len(*recs) != 1 {
		t.Fatalf("expected 1 record (only un-bit-set project), got %d: %v", len(*recs), *recs)
	}
	if (*recs)[0].Project != "proj-a" {
		t.Errorf("unexpected project fired: %q, want %q", (*recs)[0].Project, "proj-a")
	}
}
