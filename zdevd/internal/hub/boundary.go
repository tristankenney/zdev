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

	// Anchored work finished: the anchor names a project and that
	// project's DISPLAYED attention (the dwell-debounced value buildSnapshot
	// just committed for this pass) is AttFinished.
	if a.Project != "" {
		if pd, ok := s.projectData[proto.SessionKey(a.Project)]; ok && pd.Attention == proto.AttFinished {
			fireBoundary(s, a, fire)
			return true
		}
	}

	// Expiry: ZDEV_ANCHOR_EXPIRY_MIN resolved by cmd/zdevd into
	// s.anchorExpirySec (0 = never — the hub never reads the env itself).
	if s.anchorExpirySec > 0 && now-a.SinceTS >= s.anchorExpirySec {
		fireBoundary(s, a, fire)
		return true
	}

	return false
}

// fireBoundary clears the anchor via applyEvent (so the mutation itself
// stays in the pure-function layer) and fires the boundary notification.
// The held set is left untouched — the boundary review (a later phase)
// consumes it item-by-item; the daemon never auto-opens anything on its
// own here (deliberate v1 choice: the daemon stays passive, the
// notification IS the invitation — see command-centre.md "Boundary").
func fireBoundary(s *state, a *proto.Anchor, fire func(Notification)) {
	held := len(s.heldItems)
	applyEvent(s, tmuxctl.AnchorClear{}, nil)
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
