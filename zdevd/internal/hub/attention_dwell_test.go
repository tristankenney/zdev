package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestApplyDwell enumerates the minimum-dwell debounce decision table. Each
// row is one call; the function is pure so the table is exhaustive.
func TestApplyDwell(t *testing.T) {
	const dwell = int64(250)
	const W, A, F, I = proto.AttWorking, proto.AttWaiting, proto.AttFinished, proto.AttIdle

	tests := []struct {
		name       string
		committed  proto.Attention
		init       bool
		derived    proto.Attention
		pendCand   proto.Attention
		pendSince  int64
		nowMS      int64
		dwellMS    int64
		wantCommit proto.Attention
		wantPend   proto.Attention
		wantPendTS int64
	}{
		{
			name:      "disabled commits derived immediately",
			committed: W, init: true, derived: A,
			dwellMS:    0,
			wantCommit: A, wantPend: I, wantPendTS: 0,
		},
		{
			name:      "cold start commits derived immediately",
			committed: I, init: false, derived: W,
			nowMS: 1000, dwellMS: dwell,
			wantCommit: W, wantPend: I, wantPendTS: 0,
		},
		{
			name:      "no transition clears any pending",
			committed: A, init: true, derived: A,
			pendCand: W, pendSince: 900,
			nowMS: 1000, dwellMS: dwell,
			wantCommit: A, wantPend: I, wantPendTS: 0,
		},
		{
			name:      "first divergence starts the clock, holds committed",
			committed: W, init: true, derived: A,
			pendCand: I, pendSince: 0,
			nowMS: 1000, dwellMS: dwell,
			wantCommit: W, wantPend: A, wantPendTS: 1000,
		},
		{
			name:      "within window keeps committed and pending",
			committed: W, init: true, derived: A,
			pendCand: A, pendSince: 1000,
			nowMS: 1100, dwellMS: dwell, // only 100ms < 250ms
			wantCommit: W, wantPend: A, wantPendTS: 1000,
		},
		{
			name:      "candidate held full window commits",
			committed: W, init: true, derived: A,
			pendCand: A, pendSince: 1000,
			nowMS: 1250, dwellMS: dwell, // exactly 250ms
			wantCommit: A, wantPend: I, wantPendTS: 0,
		},
		{
			name:      "candidate change mid-window restarts clock",
			committed: W, init: true, derived: F,
			pendCand: A, pendSince: 1000,
			nowMS: 1100, dwellMS: dwell,
			wantCommit: W, wantPend: F, wantPendTS: 1100,
		},
		{
			name:      "pending candidate is idle (distinguished by pendSince)",
			committed: W, init: true, derived: I,
			pendCand: I, pendSince: 1000,
			nowMS: 1300, dwellMS: dwell, // 300ms >= 250ms
			wantCommit: I, wantPend: I, wantPendTS: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotCommit, gotPend, gotTS := applyDwell(
				tc.committed, tc.init, tc.derived,
				tc.pendCand, tc.pendSince, tc.nowMS, tc.dwellMS,
			)
			if gotCommit != tc.wantCommit {
				t.Errorf("committed = %q; want %q", gotCommit, tc.wantCommit)
			}
			if gotPend != tc.wantPend {
				t.Errorf("pendCand = %q; want %q", gotPend, tc.wantPend)
			}
			if gotTS != tc.wantPendTS {
				t.Errorf("pendSince = %d; want %d", gotTS, tc.wantPendTS)
			}
		})
	}
}

// dwellTestState builds a single-session state wired so DeriveAttention
// produces a clean working↔waiting derived sequence: the user is "present"
// (lastVisitTS) to disable the no-visit latch, and the last title change is
// stamped just after the visit so the stale-✳ demoter does not fire.
func dwellTestState(t *testing.T, now int64, dwell time.Duration) (*state, string) {
	t.Helper()
	const name = "proj"
	s := buildTestState(name, []string{"%1"}, []string{"⠂ claude"}) // working
	s.statusDwell = dwell
	s.lastVisitTS[name] = now           // visited at/after WaitStartedTS → latch off
	s.lastTitleChangeTS[name] = now + 1 // title moved after the visit → no demote
	return s, name
}

func setTitle(s *state, title string) { s.panesByID["%1"].Title = title }

// TestBuildSnapshot_StatusDwell_SuppressesFlap proves a working→waiting→working
// blip inside the dwell window never surfaces as "waiting" on the wire.
func TestBuildSnapshot_StatusDwell_SuppressesFlap(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)

	// Pass 1 (t=1000ms): cold start commits working.
	snap := buildSnapshot(s, 1, time.Time{}, now, 1000)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWorking {
		t.Fatalf("pass 1 Attention = %q; want working", got)
	}

	// Pass 2 (t=1100ms): derived flips to waiting — within the window, so the
	// display must stay working while the candidate is pending.
	setTitle(s, "● claude") // waiting
	snap = buildSnapshot(s, 2, time.Time{}, now, 1100)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWorking {
		t.Fatalf("pass 2 Attention = %q; want working (waiting still pending)", got)
	}

	// Pass 3 (t=1150ms): derived reverts to working before the window elapses.
	// The waiting candidate is dropped; "waiting" was never shown.
	setTitle(s, "⠂ claude") // working
	snap = buildSnapshot(s, 3, time.Time{}, now, 1150)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWorking {
		t.Fatalf("pass 3 Attention = %q; want working (flap suppressed)", got)
	}
	if pd := s.projectData[name]; pd.PendingSinceMS != 0 {
		t.Errorf("PendingSinceMS = %d; want 0 (candidate cleared)", pd.PendingSinceMS)
	}
}

// TestBuildSnapshot_StatusDwell_CommitsSustained proves a genuine transition
// that holds past the dwell window is promoted to the displayed status.
func TestBuildSnapshot_StatusDwell_CommitsSustained(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)

	// Pass 1 (t=1000ms): commit working.
	_ = buildSnapshot(s, 1, time.Time{}, now, 1000)

	// Pass 2 (t=1100ms): waiting appears — pending, display still working.
	setTitle(s, "● claude")
	snap := buildSnapshot(s, 2, time.Time{}, now, 1100)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWorking {
		t.Fatalf("pass 2 Attention = %q; want working (within window)", got)
	}

	// Pass 3 (t=1400ms): waiting has held 300ms >= 250ms → commit waiting.
	snap = buildSnapshot(s, 3, time.Time{}, now, 1400)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWaiting {
		t.Fatalf("pass 3 Attention = %q; want waiting (held past window)", got)
	}
}

// TestBuildSnapshot_StatusDwell_DisabledIsImmediate confirms statusDwell == 0
// preserves the pre-debounce behavior: every derived transition surfaces at
// once.
func TestBuildSnapshot_StatusDwell_DisabledIsImmediate(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 0) // disabled

	_ = buildSnapshot(s, 1, time.Time{}, now, 1000)
	setTitle(s, "● claude")
	snap := buildSnapshot(s, 2, time.Time{}, now, 1010) // 10ms later
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWaiting {
		t.Fatalf("Attention = %q; want waiting immediately (debounce disabled)", got)
	}
}

// TestEarliestDwellDeadlineMS covers the hub's timer-arming helper.
func TestEarliestDwellDeadlineMS(t *testing.T) {
	s := newState()
	s.statusDwell = 250 * time.Millisecond

	if got := earliestDwellDeadlineMS(s); got != 0 {
		t.Errorf("no pending: got %d; want 0", got)
	}

	pdA := s.projectData["a"]
	pdA.PendingSinceMS = 1000
	s.projectData["a"] = pdA
	pdB := s.projectData["b"]
	pdB.PendingSinceMS = 800
	s.projectData["b"] = pdB

	if got, want := earliestDwellDeadlineMS(s), int64(800+250); got != want {
		t.Errorf("earliest deadline = %d; want %d (min pending + dwell)", got, want)
	}

	// Disabled → always 0 regardless of pending candidates.
	s.statusDwell = 0
	if got := earliestDwellDeadlineMS(s); got != 0 {
		t.Errorf("disabled: got %d; want 0", got)
	}
}

// ---- waiting-dwell (transition-aware; dogfood 2026-06-07) ----
//
// The flat 250ms dwell could never suppress poll-sampled blips: cross-
// session titles arrive every 5s, so a single waiting-shaped sample
// (Claude's inter-command ✳ flash) stands unrefuted for a full poll
// period. Transitions INTO waiting now take waitingDwell (~7s) unless a
// fresh hook receipt (HookWaitTS) confirms the wait, which displays
// instantly.

// TestBuildSnapshot_WaitingDwell_SuppressesPollBlip replays the exact
// dogfood flap at title-poll cadence: working → one waiting sample →
// working. With waitingDwell=7s the blip never surfaces.
func TestBuildSnapshot_WaitingDwell_SuppressesPollBlip(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)
	s.waitingDwell = 7 * time.Second

	// t=0s: cold start commits working.
	_ = buildSnapshot(s, 1, time.Time{}, now, 0)

	// t=5s poll: a single ✳ blip sample. Old behavior: 250ms dwell expires
	// long before the next poll → committed waiting. New behavior: the
	// waiting candidate needs 7s.
	setTitle(s, "✳ between commands")
	snap := buildSnapshot(s, 2, time.Time{}, now+5, 5000)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWaiting {
		// it must NOT be waiting
	} else {
		t.Fatalf("t=5s Attention = waiting; blip committed instantly")
	}
	// Intermediate publish 1s later (event-driven passes happen between
	// polls) — still inside the 7s window.
	snap = buildSnapshot(s, 3, time.Time{}, now+6, 6000)
	if got := findProject(snap.Projects, name).Attention; got == proto.AttWaiting {
		t.Fatalf("t=6s Attention = waiting; want suppressed (within waitingDwell)")
	}

	// t=10s poll: title is back to working — the candidate drops; the blip
	// was never shown.
	setTitle(s, "⠂ claude")
	snap = buildSnapshot(s, 4, time.Time{}, now+10, 10000)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWorking {
		t.Fatalf("t=10s Attention = %q; want working (blip suppressed end-to-end)", got)
	}
	if pd := s.projectData[name]; pd.PendingSinceMS != 0 {
		t.Errorf("PendingSinceMS = %d; want 0 (candidate cleared)", pd.PendingSinceMS)
	}
}

// TestBuildSnapshot_WaitingDwell_HookWaitDisplaysInstantly: a hook-fired
// wait (zdev-notify → NotifSeen) carries a fresh HookWaitTS receipt and
// bypasses the long dwell entirely — genuine claude/opencode waits keep
// sub-second display latency.
func TestBuildSnapshot_WaitingDwell_HookWaitDisplaysInstantly(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)
	s.waitingDwell = 7 * time.Second

	_ = buildSnapshot(s, 1, time.Time{}, now, 0)

	// Hook fires (Notification → zdev-notify waiting) and the title poll
	// brings the ● title in the same window.
	applyEvent(s, tmuxctl.NotifSeen{Session: name, Timestamp: now + 4, Kind: proto.WaitKindDecision, Summary: "which db?"}, nil)
	setTitle(s, "● claude")
	snap := buildSnapshot(s, 2, time.Time{}, now+5, 5000)
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWaiting {
		t.Fatalf("hook-confirmed wait Attention = %q; want waiting INSTANTLY", got)
	}
}

// TestBuildSnapshot_WaitingDwell_TitleOnlyCommitsAfterDwell: a sustained
// title-only wait (un-hooked agent) still surfaces — just after the
// poll-aware window instead of 250ms.
func TestBuildSnapshot_WaitingDwell_TitleOnlyCommitsAfterDwell(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)
	s.waitingDwell = 7 * time.Second

	_ = buildSnapshot(s, 1, time.Time{}, now, 0)

	setTitle(s, "● someagent")
	_ = buildSnapshot(s, 2, time.Time{}, now+5, 5000)   // candidate starts
	snap := buildSnapshot(s, 3, time.Time{}, now+13, 13000) // 8s held > 7s window
	if got := findProject(snap.Projects, name).Attention; got != proto.AttWaiting {
		t.Fatalf("sustained title wait Attention = %q; want waiting after dwell", got)
	}
}

// TestBuildSnapshot_WaitingDwell_StaleHookReceiptDoesNotBypass: an old
// HookWaitTS (wait long since answered) must not fast-track a later
// unrelated ✳ blip.
func TestBuildSnapshot_WaitingDwell_StaleHookReceiptDoesNotBypass(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)
	s.waitingDwell = 7 * time.Second

	pd := s.projectData[name]
	pd.HookWaitTS = now - 120 // stale: two minutes old
	s.projectData[name] = pd

	_ = buildSnapshot(s, 1, time.Time{}, now, 0)
	setTitle(s, "✳ blip")
	snap := buildSnapshot(s, 2, time.Time{}, now+5, 5000)
	if got := findProject(snap.Projects, name).Attention; got == proto.AttWaiting {
		t.Fatal("stale hook receipt bypassed the waiting dwell")
	}
}

// TestEarliestDwellDeadline_WaitingCandidateUsesLongWindow pins the
// timer/snapshot agreement (invariants review finding 1): a pending
// waiting candidate's deadline must use waitingDwell, or the Run loop
// arms a 250ms timer whose deadline is instantly in the past and spins
// a ~1ms wake loop for the remaining ~6.75s.
func TestEarliestDwellDeadline_WaitingCandidateUsesLongWindow(t *testing.T) {
	now := time.Now().Unix()
	s, name := dwellTestState(t, now, 250*time.Millisecond)
	s.waitingDwell = 7 * time.Second

	_ = buildSnapshot(s, 1, time.Time{}, now, 0)
	setTitle(s, "✳ blip")
	_ = buildSnapshot(s, 2, time.Time{}, now+5, 5000) // candidate starts at 5000ms

	got := earliestDwellDeadlineMS(s)
	want := int64(5000 + 7000)
	if got != want {
		t.Fatalf("earliestDwellDeadlineMS = %d; want %d (PendingSince + waitingDwell)", got, want)
	}
	_ = name
}

// TestPersistRestore_WaitSurvivesAsConfirmed pins invariants-review
// finding 2: a persisted pre-restart wait must keep its latch on the
// first post-restart pass even when the agent's waiting title is gone —
// the restore seeds the DISPLAYED attention, which is what WaitConfirmed
// reads.
func TestPersistRestore_WaitSurvivesAsConfirmed(t *testing.T) {
	now := time.Now().Unix()

	// Pre-restart state: committed, unvisited wait.
	s1 := buildTestState("proj", []string{"%1"}, []string{"● claude"})
	s1.projectListNames = []string{"proj"}
	pd := s1.projectData["proj"]
	pd.Attention = proto.AttWaiting
	pd.AttentionDerived = proto.AttWaiting
	pd.AttentionInit = true
	pd.WaitStartedTS = now - 300
	s1.projectData["proj"] = pd

	dir := t.TempDir()
	path := dir + "/state.json"
	if err := saveState(path, s1); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Post-restart: agent's pane shows a non-waiting title (it self-
	// resolved while the daemon was down) — the latch is the only thing
	// keeping the unseen wait alive.
	s2 := buildTestState("proj", []string{"%1"}, []string{"⠂ claude"})
	s2.projectListNames = []string{"proj"}
	s2.statusDwell = 250 * time.Millisecond
	s2.waitingDwell = 7 * time.Second
	ps, err := loadState(path)
	if err != nil || ps == nil {
		t.Fatalf("loadState: %v %v", ps, err)
	}
	applyPersistedState(s2, ps)

	snap := buildSnapshot(s2, 1, time.Time{}, now, 0)
	if got := findProject(snap.Projects, "proj").Attention; got != proto.AttWaiting {
		t.Fatalf("post-restart Attention = %q; want waiting (persisted latch must survive WaitConfirmed gating)", got)
	}
}
