package render

import (
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// Animator owns the renderer's pulse and breath animation counters.
// CONTEXT D3-07 locks animation as RENDERER-LOCAL — the daemon emits
// state-on-change only and never ticks. Each renderer pane runs its own
// Animator.
//
// The Animator does NOT own a ticker — the caller (cmd/zdev-sidebar/main.go)
// drives Tick() from a select-loop on time.NewTicker.C. This separation
// keeps the Animator pure (testable without sleeps) and lets the caller
// re-pace via CadenceFor() on each new snapshot.
//
// Per Plan 01's anti-fork-gate scope decision, internal/render/ is OUT
// of the daemon-only path list — the render package may use time imports
// freely.
type Animator struct {
	pulseFrame      int // 0..7, advances every PulseHold ticks
	pulseTickCount  int // counts ticks toward next pulseFrame advance
	breathState     int // 0..3, advances every BreathHold ticks
	breathTickCount int // counts ticks toward next breathState advance
	lastSnap        *proto.Snapshot

	// Transition flash (delight, 2026-08-20): the moment a row ENTERS an
	// attention state is worth a brief emphasis — the eye gets told "here,
	// now" instead of noticing a difference on the next deliberate scan.
	// attnPrev holds the last observed attention per project; attnFlash
	// the unix second of the last transition into a NON-idle state.
	// Renderer-local like every other animation counter (D3-07); bounded
	// by the fleet's project count, so no pruning discipline is needed
	// the way the hub's monotonic maps require.
	attnPrev  map[string]proto.Attention
	attnFlash map[string]int64
}

// NewAnimator returns a fresh Animator with all counters at 0.
func NewAnimator() *Animator {
	return &Animator{
		attnPrev:  make(map[string]proto.Attention),
		attnFlash: make(map[string]int64),
	}
}

// Tick advances the pulse and breath counters. The caller invokes this
// once per renderer tick (~15fps when animating, ~5fps idle — see
// CadenceFor).
func (a *Animator) Tick() {
	a.pulseTickCount++
	if a.pulseTickCount >= PulseHold {
		a.pulseTickCount = 0
		a.pulseFrame = (a.pulseFrame + 1) % pulseWrap
	}
	a.breathTickCount++
	if a.breathTickCount >= BreathHold {
		a.breathTickCount = 0
		a.breathState = (a.breathState + 1) % len(BreathBrightness)
	}
}

// pulseWrap is the pulse counter's modulus: len(PulseFrames) × 12 so
// every age divisor in PulseGlyphAt (1, 2, 4) divides it evenly, the
// work spinner's workHold (2) divides it evenly (96/2 = 48 ≡ 0 mod
// len(WorkFrames)), and both cycles stay seamless across the wrap.
const pulseWrap = len(PulseFrames) * 12

// workHold is how many renderer ticks each WorkFrames glyph holds. At
// the idle 5fps cadence (the usual rate for a working-but-nothing-
// waiting fleet) that's a glyph change every ~0.4s — a calm ~1.6s full
// rotation; at the waiting 15fps cadence it spins ~3× faster, which
// reads as urgency-adjacent liveliness rather than alarm.
const workHold = 2

// OnSnapshot stores the snapshot for later Render() calls. The animation
// counters are NOT reset — the pulse and breath cycles continue advancing
// independently of snapshot arrivals. The previous reset-on-every-snapshot
// behavior was a bash-baseline artifact: bash's per-frame render loop made
// every frame a fresh state, so resetting was free. In the Go version,
// state-on-change snapshot publishes (with a 1Hz heartbeat from the
// list-clients poll) caused the pulse to visibly jitter — counters reset
// mid-cycle every second, making the marker feel "too fast". The pulse is
// purely a visual rhythm; nothing about it depends on snapshot identity.
func (a *Animator) OnSnapshot(snap *proto.Snapshot) {
	a.lastSnap = snap
}

// CadenceFor returns the renderer tick interval appropriate for the
// snapshot. Three tiers:
//
//   - InvisibleSleepMS — the renderer's pane has no attached tmux client
//     (the user can't see this sidebar). Effectively paused; the next
//     snapshot arrival will trigger a fresh paint and reset the ticker
//     when visibility returns.
//   - FrameSleepMS (15fps) — a waiting agent's pulse glyph needs the
//     higher cadence to animate smoothly.
//   - IdleSleepMS (5fps) — the breath bar cycles slowly enough that 5fps
//     still reads as continuous motion.
//
// Halting invisible-pane animation is the structural fix for the
// "13 sidebars × 15fps each starving tmux's input handler" pattern —
// paint work now scales with attended sessions, not total pane count.
func (a *Animator) CadenceFor(snap *proto.Snapshot) time.Duration {
	if snap == nil {
		return time.Duration(IdleSleepMS) * time.Millisecond
	}
	if !snap.PaneVisible {
		return time.Duration(InvisibleSleepMS) * time.Millisecond
	}
	if anyWaiting(snap) {
		return time.Duration(FrameSleepMS) * time.Millisecond
	}
	return time.Duration(IdleSleepMS) * time.Millisecond
}

// PulseGlyphAt returns the pulse glyph paced by wait age (dogfood
// feedback: a flat ~0.5s pulse reads as alarm from second one). The
// pulse starts as a calm ~2s blink and accelerates as the wait crosses
// the same tiers the notifier uses:
//
//	age < WaitWarnSec (60s)    → ÷4  (~2.1s cycle — present, not loud)
//	age < WaitUrgentSec (300s) → ÷2  (~1.1s cycle)
//	age ≥ WaitUrgentSec        → ÷1  (~0.5s cycle — the classic urgent pulse)
//
// The divisor slows frame advance rather than dropping frames, so the
// cycle shape is identical at every pace; pulseWrap guarantees a
// seamless wrap for every divisor.
func (a *Animator) PulseGlyphAt(ageSec int64) string {
	div := 4
	switch {
	case ageSec >= int64(WaitUrgentSec):
		div = 1
	case ageSec >= int64(WaitWarnSec):
		div = 2
	}
	return PulseFrames[(a.pulseFrame/div)%len(PulseFrames)]
}

// WorkGlyph returns the current working-marker spinner frame. Driven by
// the same tick counter as the pulse so the two animations stay phase-
// stable relative to each other; pulseWrap is a multiple of
// workHold×len(WorkFrames), so the rotation is seamless across the wrap.
func (a *Animator) WorkGlyph() string {
	return WorkFrames[(a.pulseFrame/workHold)%len(WorkFrames)]
}

// BreathFrame returns the current breath cycle index (0..3) for
// use with render.BreathColorForProject.
func (a *Animator) BreathFrame() int { return a.breathState % len(BreathBrightness) }

// FlashSec is how long a row's transition emphasis holds after it enters
// an attention state. Wall-clock seconds, not renderer ticks — the flash
// must read the same at the idle 5fps cadence and the waiting 15fps one.
// 3s: long enough that a glance-away doesn't miss it, short enough that
// three rows changing in a burst never reads as standing alarm.
const FlashSec = 3

// ObserveTransitions records, per project, the moment its attention
// changes — the renderer calls it once per frame, before drawing rows.
// Idempotent across repeated frames of the same snapshot: a moment is
// recorded only when the observed value differs from the stored one.
//
// Two deliberate silences:
//   - First sight is baseline, not a transition. Without this, every
//     daemon restart or pane respawn would flash the entire fleet at
//     once — the same restart-storm failure mode the "✳ Claude Code"
//     idle-title carve-out exists for.
//   - A transition INTO idle clears any active flash and records
//     nothing: fading to quiet is silence, not an event. Only entering
//     waiting/working/finished/dead earns the emphasis.
func (a *Animator) ObserveTransitions(snap *proto.Snapshot, now int64) {
	if snap == nil {
		return
	}
	for i := range snap.Projects {
		p := &snap.Projects[i]
		att := projectAttention(p)
		prev, seen := a.attnPrev[p.Name]
		a.attnPrev[p.Name] = att
		if !seen || att == prev {
			continue
		}
		if att == "" || att == proto.AttIdle {
			delete(a.attnFlash, p.Name)
			continue
		}
		a.attnFlash[p.Name] = now
	}
}

// FlashActive reports whether name's transition emphasis is still live
// at now — true within FlashSec of its last recorded transition.
func (a *Animator) FlashActive(name string, now int64) bool {
	t, ok := a.attnFlash[name]
	return ok && now-t < FlashSec
}

// LastSnap returns the last snapshot passed to OnSnapshot, or nil if
// none has been observed.
func (a *Animator) LastSnap() *proto.Snapshot { return a.lastSnap }

func anyWaiting(snap *proto.Snapshot) bool {
	for i := range snap.Projects {
		if projectAttention(&snap.Projects[i]) == proto.AttWaiting {
			return true
		}
	}
	return false
}
