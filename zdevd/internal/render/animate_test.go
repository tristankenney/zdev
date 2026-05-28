package render

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

func TestAnimator_Tick_AdvancesPulse(t *testing.T) {
	a := NewAnimator()
	if a.pulseFrame != 0 {
		t.Fatalf("init pulseFrame = %d", a.pulseFrame)
	}
	a.Tick()
	if a.pulseFrame != 1 {
		t.Errorf("after 1 tick pulseFrame = %d; want 1", a.pulseFrame)
	}
	for i := 0; i < 7; i++ {
		a.Tick()
	}
	if a.pulseFrame != 0 {
		t.Errorf("after 8 ticks pulseFrame = %d; want 0 (wrap)", a.pulseFrame)
	}
}

func TestAnimator_Tick_AdvancesBreath(t *testing.T) {
	a := NewAnimator()
	for i := 0; i < BreathHold; i++ {
		a.Tick()
	}
	if a.breathState != 1 {
		t.Errorf("after %d ticks breathState = %d; want 1", BreathHold, a.breathState)
	}
	for i := 0; i < 3*BreathHold; i++ {
		a.Tick()
	}
	if a.breathState != 0 {
		t.Errorf("after %d ticks breathState = %d; want 0 (wrap)", 4*BreathHold, a.breathState)
	}
}

func TestAnimator_OnSnapshot_PreservesCounters(t *testing.T) {
	// OnSnapshot must NOT reset the pulse/breath counters — heartbeat
	// snapshots from the daemon (1Hz from the list-clients poll) would
	// otherwise jitter the animation back to frame 0 mid-cycle. Counters
	// advance solely under Tick(); OnSnapshot only stores the snapshot.
	a := NewAnimator()
	for i := 0; i < 50; i++ {
		a.Tick()
	}
	wantPulse := a.pulseFrame
	wantPulseTicks := a.pulseTickCount
	wantBreath := a.breathState
	wantBreathTicks := a.breathTickCount
	if wantPulse == 0 && wantBreath == 0 {
		t.Fatal("counters should be non-zero before OnSnapshot for the test to be meaningful")
	}
	snap := &proto.Snapshot{}
	a.OnSnapshot(snap)
	if a.pulseFrame != wantPulse || a.pulseTickCount != wantPulseTicks ||
		a.breathState != wantBreath || a.breathTickCount != wantBreathTicks {
		t.Errorf("OnSnapshot mutated counters: pulse=%d→%d tickC=%d→%d breath=%d→%d btickC=%d→%d",
			wantPulse, a.pulseFrame, wantPulseTicks, a.pulseTickCount,
			wantBreath, a.breathState, wantBreathTicks, a.breathTickCount)
	}
	if a.LastSnap() != snap {
		t.Error("LastSnap() did not return the stored snapshot")
	}
}

func TestAnimator_CadenceFor_AnimatingFast(t *testing.T) {
	a := NewAnimator()
	snap := &proto.Snapshot{
		Projects:    []proto.Project{{Name: "alpha", Status: "waiting"}},
		PaneVisible: true,
	}
	if got := a.CadenceFor(snap); got != time.Duration(FrameSleepMS)*time.Millisecond {
		t.Errorf("CadenceFor(waiting) = %v; want %dms", got, FrameSleepMS)
	}
}

// 260515 perf fix: current-session ALONE no longer drives fast cadence.
// Only a waiting agent (whose pulse glyph genuinely needs 15fps) keeps the
// renderer at FrameSleepMS. A current-session breath bar with no waiting
// agent uses IdleSleepMS so 13+ sidebar renderers on a multi-pane setup
// don't collectively starve tmux's input handler.
func TestAnimator_CadenceFor_CurrentSessionAloneIsIdle(t *testing.T) {
	a := NewAnimator()
	snap := &proto.Snapshot{
		CurrentSession: "alpha",
		Projects:       []proto.Project{{Name: "alpha", Status: "alive"}},
		PaneVisible:    true,
	}
	if got := a.CadenceFor(snap); got != time.Duration(IdleSleepMS)*time.Millisecond {
		t.Errorf("CadenceFor(current-session, no waiting) = %v; want %dms (idle)", got, IdleSleepMS)
	}
}

func TestAnimator_CadenceFor_IdleSlow(t *testing.T) {
	a := NewAnimator()
	snap := &proto.Snapshot{
		Projects:    []proto.Project{{Name: "alpha", Status: "alive"}},
		PaneVisible: true,
	}
	if got := a.CadenceFor(snap); got != time.Duration(IdleSleepMS)*time.Millisecond {
		t.Errorf("CadenceFor(idle) = %v; want %dms", got, IdleSleepMS)
	}
}

// TestAnimator_CadenceFor_InvisiblePaused verifies that a snapshot whose
// PaneVisible is false returns the long InvisibleSleepMS cadence regardless
// of waiting state. The renderer uses this to halt animation when its pane
// has no attached tmux client (no user is looking at it).
func TestAnimator_CadenceFor_InvisiblePaused(t *testing.T) {
	a := NewAnimator()
	cases := []struct {
		name string
		snap *proto.Snapshot
	}{
		{"invisible+waiting", &proto.Snapshot{
			Projects: []proto.Project{{Name: "alpha", Status: "waiting"}},
		}},
		{"invisible+alive", &proto.Snapshot{
			Projects: []proto.Project{{Name: "alpha", Status: "alive"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := a.CadenceFor(c.snap); got != time.Duration(InvisibleSleepMS)*time.Millisecond {
				t.Errorf("CadenceFor(%s) = %v; want %dms (paused)", c.name, got, InvisibleSleepMS)
			}
		})
	}
}

func TestAnimator_PulseFrame_Glyph(t *testing.T) {
	a := NewAnimator()
	for i := 0; i < 8; i++ {
		if a.PulseGlyph() != PulseFrames[i] {
			t.Errorf("at frame %d PulseGlyph = %q; want %q", i, a.PulseGlyph(), PulseFrames[i])
		}
		a.Tick()
	}
}

func TestAnimator_BreathFrame(t *testing.T) {
	a := NewAnimator()
	// At init: frame 0.
	if got := a.BreathFrame(); got != 0 {
		t.Errorf("init BreathFrame = %d; want 0", got)
	}
	// After 1*BreathHold ticks: frame 1.
	for i := 0; i < BreathHold; i++ {
		a.Tick()
	}
	if got := a.BreathFrame(); got != 1 {
		t.Errorf("after %d ticks BreathFrame = %d; want 1", BreathHold, got)
	}
	// After 2*BreathHold ticks: frame 2.
	for i := 0; i < BreathHold; i++ {
		a.Tick()
	}
	if got := a.BreathFrame(); got != 2 {
		t.Errorf("after %d ticks BreathFrame = %d; want 2", 2*BreathHold, got)
	}
	// After 3*BreathHold ticks: frame 3.
	for i := 0; i < BreathHold; i++ {
		a.Tick()
	}
	if got := a.BreathFrame(); got != 3 {
		t.Errorf("after %d ticks BreathFrame = %d; want 3", 3*BreathHold, got)
	}
	// After 4*BreathHold ticks: frame 0 (wrap).
	for i := 0; i < BreathHold; i++ {
		a.Tick()
	}
	if got := a.BreathFrame(); got != 0 {
		t.Errorf("after %d ticks BreathFrame = %d; want 0 (wrap)", 4*BreathHold, got)
	}
}
