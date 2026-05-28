// internal/hub/notify.go
//
// Wait-tier notification logic. Pure function — no time.Now, no exec, no
// I/O. Caller passes the clock and the fire function.
//
// Threshold ladder (locked): 60s / 5m / 15m with escalating macOS sound.
// Each (project, tier) fires AT MOST ONCE per wait cycle. The bitmap is
// reset to 0 on the recomputeAgents transition INTO waiting (BEFORE the
// WaitStartedTS stamp — replay-safe ordering) and on transition OUT.
//
// Multi-tier collapse: when a single check crosses multiple thresholds
// (e.g. daemon offline for 30 minutes; restart sees age=1800s with no
// bits set), tierCheck fires the HIGHEST-tier-only and marks ALL lower
// bits as suppressed-after-the-fact. This avoids three stacked banners
// for one delayed wakeup. Iteration is from largest threshold down.
//
// EXCEPTIONAL WRITE NOTE: tierCheck writes pd.WaitNotifiedTiers — this
// is the ONLY hub-goroutine write to projectData OUTSIDE applyEvent.
// It is safe because the hub goroutine is the sole owner of state, but
// any future refactor that splits state ownership must audit this site.
package hub

// tier describes one notification threshold in the escalating ladder.
type tier struct {
	AgeSec  int64
	Message string
	Sound   string
	Bit     uint8
}

// tiers is the locked threshold ladder. Tests iterate this slice — do not
// bury the constants inline at the call site. Ordered smallest to largest
// so the iterating-largest-to-smallest pattern in tierCheck is explicit.
var tiers = []tier{
	{AgeSec: 60, Message: "waiting 1m", Sound: "Glass", Bit: 1 << 0},
	{AgeSec: 300, Message: "still waiting (5m)", Sound: "Ping", Bit: 1 << 1},
	{AgeSec: 900, Message: "STUCK (15m)", Sound: "Sosumi", Bit: 1 << 2},
}

// allTierBits is the bitmask with all tier bits set.
const allTierBits uint8 = 0b111

// tierCheck walks every project in s.projectData and fires (at most once
// per project per call) for the highest tier whose threshold is crossed
// and whose bit is unset. Cheap suppressions are checked first; the
// bitmap is updated AFTER firing so a panic in fire still reflects an
// un-fired tier on the next call.
//
// Returns true if at least one tier fired (and therefore WaitNotifiedTiers
// mutated); the hub uses this to force a saveState even when the user-visible
// snapshot is unchanged, so the bitmap update is captured before any
// subsequent crash that would otherwise cause the same tier to re-fire on
// restart.
//
// fire is the dispatch function — nil-safe (entire call is a no-op).
// Production wires fire to RealNotifier(path); tests inject a recorder.
func tierCheck(now int64, s *state, fire func(project, msg, sound string)) bool {
	if fire == nil {
		return false
	}
	fired := false
	for name, pd := range s.projectData {
		if pd.WaitStartedTS == 0 {
			continue
		}
		if pd.WaitNotifiedTiers&allTierBits == allTierBits {
			continue
		}
		if isClientAttended(s, name) {
			continue
		}
		if isWaitAcknowledged(s, name, pd.WaitStartedTS, now) {
			continue
		}
		age := now - pd.WaitStartedTS
		// Iterate from largest threshold down; fire first crossed-and-unfired
		// tier (highest). On a match, mark ALL bits at-or-below the matched
		// tier as set so lower tiers do not fire on subsequent ticks.
		// This collapses a multi-tier crossing (e.g., daemon offline 30m and
		// restart) into a single "highest relevant tier" notification.
		for i := len(tiers) - 1; i >= 0; i-- {
			t := tiers[i]
			if age < t.AgeSec {
				continue // threshold not yet crossed
			}
			if pd.WaitNotifiedTiers&t.Bit != 0 {
				continue // this tier already fired; keep scanning toward smaller tiers
			}
			// First crossed-and-unfired tier (largest such). Fire it, then
			// mark all bits at or below this tier so lower tiers are
			// suppressed without separate firings.
			fire(name, t.Message, t.Sound)
			var combined uint8
			for j := 0; j <= i; j++ {
				combined |= tiers[j].Bit
			}
			pd.WaitNotifiedTiers |= combined
			s.projectData[name] = pd
			fired = true
			break
		}
	}
	return fired
}
