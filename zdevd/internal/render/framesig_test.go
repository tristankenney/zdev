package render

import (
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestFrameSig pins the tick-skip contract (card 5): the signature must be
// stable across ticks that change no visible glyph, and must change exactly
// when a displayed index advances — at the divisor of the FASTEST pulse any
// waiting row needs.
func TestFrameSig(t *testing.T) {
	calm := &proto.Snapshot{Projects: []proto.Project{
		{Name: "a", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: 1000},
	}}
	now := int64(1010) // age 10s → calm tier, div 4

	a := NewAnimator()
	sig0 := a.FrameSigFor(calm, now)
	a.Tick() // pulseFrame 0→1: calm glyph 1/4 == 0/4, spinner 1/2 == 0/2
	if got := a.FrameSigFor(calm, now); got != sig0 {
		t.Errorf("calm tick 1 changed sig: %+v -> %+v", sig0, got)
	}
	a.Tick() // pulseFrame 2: spinner index 2/2 == 1 — visible change
	if got := a.FrameSigFor(calm, now); got == sig0 {
		t.Error("spinner advance did not change sig")
	}

	// Urgent wait → div 1: every tick changes the sig.
	urgent := &proto.Snapshot{Projects: []proto.Project{
		{Name: "a", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: 1000},
	}}
	uNow := int64(1000 + int64(WaitUrgentSec))
	b := NewAnimator()
	s0 := b.FrameSigFor(urgent, uNow)
	b.Tick()
	if got := b.FrameSigFor(urgent, uNow); got == s0 {
		t.Error("urgent tick did not change sig")
	}

	// The wall-clock second always participates (age strings).
	if a.FrameSigFor(calm, now) == a.FrameSigFor(calm, now+1) {
		t.Error("second rollover did not change sig")
	}

	// A new snapshot pointer always changes the sig.
	clone := *calm
	if a.FrameSigFor(calm, now) == a.FrameSigFor(&clone, now) {
		t.Error("new snapshot pointer did not change sig")
	}
}
