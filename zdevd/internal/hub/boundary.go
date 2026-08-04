// internal/hub/boundary.go
//
// Boundary detection (phase 3A, docs/design/command-centre.md — "Boundaries"
// / "The anchor lifecycle"): the three ways an anchor ends. Two are PASSIVE
// and detected here, on the hub's existing tierCheck rhythm (the same 1Hz
// heartbeat driven by the supervisor's idempotent polls arming the
// debounce — see hub.go's publishPass): the anchored project's attention
// finishing, and the settable expiry elapsing. The third — an explicit
// "anchor clear" — is request-driven, not periodic, and is applied at its
// own call site in hub.go's Run loop (the anchorRequests branch); both paths
// share boundaryNotification for the message shape so it lives in one place.
//
// checkBoundary mutates state (clears s.anchor via applyEvent) and calls the
// injected `fire` function — the SAME kind of documented exception
// notify.go's tierCheck already established for WaitNotifiedTiers/
// DeadNotified (see that file's header comment, updated to mention this
// site too). Unlike tierCheck, checkBoundary does NOT short-circuit when
// fire is nil: clearing a finished/expired anchor is a meaningful state
// transition on its own, independent of whether a notification transport
// exists (ZDEV_NOTIFY=0 must not pin a stale anchor forever) — only the
// notification itself is skipped when there's no fire func to call.
package hub

import (
	"fmt"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// checkBoundary detects the anchor's two passive boundary conditions and
// clears the anchor + fires the boundary notification when one fires.
// Returns true if a boundary fired this pass.
//
// now is the current unix-second timestamp, threaded in (no time.Now() here)
// so callers share one deterministic value per pass — same discipline as
// tierCheck.
func checkBoundary(now int64, s *state, fire func(Notification)) bool {
	if s.anchor == nil {
		return false
	}
	a := s.anchor

	// Scheduled-anchor block end (design amendment, docs/design/
	// command-centre.md — "The scheduled anchor and the push surface"): the
	// block's Until passing is a boundary IF the anchor is STILL that same
	// scheduled anchor. The isScheduledAnchor guard is load-bearing for the
	// "one notification per block end" rule: an explicit override already
	// replaced s.anchor via a DIFFERENT code path (hub.go's anchorRequests
	// "set" branch), so by the time this runs the anchor is no longer
	// scheduled and this must not fire a second, redundant boundary for a
	// block whose anchor was already swapped out from under it.
	// scheduledAnchorUntil is stamped by checkScheduledAnchor at arm time —
	// a plain integer comparison here, no re-scan of the commitment set.
	if isScheduledAnchor(a) && s.scheduledAnchorUntil > 0 && now >= s.scheduledAnchorUntil {
		fireBoundary(now, s, a, fire)
		return true
	}

	// Anchored work finished: the anchor names a project and that
	// project's DISPLAYED attention (the dwell-debounced value buildSnapshot
	// just committed for this pass) is AttFinished — as an EDGE, not a
	// level. anchorFinishArmed is false while the project has been finished
	// SINCE anchor-set (anchoring onto finished work must not bounce inside
	// its own ack — invariants review R2); it arms the first pass the
	// project is seen in any other state, after which a fresh finish is a
	// real boundary.
	if a.Project != "" {
		if pd, ok := s.projectData[proto.SessionKey(a.Project)]; ok {
			if pd.Attention == proto.AttFinished {
				if s.anchorFinishArmed {
					// A finish on a SCHEDULED anchor consumes its block:
					// the work the block existed for is done, so the block
					// must not silently re-grab on this same pass (the
					// operator would hear "boundary" while the anchor
					// quietly persisted — the untested transition the
					// invariants review flagged). Presence or the next
					// block takes over by the normal rules.
					if isScheduledAnchor(a) {
						markScheduledOverridden(s, a)
					}
					fireBoundary(now, s, a, fire)
					return true
				}
			} else if !s.anchorFinishArmed {
				s.anchorFinishArmed = true
			}
		}
	}

	// Expiry: ZDEV_ANCHOR_EXPIRY_MIN resolved by cmd/zdevd into
	// s.anchorExpirySec (0 = never — the hub never reads the env itself).
	//
	// Phase 3E (docs/design/command-centre.md — "hook-informed focus",
	// mechanism 2 "idle-based expiry"): measured from s.lastEngagedTS, NOT
	// from a.SinceTS (the ORIGINAL pick time) as phase 3A shipped it. A
	// long, continuously-prompted session would otherwise expire on
	// schedule from the moment it was anchored regardless of how much real
	// engagement followed — exactly the "stale anchor teaches the operator
	// to ignore the tether" failure mode the design note's expiry exists
	// to prevent, just triggered by success (a long focused session) rather
	// than failure (an abandoned one). lastEngagedTS starts equal to
	// a.SinceTS at anchor-set (state.go's AnchorSet case) and is refreshed
	// by a prompt on the anchor's own project while attended
	// (autoanchor.go's handleWorkingSignal) — so a genuinely abandoned
	// anchor (no prompts after the operator wandered off) still expires on
	// schedule, measured from the LAST real engagement.
	// Scheduled anchors are EXEMPT from idle expiry: the block's Until IS
	// its expiry (checked above). Without this, a block longer than the
	// expiry window flooded boundaries at heartbeat rate — expiry fired,
	// the same-pass checkScheduledAnchor re-grabbed the block, engagement
	// snapped back to the block's START (SinceTS=At), and the next pass
	// fired again: nine notifications in ten seconds in the invariants
	// review's reproduction (2026-08-04).
	if s.anchorExpirySec > 0 && !isScheduledAnchor(a) && now-s.lastEngagedTS >= s.anchorExpirySec {
		fireBoundary(now, s, a, fire)
		return true
	}

	return false
}

// fireBoundary clears the anchor via applyEvent (so the mutation itself
// stays in the pure-function layer), force-restarts the dwell clock
// (autoanchor.go's resetDwellForCurrentAttendance — phase 3D's "re-arm
// hygiene": landing back in the same session right after ANY boundary must
// take a full fresh dwell before the auto-anchor can retrigger, never an
// instant re-anchor), and fires the boundary notification. The held set is
// left untouched — the boundary review (a later phase) consumes it
// item-by-item; the daemon never auto-opens anything on its own here
// (deliberate v1 choice: the daemon stays passive, the notification IS the
// invitation — see command-centre.md "Boundary"). Shared by checkBoundary's
// two passive causes AND autoanchor.go's checkAutoAnchorAway — every
// boundary that clears an anchor funnels through here, so the dwell reset
// applies uniformly regardless of which cause fired or which anchor kind
// (auto or explicit) it cleared.
func fireBoundary(now int64, s *state, a *proto.Anchor, fire func(Notification)) {
	held := len(s.heldItems)
	applyEvent(s, tmuxctl.AnchorClear{}, nil)
	resetDwellForCurrentAttendance(s, now)
	// Scheduled bookkeeping never outlives the anchor it described —
	// regardless of which boundary cause ended it (invariants review,
	// 2026-08-04). Harmless no-op for auto/explicit anchors.
	clearScheduledBookkeeping(s)
	if fire == nil {
		return
	}
	fire(boundaryNotification(a, held))
}

// boundaryNotification builds the ONE notification a boundary fires. It
// respects the global mute (ResolveNotifier's wrapper gates every fire on
// MutePath) but deliberately NOT presence-suppression: a boundary is the
// moment the operator explicitly wants to hear from zdev again, unlike a
// routine wait tier the airlock would otherwise hold.
func boundaryNotification(a *proto.Anchor, held int) Notification {
	return Notification{
		Project: a.Project,
		Message: fmt.Sprintf("boundary: %s — %d held", a.Title, held),
		Kind:    "boundary",
	}
}
