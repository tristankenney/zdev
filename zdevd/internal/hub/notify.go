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
// EXCEPTIONAL WRITE NOTE: tierCheck writes pd.WaitNotifiedTiers and
// pd.DeadNotified — these are the ONLY hub-goroutine writes to
// projectData OUTSIDE applyEvent (buildSnapshot's attention/dwell/death
// writes excepted, which own their own fields). Safe because the hub
// goroutine is the sole owner of state, but any future refactor that
// splits state ownership must audit this site.
package hub

import (
	"fmt"
	"sort"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

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

// tierCrossing records one project crossing a tier threshold during a
// tierCheck pass, collected so the fleet router below can collapse a
// multi-project burst into a single notification.
type tierCrossing struct {
	project string
	tierIdx int // index into tiers
	age     int64
	kind    string // pd.WaitKind at crossing time ("" / permission / decision)
}

// tierCheck walks every project in s.projectData, collects every tier
// crossing this pass (per project: the highest crossed-and-unfired tier,
// marking ALL bits at-or-below so lower tiers never fire separately —
// the multi-tier collapse documented in the file header), then routes
// the crossings through the fleet digest:
//
//   - At most ONE notification fires per pass. A single crossing fires
//     close to the classic per-project banner; a multi-project burst
//     (wake-from-sleep, daemon restart replay) collapses to one banner
//     led by the most urgent crossing — highest tier first, then oldest
//     wait, then name (map order must not pick the leader).
//   - The message carries fleet context: the leader's cost-class when
//     it's a permission prompt (" (permission)") and how many OTHER
//     notification-worthy waits exist right now (" · N more waiting") —
//     whether or not they crossed anything this pass.
//   - Presence-aware deferral: while ANY tmux client is attached the
//     lowest tier (60s) neither fires nor marks its bit — the sidebar
//     triage strip and pulse carry sub-minute waits when the user is at
//     the screen. The deferred tier fires the moment the user detaches
//     (next pass) or is absorbed by the 5m tier's crossing. Higher
//     tiers notify regardless of presence: present-but-not-looking is
//     exactly what a 5-minute unnoticed wait means.
//
// Returns true if at least one tier fired (and therefore WaitNotifiedTiers
// mutated); the hub uses this to force a saveState even when the user-visible
// snapshot is unchanged, so the bitmap update is captured before any
// subsequent crash that would otherwise cause the same tier to re-fire on
// restart. Deferral mutates nothing and returns false.
//
// fire is the dispatch function — nil-safe (entire call is a no-op).
// Production wires fire through ResolveNotifier (terminal-notifier /
// notify-send / ZDEV_NOTIFY_CMD); tests inject a recorder. The
// Notification payload carries the digest leader plus its cost-class and
// age in structured form so transports don't re-parse the message; this
// is also the seam the remote push fan-out plugs into.
func tierCheck(now int64, s *state, fire func(Notification)) bool {
	if fire == nil {
		return false
	}
	present := len(s.clientSessions) > 0

	// Death crossings (NOW#3): hook-confirmed unclean exits fire exactly
	// once per disappearance (DeadNotified, persisted) and are NOT
	// deferred by presence — a dead agent stays dead whether or not the
	// user is at the screen, and nothing else will ever escalate it.
	// Being attached to the dead session itself still suppresses (the
	// user is looking at the corpse); the bit is left unset so the
	// banner fires if they detach without relaunching.
	var deaths []string // project names, unsorted here
	for name, pd := range s.projectData {
		if pd.DeadSinceTS == 0 || pd.DeadNotified {
			continue
		}
		if isClientAttended(s, name) {
			continue
		}
		deaths = append(deaths, name)
	}

	var crossings []tierCrossing
	eligibleWaits := 0 // notification-worthy waits, crossing or not — fleet-context denominator
	for name, pd := range s.projectData {
		if pd.WaitStartedTS == 0 {
			continue
		}
		if isClientAttended(s, name) {
			continue
		}
		if isWaitAcknowledged(s, name, pd.WaitStartedTS, now) {
			continue
		}
		eligibleWaits++
		if pd.WaitNotifiedTiers&allTierBits == allTierBits {
			continue
		}
		age := now - pd.WaitStartedTS
		// Iterate from largest threshold down; collect the first
		// crossed-and-unfired tier (highest). On a match, mark ALL bits
		// at-or-below the matched tier so lower tiers do not fire on
		// subsequent ticks — a multi-tier crossing (e.g., daemon offline
		// 30m and restart) collapses to the highest relevant tier.
		for i := len(tiers) - 1; i >= 0; i-- {
			t := tiers[i]
			if age < t.AgeSec {
				continue // threshold not yet crossed
			}
			if pd.WaitNotifiedTiers&t.Bit != 0 {
				continue // this tier already fired; keep scanning toward smaller tiers
			}
			if present && i == 0 {
				// Presence deferral: leave the bit UNSET so the 60s tier
				// fires on detach (or folds into the 5m crossing).
				break
			}
			var combined uint8
			for j := 0; j <= i; j++ {
				combined |= tiers[j].Bit
			}
			pd.WaitNotifiedTiers |= combined
			s.projectData[name] = pd
			crossings = append(crossings, tierCrossing{project: name, tierIdx: i, age: age, kind: pd.WaitKind})
			break
		}
	}
	// A death leads any digest: mark every unnotified death's bit (all
	// are "covered" by this banner), lead with the oldest, and fold the
	// wait context into the message. Tier bits for wait crossings were
	// already marked above, so they don't re-fire next pass either.
	if len(deaths) > 0 {
		sort.Slice(deaths, func(i, j int) bool {
			a, b := s.projectData[deaths[i]], s.projectData[deaths[j]]
			if a.DeadSinceTS != b.DeadSinceTS {
				return a.DeadSinceTS < b.DeadSinceTS // oldest death leads
			}
			return deaths[i] < deaths[j]
		})
		for _, name := range deaths {
			pd := s.projectData[name]
			pd.DeadNotified = true
			s.projectData[name] = pd
		}
		lead := s.projectData[deaths[0]]
		msg := "agent died"
		if lead.DeadReason != "" {
			msg += " (" + lead.DeadReason + ")"
		}
		if extra := len(deaths) - 1; extra > 0 {
			msg += fmt.Sprintf(" · %d more dead", extra)
		}
		if eligibleWaits > 0 {
			msg += fmt.Sprintf(" · %d waiting", eligibleWaits)
		}
		fire(Notification{
			Project: deaths[0],
			Message: msg,
			Sound:   "Sosumi", // the STUCK-tier sound — death is never routine
			Kind:    proto.WaitKindDead,
			AgeSec:  now - lead.DeadSinceTS,
		})
		return true
	}

	if len(crossings) == 0 {
		return false
	}

	// Digest leader: most urgent crossing — highest tier, then oldest
	// wait, then name so map iteration order never decides.
	sort.Slice(crossings, func(i, j int) bool {
		a, b := crossings[i], crossings[j]
		if a.tierIdx != b.tierIdx {
			return a.tierIdx > b.tierIdx
		}
		if a.age != b.age {
			return a.age > b.age
		}
		return a.project < b.project
	})
	top := crossings[0]
	msg := tiers[top.tierIdx].Message
	if top.kind == proto.WaitKindPermission {
		msg += " (permission)"
	}
	if others := eligibleWaits - 1; others > 0 {
		msg += fmt.Sprintf(" · %d more waiting", others)
	}
	fire(Notification{
		Project: top.project,
		Message: msg,
		Sound:   tiers[top.tierIdx].Sound,
		Kind:    top.kind,
		AgeSec:  top.age,
	})
	return true
}
