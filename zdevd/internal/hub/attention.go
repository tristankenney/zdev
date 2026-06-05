// internal/hub/attention.go
//
// Pure per-session UX-attention derivation. Single source of truth for
// "what should the sidebar show for this session right now". Replaces the
// scattered decision logic in recomputeAgents (state mutator switch),
// snapshot.go (stale-waiting demoter), and MarkerFor (renderer fan-out
// across Status/AgentClaude/AgentPi).
//
// The function is pure: all inputs are passed explicitly (no time.Now,
// no map lookups). Tests are table-driven and enumerable, which is the
// whole point of the refactor — every transition is one row in the table.
package hub

import (
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// AttentionInputs is the set of observables for one session at one
// moment. Construct via collectAttentionInputs from state.
type AttentionInputs struct {
	// Titles is the current title of every pane in the session.
	Titles []string

	// LastVisitTS is the unix-second stamp at which the user was last
	// observed attending this session (ClientSessionChanged or the
	// 2-second list-clients refresh). 0 means never visited.
	LastVisitTS int64

	// LastTitleChangeTS is the unix-second stamp at which any pane title
	// in this session most recently changed. Used by the stale-waiting
	// demoter so a "✳ <task>" title that claude left behind doesn't pin
	// the chip forever after a visit. 0 means no title event has been
	// observed (e.g., the session was discovered by SessionsList but
	// PaneTitleChanged hasn't fired yet).
	LastTitleChangeTS int64

	// WaitStartedTS is the previously-stamped wait-start timestamp,
	// threaded through so a fresh entry into waiting stamps `now` and
	// subsequent passes keep the original entry time stable for the
	// wait-age display and tier-escalation.
	WaitStartedTS int64

	// PrevAttention is the last derived attention for this session.
	// Drives the latch path: a transition out of AttWaiting before the
	// user has visited keeps the session pinned as AttWaiting so the
	// chip doesn't silently drop.
	PrevAttention proto.Attention
}

// AttentionResult is the derived UX-state for one session.
type AttentionResult struct {
	Attention     proto.Attention
	WaitStartedTS int64 // 0 means clear; non-zero is the original entry time
}

// DeriveAttention computes the single attention output from a snapshot of
// inputs and the current wall-clock `now`. Pure — same inputs always
// produce the same output.
//
// State table (top-to-bottom, first match wins):
//
//	Inputs                                                       → Result
//	─────────────────────────────────────────────────────────────────────
//	title-waiting AND (visited AND visit >= title-change)        → Idle      (stale ✳ demoter — user has already seen it)
//	title-waiting                                                → Waiting   (fresh wait — stamp WaitStartedTS if zero)
//	prev=Waiting AND NOT visited-since-wait-start                → Waiting   (latch — agent self-exited before user noticed)
//	title-working                                                → Working
//	title-finished                                               → Finished
//	otherwise                                                    → Idle
//
// "visited" means LastVisitTS > 0 AND LastVisitTS >= WaitStartedTS (for
// the latch guard) or LastVisitTS >= LastTitleChangeTS (for the demoter).
// The two checks differ because the demoter is about "did the user see
// the current title" while the latch is about "did the user see the wait
// at all" — those are different time references.
func DeriveAttention(in AttentionInputs, now int64) AttentionResult {
	hasWait, hasWork, hasDone := classifyTitles(in.Titles)

	// Stale-waiting demoter. Claude Code (Sonnet 4.6 era) leaves a
	// "✳ <task>" title behind when it returns to its idle prompt. If
	// the user has visited the session since the title last moved, the
	// title is stale — treat as no-wait. A subsequent real wait will
	// change a title and re-elevate.
	if hasWait && in.LastVisitTS > 0 && in.LastVisitTS >= in.LastTitleChangeTS {
		hasWait = false
	}

	visitedSinceWait := in.LastVisitTS > 0 && in.LastVisitTS >= in.WaitStartedTS

	switch {
	case hasWait:
		ts := in.WaitStartedTS
		if ts == 0 {
			ts = now
		}
		return AttentionResult{Attention: proto.AttWaiting, WaitStartedTS: ts}

	case in.PrevAttention == proto.AttWaiting && !visitedSinceWait && in.WaitStartedTS > 0:
		// Latch — agent left waiting (or title went stale) before user
		// visited. Keep pulsing until the next visit; the next
		// derivation pass after that visit will fall through to one of
		// the lower cases and clear.
		return AttentionResult{Attention: proto.AttWaiting, WaitStartedTS: in.WaitStartedTS}

	case hasWork:
		return AttentionResult{Attention: proto.AttWorking, WaitStartedTS: 0}

	case hasDone:
		return AttentionResult{Attention: proto.AttFinished, WaitStartedTS: 0}

	default:
		return AttentionResult{Attention: proto.AttIdle, WaitStartedTS: 0}
	}
}

// applyDwell is the minimum-dwell debounce layered on top of DeriveAttention.
// It decides the DISPLAYED attention from the raw derived value, suppressing
// transitions that don't hold for at least dwellMS. The motivating case: a
// title that blips working→waiting→working inside 200ms shouldn't flash the
// "needs attention" state — nothing actually wanted attention.
//
// Inputs:
//   - committed:    the currently displayed attention.
//   - init:         whether any prior pass has run for this project. The first
//                   pass has no established status to protect, so it commits
//                   immediately (keeps the single-pass behavior tests rely on).
//   - derived:      this pass's raw DeriveAttention output.
//   - pendCand:     the candidate from the previous pass that is waiting out
//                   the dwell window (AttIdle/"" when none — disambiguated by
//                   pendSinceMS, since AttIdle is itself a valid candidate).
//   - pendSinceMS:  unix-ms the candidate was first seen; 0 means none pending.
//   - nowMS:        current unix-millisecond time.
//   - dwellMS:      the dwell window in milliseconds; <= 0 disables debouncing.
//
// Returns the new displayed attention plus the carried-forward pending
// candidate and its since-stamp (0 when nothing is pending).
//
// Behavior table (first match wins):
//
//	dwellMS <= 0                         → commit derived          (debounce disabled)
//	!init                                → commit derived          (cold start — nothing to protect)
//	derived == committed                 → keep committed, clear pending
//	derived != pendCand OR pendSinceMS=0 → keep committed, (re)start clock on derived
//	nowMS - pendSinceMS >= dwellMS        → commit derived          (held long enough)
//	otherwise                            → keep committed, hold pending
func applyDwell(
	committed proto.Attention,
	init bool,
	derived proto.Attention,
	pendCand proto.Attention,
	pendSinceMS int64,
	nowMS int64,
	dwellMS int64,
) (newCommitted, newPendCand proto.Attention, newPendSinceMS int64) {
	if dwellMS <= 0 || !init {
		return derived, proto.AttIdle, 0
	}
	if derived == committed {
		// Derived matches what we're showing — any in-flight candidate was a
		// flap that reverted. Drop it.
		return committed, proto.AttIdle, 0
	}
	if derived != pendCand || pendSinceMS == 0 {
		// First divergence from the displayed value, or the candidate changed
		// to a different target mid-window. (Re)start the dwell clock; keep
		// showing the established status until the new one proves it will hold.
		return committed, derived, nowMS
	}
	if nowMS-pendSinceMS >= dwellMS {
		// Candidate held continuously for the full window — promote it.
		return derived, proto.AttIdle, 0
	}
	// Still inside the window — keep showing the established status.
	return committed, pendCand, pendSinceMS
}

// classifyTitles reduces the set of pane titles to three booleans matching
// tmuxctl.ClassifyPaneTitle's four-status mapping (waiting/shell-running/
// finished/alive, where alive yields all-false).
func classifyTitles(titles []string) (hasWait, hasWork, hasDone bool) {
	for _, t := range titles {
		switch tmuxctl.ClassifyPaneTitle(t) {
		case tmuxctl.StatusWaiting:
			hasWait = true
		case tmuxctl.StatusShellRunning:
			hasWork = true
		case tmuxctl.StatusFinished:
			hasDone = true
		}
	}
	return
}

// AttentionToStatus maps the rich UX state back to the legacy four-value
// Status string the renderer's pre-refactor paths still consume. The
// mapping is direct: AttWaiting → "waiting", AttWorking →
// "shell-running" (the historical name), AttFinished → "finished",
// AttIdle → "alive". Used by snapshot.go to keep Status in sync with
// Attention while step-2 of the refactor migrates callers off Status.
func AttentionToStatus(a proto.Attention) string {
	switch a {
	case proto.AttWaiting:
		return tmuxctl.StatusWaiting
	case proto.AttWorking:
		return tmuxctl.StatusShellRunning
	case proto.AttFinished:
		return tmuxctl.StatusFinished
	default:
		return tmuxctl.StatusAlive
	}
}

