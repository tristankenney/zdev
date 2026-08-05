// internal/hub/scheduledanchor.go
//
// The scheduled anchor (design amendment to docs/design/command-centre.md,
// recorded there as "The scheduled anchor and the push surface", extending
// "The anchor lifecycle" / "Calibration: the tether and the shield"). The
// operator's requirement: the anchor should be able to derive from the run
// sheet/calendar implications, not just presence — a run-sheet block that
// says "10:00-11:00 marketplace/pay-ops" should anchor the operator to that
// project for that window, the same way dwelling in a session or a
// scheduled meeting already earns a tether.
//
// The tier order this extends (calibration section's philosophy): explicit
// > scheduled > presence (prompt/dwell). A scheduled anchor is EARNED
// EVIDENCE one rung above inferred presence — the operator (or a skill
// acting on their behalf, via /plan) put it on the run sheet — but still
// below a deliberate in-the-moment pick, which always wins. Shield posture
// mirrors the auto-anchor's calibration exactly: tether-only, full
// notifications — a run-sheet block earns context, not silence (the deep
// shield remains opt-in via M-,/pick/plan-explicit).
//
// Kind-convention hack (v1, NO schema bump, own this loudly): proto.Commitment
// has no Project field and may not gain one outside a natural schema bump
// (the brief's explicit constraint). anchor-eligibility and the project
// mapping BOTH ride the existing free-form Kind string via a "task:"
// prefix: Kind == "task:<project>" (e.g. "task:marketplace/pay-ops") is
// anchor-eligible with Project == the suffix; plain "task" (no colon/
// mapping) is a valid kind but never anchor-eligible; every other kind
// ("meeting", "focus", …) is never eligible either. This is the SAME kind
// of documented hack as autoanchor.go's Title convention for "(auto)" —
// the wire needs no change, and a real Kind/Project split waits for the
// next natural proto bump.
package hub

import (
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// scheduledAnchorSuffix is the v1 Title-convention marker for a scheduled
// anchor, mirroring autoanchor.go's autoAnchorSuffix but with its own
// distinct text so the two conventions can never collide: a scheduled
// anchor's Title is "<commitment title> (scheduled)", NOT
// "<project> (scheduled)" — the commitment's own title is what the
// operator put on the run sheet, and is usually more specific than the
// bare project name (e.g. "IMP-97 stand-up (scheduled)" rather than
// "marketplace/pay-ops (scheduled)").
const scheduledAnchorSuffix = " (scheduled)"

// schedulableKindPrefix is the Kind-convention prefix marking a commitment
// as anchor-eligible (see file header). A Kind of exactly this prefix with
// nothing after it ("task:") is NOT eligible — there is no project to map
// to — same as plain "task" with no colon at all.
const schedulableKindPrefix = "task:"

// isSchedulable reports whether c is anchor-eligible per the Kind
// convention: Kind starts with "task:" and names a non-empty project.
func isSchedulable(c proto.Commitment) bool {
	return strings.HasPrefix(c.Kind, schedulableKindPrefix) && len(c.Kind) > len(schedulableKindPrefix)
}

// schedulableProject extracts the project mapping from an eligible
// commitment's Kind ("task:marketplace/pay-ops" → "marketplace/pay-ops").
// Callers must have already confirmed isSchedulable(c) — this does not
// re-check the prefix.
func schedulableProject(kind string) string {
	return strings.TrimPrefix(kind, schedulableKindPrefix)
}

// isScheduledAnchor reports whether a is a scheduled anchor per the Title
// convention: Title ends with " (scheduled)". nil is never a scheduled
// anchor. Unlike isAutoAnchor, this does NOT also require Title to equal
// Project+suffix — a scheduled anchor's Title is the commitment's own
// title, which has no fixed relationship to Project. The (near-zero-
// probability) false positive — an operator hand-typing a title that
// happens to end in exactly " (scheduled)" — is accepted per the same
// "own this hack in a comment" discipline autoanchor.go's isAutoAnchor
// documents for its own convention: it would only ever cost that one
// anchor a persistence write and a spurious eligibility toward the
// airlock's "scheduled = notifications speak" gate, never a wrong CLAIM
// about a run-sheet commitment that doesn't exist.
func isScheduledAnchor(a *proto.Anchor) bool {
	return a != nil && strings.HasSuffix(a.Title, scheduledAnchorSuffix)
}

// isExplicitAnchor reports whether a is neither presence-derived (auto)
// nor run-sheet-derived (scheduled) — i.e. it was actually PICKED, whether
// by hand, a boundary review, or /plan's explicit anchor-set. This is the
// airlock's gate as of the design amendment: "the anchor is a tether, not
// a wall" applies to auto AND scheduled alike (both tether-not-shield);
// only an explicit anchor earns the deep shield (full airlock, silence).
func isExplicitAnchor(a *proto.Anchor) bool {
	return a != nil && !isAutoAnchor(a) && !isScheduledAnchor(a)
}

// activeSchedulableCommitment returns the FIRST anchor-eligible commitment
// (chronological order) whose [At, commitmentEnd) window contains now, or
// ok=false when none does. commitments MUST already be chronological —
// buildSnapshot's mergedCommitments output, which is what hub.go's
// publishPass passes in (it already built this pass's merged snapshot
// before calling checkScheduledAnchor, so there is nothing to re-merge
// here). Ties (two eligible commitments both covering now — an unlikely
// double-booked run sheet) resolve to whichever sorts first; this tier
// does not try to adjudicate a scheduling conflict, only reflect one
// consistently.
func activeSchedulableCommitment(commitments []proto.Commitment, now int64) (proto.Commitment, bool) {
	for _, c := range commitments {
		if !isSchedulable(c) {
			continue
		}
		if now >= c.At && now < commitmentEnd(c) {
			return c, true
		}
	}
	return proto.Commitment{}, false
}

// checkScheduledAnchor implements the scheduled-anchor tier's derivation,
// called on the SAME publishPass heartbeat as checkBoundary/
// checkAutoAnchorAway/checkAutoAnchorArm (hub.go) — inserted between
// checkBoundary and checkAutoAnchorAway so a block that just started wins
// the pass over the presence tier, and so a boundary that just cleared the
// previous scheduled anchor (block end) can be immediately superseded by
// the next block in the SAME pass (the design's explicit "back-to-back"
// allowance — one boundary notification for the ended block, then a fresh
// scheduled anchor for the next, both in one heartbeat).
//
// Tier discipline, one guard: `s.anchor != nil && !isAutoAnchor(s.anchor)`
// refuses to touch EITHER an explicit anchor (isAutoAnchor false, guard
// fires) or an ALREADY-scheduled anchor (isScheduledAnchor is also not
// isAutoAnchor, so the same guard fires) — a scheduled anchor never
// overrides an explicit one, and never redundantly re-derives itself every
// pass (no repeated SinceTS resets while sitting inside the same block).
// It DOES override an auto-anchor silently (no fire() call here, same as
// checkAutoAnchorArm/tryInstantAnchor never firing one) — "the plan
// outranks inferred presence," and overriding a tether is not a boundary
// any more than an explicit pick overriding one is.
//
// scheduledOverriddenBlocks pins the "once explicitly overridden, that
// block never re-anchors" semantics: if the eligible commitment's ID was
// marked (hub.go's anchorRequests "set" branch, when an explicit pick
// replaces a scheduled anchor), this refuses to re-grab that SPECIFIC
// block for the rest of its window even though s.anchor may be nil again
// (the operator cleared or let the override expire).
//
// SinceTS is stamped to the commitment's OWN start (c.At), not `now` — so
// the sidebar's elapsed time reads as time-into-block even when the
// anchor arms mid-block (e.g. after a prior anchor's boundary just
// cleared the way), matching the run sheet rather than "since zdev
// noticed." Returns true when it (re-)armed this pass.
func checkScheduledAnchor(now int64, s *state, commitments []proto.Commitment) bool {
	if s.anchor != nil && !isAutoAnchor(s.anchor) {
		return false
	}
	c, ok := activeSchedulableCommitment(commitments, now)
	if !ok {
		return false
	}
	if _, blocked := s.scheduledOverriddenBlocks[c.ID]; blocked {
		return false
	}
	applyEvent(s, tmuxctl.AnchorSet{
		Title:    c.Title + scheduledAnchorSuffix,
		Project:  schedulableProject(c.Kind),
		NowNanos: c.At * int64(time.Second),
	}, nil)
	s.scheduledAnchorCommitmentID = c.ID
	s.scheduledAnchorUntil = commitmentEnd(c)
	return true
}

// markScheduledOverridden marks prev's governing commitment (if prev was a
// scheduled anchor) as permanently overridden for the rest of its window.
// Called from hub.go's anchorRequests branch on BOTH the "set" path (an
// explicit pick REPLACES the scheduled anchor) and the "clear" path (an
// explicit release of it): either is a deliberate operator act overriding
// the tier's own choice for that specific block, and the design's pinned
// semantics are that neither should let the SAME block silently re-grab
// the anchor once `now` is back inside its window — impossible to observe
// after "set" (something else is anchored for the rest of the block
// regardless), but very real after "clear", which leaves s.anchor nil
// while `now` may still be inside [At, Until). No-op when prev isn't a
// scheduled anchor at all (an override of an explicit or auto anchor marks
// nothing — there is no run-sheet block to protect).
func markScheduledOverridden(s *state, prev *proto.Anchor) {
	if !isScheduledAnchor(prev) {
		return
	}
	if s.scheduledOverriddenBlocks == nil {
		s.scheduledOverriddenBlocks = make(map[string]struct{})
	}
	s.scheduledOverriddenBlocks[s.scheduledAnchorCommitmentID] = struct{}{}
	// The bookkeeping dies with the anchor it described (invariants
	// review, 2026-08-04): left stale, an operator's EXPLICIT anchor whose
	// hand-typed title happened to end "(scheduled)" would be judged
	// against a long-ended block's Until — spuriously cleared with a
	// boundary — and a later override would mark the WRONG block.
	clearScheduledBookkeeping(s)
}

// clearScheduledBookkeeping zeroes the fields that describe WHICH block the
// current scheduled anchor came from. Call whenever the scheduled anchor
// stops being the anchor (override, clear, or its own boundary) — the
// fields must never outlive the anchor they describe.
func clearScheduledBookkeeping(s *state) {
	s.scheduledAnchorCommitmentID = ""
	s.scheduledAnchorUntil = 0
}
