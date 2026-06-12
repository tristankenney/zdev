package render

import "github.com/tristankenney/zdev/zdevd/internal/proto"

// FrameSig identifies everything that can change a rendered frame's bytes
// BETWEEN snapshots: the snapshot itself (pointer identity — a new publish
// is always a new pointer), the effective pulse-glyph index, the work-
// spinner index, the breath slot, and the wall-clock second (age strings).
//
// The renderer's ticker fires at up to 15fps, but PulseHold=1 only advances
// a COUNTER every tick — the visible glyph indices divide it (calm pulse
// ÷4, warn ÷2, spinner ÷workHold), so most ticks rebuild a byte-identical
// frame that the FrameWriter then discards after the full Render cost
// (maps, tallies, ~1.5KB of buffer) was already paid. Comparing FrameSigs
// before rendering skips that work outright: ~50% of ticks with calm waits,
// ~50-90% on an idle fleet, and 0% under an urgent (÷1) pulse — where every
// tick genuinely changes bytes. (Perf-hunt renderer finding #3; the
// finding's other half — per-pane cadence gating — turned out to be a
// non-fix: every sidebar renders the whole fleet, and waiting rows are
// never stale-dimmed, so a visible pulse legitimately needs the fast tick.)
type FrameSig struct {
	Snap   *proto.Snapshot
	Pulse  int
	Work   int
	Breath int
	Sec    int64
}

// FrameSigFor computes the signature for one prospective paint. The pulse
// divisor mirrors PulseGlyphAt's age tiers, taking the FASTEST divisor any
// waiting row needs (a single urgent wait means per-tick byte changes).
// Waiting team members are treated as urgent-rate conservatively — their
// row glyphs ride the same counters and a missed skip is merely a render,
// never a stale frame.
func (a *Animator) FrameSigFor(snap *proto.Snapshot, now int64) FrameSig {
	div := 4
	if snap != nil {
		for i := range snap.Projects {
			p := &snap.Projects[i]
			if projectAttention(p) != proto.AttWaiting {
				continue
			}
			age := int64(0)
			if p.WaitStartedTS > 0 {
				age = now - p.WaitStartedTS
			}
			switch {
			case age >= int64(WaitUrgentSec):
				div = 1
			case age >= int64(WaitWarnSec) && div > 2:
				div = 2
			}
			if div == 1 {
				break
			}
		}
		if div > 1 && teamRowsFor(snap) {
			for i := range snap.TeamGroups {
				for _, m := range snap.TeamGroups[i].Members {
					if m.Status == "waiting" {
						div = 1
						break
					}
				}
			}
		}
	}
	return FrameSig{
		Snap:   snap,
		Pulse:  a.pulseFrame / div,
		Work:   a.pulseFrame / workHold,
		Breath: a.breathState,
		Sec:    now,
	}
}
