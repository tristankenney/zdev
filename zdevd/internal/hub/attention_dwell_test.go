package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
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
