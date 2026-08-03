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
//
// Phase 3A (docs/design/command-centre.md — "the airlock"): tierCheck also
// writes s.heldItems via captureHeldWait when a tier crossing is airlocked
// (anchored, foreign project — see below), and boundary.go's checkBoundary
// writes s.anchor directly through applyEvent(AnchorClear). Same exception,
// same reasoning.
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
	// Airlock (phase 3A, docs/design/command-centre.md "the airlock" / "the
	// pierce list"): while anchored, a tier crossing on any project OTHER
	// than the anchor's own is HELD — captured into the held set instead of
	// fired, per the pierce list's one explicit behaviour change ("waits on
	// *other* projects... speak aloud today"). The anchor's own project
	// still fires normally (pierce list item (b)). This gate is keyed on
	// s.anchor alone: InFocus-via-commitment (a meeting with no anchor)
	// does NOT gate here — that's the meeting-shield the design defers to
	// the meeting-edge boundary work (command-centre.md "Open calibration").
	// Meeting-in-5m piercing is explicitly out of scope for this phase: no
	// such notification kind exists yet.
	//
	// Tier bits were already marked above regardless of this gate — a
	// suppressed crossing must not re-fire once the operator un-anchors.
	// The alternative (leaving the bit unset so it fires on un-anchor)
	// floods the operator at the exact moment they release the anchor,
	// which is precisely the wrong moment (deliberate choice).
	if s.anchor != nil {
		anchorKey := ""
		if s.anchor.Project != "" {
			anchorKey = proto.SessionKey(s.anchor.Project)
		}
		var kept []tierCrossing
		for _, c := range crossings {
			if anchorKey != "" && c.project == anchorKey {
				kept = append(kept, c)
				continue
			}
			captureHeldWait(s, c, now)
		}
		crossings = kept
		if len(crossings) == 0 && len(deaths) == 0 {
			// Every crossing this pass was airlocked into the held set and
			// no death needs to fire; nothing emits, but the held-set
			// mutation (and the tier bits marked above) both need saving —
			// the caller (publishPass) treats a true return as "must
			// persist" exactly like a crossing that did fire.
			return true
		}
		// R1 (invariants review, 2026-08-03): this block deliberately runs
		// BEFORE the deaths branch below. When a death and a foreign
		// crossing land in the same pass, the death's early return used to
		// skip capture entirely — the crossing's tier bit was already
		// marked, so a 15m-tier crossing (the top tier, with no later
		// escalation to rescue it) silently vanished: not fired, not held,
		// never re-generated. Capture-first makes the held set complete no
		// matter which notification leads.
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
		// While anchored, the airlock's no-leak rule applies to this
		// suffix too: eligibleWaits was tallied before the airlock
		// partition, so it would count waits being held silently. The
		// surviving (pierced) crossings are the only honest number.
		waitCount := eligibleWaits
		if s.anchor != nil {
			waitCount = len(crossings)
		}
		if waitCount > 0 {
			msg += fmt.Sprintf(" · %d waiting", waitCount)
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
	// Fleet "more waiting" context: while anchored, only count OTHER
	// eligible waits within the SAME (anchor) project — waits elsewhere are
	// airlocked, and leaking their count into a notification that DID pierce
	// would defeat the point of holding them silently in the first place.
	if s.anchor != nil {
		if others := len(crossings) - 1; others > 0 {
			msg += fmt.Sprintf(" · %d more waiting", others)
		}
	} else if others := eligibleWaits - 1; others > 0 {
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

// captureHeldWait appends or updates a HeldItem capturing a tier crossing
// the airlock suppressed (phase 3A). ID is stable per project
// ("wait-<project>") so a re-escalating wait updates the SAME entry rather
// than duplicating — the Title is refreshed to the new (higher) tier
// message but SinceTS is preserved from the item's FIRST capture, so the
// held set's age reflects when the airlock first caught it, not when it
// last escalated.
func captureHeldWait(s *state, c tierCrossing, now int64) {
	id := "wait-" + c.project
	msg := tiers[c.tierIdx].Message
	if c.kind == proto.WaitKindPermission {
		msg += " (permission)"
	}
	for i := range s.heldItems {
		if s.heldItems[i].ID == id {
			s.heldItems[i].Title = msg
			return
		}
	}
	s.heldItems = append(s.heldItems, proto.HeldItem{
		ID:      id,
		Kind:    "wait",
		Title:   msg,
		Project: c.project,
		SinceTS: now,
	})
}
