// internal/hub/autoanchor.go
//
// The dwell auto-anchor (phase 3D, docs/design/command-centre.md — "the
// dwell auto-anchor" under "The anchor lifecycle"): the loop's ambient
// entry point. The explicit anchor solved trust but reintroduced ceremony,
// and ceremony gets skipped; zdev already tracks session attendance as
// FACT, not inference, so continuous dwell in one managed project session
// past a threshold auto-anchors to it — visibly marked "(auto)", full
// airlock engaged. Runs on the SAME publishPass heartbeat that drives
// checkBoundary (hub.go) — no new goroutine; every function here is pure
// with `now` threaded, same discipline as boundary.go/notify.go.
//
// Title-convention encoding (v1, NO schema bump): proto.Anchor carries only
// Title/Project/SinceTS (see proto.go), so an auto-anchor is encoded as a
// naming convention rather than a new wire field — Title is exactly
// "<Project> (auto)" (see isAutoAnchor/autoAnchorSuffix below). This is a
// deliberate, DOCUMENTED hack: a proper Kind/IsAuto field waits for the
// next natural proto schema bump. persist.go's saveState (the OTHER site
// that must distinguish auto from explicit — auto-anchors are never
// persisted) reuses this exact same convention rather than inventing a
// second one.
//
// Phase 3E (docs/design/command-centre.md — "hook-informed focus") adds a
// SECOND arming path onto this same machinery: handleWorkingSignal, called
// from applyEvent's NotifSeen case (state.go) rather than from the
// publishPass heartbeat, because its whole point is "instant" — no dwell
// wait. It reuses isAutoAnchor's Title convention (an instant-anchor IS a
// dwell-auto-anchor by another arming path, not a third kind) and the same
// applyEvent(AnchorSet) call checkAutoAnchorArm makes, so persistence
// exemption, away-boundary, re-arm hygiene, and the airlock all apply to it
// for free. It is gated on the SAME s.autoAnchorMinSec>0 master switch as
// the dwell path (see handleWorkingSignal) — one knob disables auto-
// anchoring by either arming path, rather than adding a second knob for a
// mechanism that is, at its core, the same feature.
package hub

import (
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// autoAnchorSuffix is the v1 Title-convention marker. See file header.
const autoAnchorSuffix = " (auto)"

// isAutoAnchor reports whether a is a dwell auto-anchor per the Title
// convention: Title == Project + " (auto)" with a non-empty Project (an
// auto-anchor always names the project it dwelled in — there is no such
// thing as a listless auto-anchor, since nothing to derive a project from
// means nothing armed in the first place). nil is never an auto-anchor.
//
// The (near-zero-probability) false positive — an operator hand-typing
// exactly "<project> (auto)" as an explicit title for that same project —
// is accepted per the brief's "own this hack in a comment": it would only
// ever cost that one anchor a persistence write and an away-boundary exit
// it didn't ask for, never a wrong CLAIM about presence.
func isAutoAnchor(a *proto.Anchor) bool {
	return a != nil && a.Project != "" && a.Title == a.Project+autoAnchorSuffix
}

// soleAttendedManagedProject returns the canonical name of the ONE managed
// project every attached tmux client is currently viewing, or "" when: no
// client is attached, attached clients disagree (viewing different
// sessions — "continuously attended" requires unanimity, not majority),
// or the attended session doesn't correspond to a project in the
// workspace's project list (an unmanaged tmux session, or the daemon's own
// watcher/synthetic sessions — already filtered out of s.clientSessions at
// the applyEvent layer, so no extra guard is needed for those here).
func soleAttendedManagedProject(s *state) string {
	var dashName string
	for _, sess := range s.clientSessions {
		switch {
		case dashName == "":
			dashName = sess
		case dashName != sess:
			return "" // two clients disagree — no sole attendee
		}
	}
	if dashName == "" {
		return ""
	}
	for _, n := range s.projectListNames {
		if proto.SessionKey(n) == dashName {
			return n
		}
	}
	return "" // attended session isn't a managed project
}

// updateDwell tracks continuous attendance of a single managed project.
// Called unconditionally every publishPass — regardless of whether an
// anchor is currently set — because attendance must be tracked
// continuously so the dwell clock is accurate the instant a boundary
// clears the way for arming; checkAutoAnchorArm is the only consumer that
// cares about the anchored/unanchored distinction.
//
// A CHANGE in the sole-attended project (any hop, including to/from no
// session at all) restarts the clock at `now`; holding the SAME project
// (including the same "no sole attendee" empty string) leaves
// dwellSinceTS untouched. This is what makes a hop-interrupted dwell
// restart from zero: a session hop under the arm threshold changes
// dwellProject at least twice (away, then back), and each change restarts
// the clock — the accumulated time before the hop is not carried forward.
func updateDwell(s *state, now int64) {
	cur := soleAttendedManagedProject(s)
	if cur == s.dwellProject {
		return
	}
	s.dwellProject = cur
	if cur == "" {
		s.dwellSinceTS = 0
	} else {
		s.dwellSinceTS = now
	}
}

// resetDwellForCurrentAttendance force-restarts the dwell clock against
// whatever is attended RIGHT NOW, regardless of whether that matches
// s.dwellProject already — the "re-arm hygiene" rule (brief, "Semantics"):
// after ANY boundary (finish, expiry, away, or an explicit clear), landing
// back in the very same session must take a full fresh dwell before
// auto-anchoring again, never an instant re-anchor. Without this, a
// session already mid-dwell when the boundary fired would carry its
// accumulated dwell time straight through the boundary and re-arm on the
// very next pass — the exact boundary→instant-re-anchor oscillation the
// brief calls out.
//
// Called from fireBoundary (boundary.go, covers finish/expiry/away — every
// passive boundary funnels through it) and from hub.go's anchorRequests
// "clear" branch (the one boundary cause NOT applied inside checkBoundary).
func resetDwellForCurrentAttendance(s *state, now int64) {
	s.dwellProject = soleAttendedManagedProject(s)
	if s.dwellProject == "" {
		s.dwellSinceTS = 0
	} else {
		s.dwellSinceTS = now
	}
}

// checkAutoAnchorArm arms the dwell auto-anchor: while unanchored, once one
// managed project has been continuously attended for at least
// s.autoAnchorMinSec, sets Anchor{Title: "<project> (auto)", Project:
// <project>, SinceTS: now} via the SAME applyEvent path an explicit pick
// uses (tmuxctl.AnchorSet) — finish-arming, publish semantics, and (via
// persist.go's Title-convention check) selective persistence all come free
// from that one path; nothing here duplicates applyEvent's AnchorSet case.
//
// NEVER overrides an existing anchor of either kind: the guard on s.anchor
// != nil is checked here too (not just left to the publishPass call site)
// as the same defense-in-depth discipline applyEvent's own trimmed-title
// guard follows. autoAnchorMinSec <= 0 disables auto-anchoring entirely
// (mirrors anchorExpirySec's 0-means-disabled convention). Returns true
// when it armed this pass.
func checkAutoAnchorArm(now int64, s *state) bool {
	if s.anchor != nil {
		return false
	}
	if s.autoAnchorMinSec <= 0 {
		return false
	}
	// dwellProject == "" is the ONLY "nothing to arm from" signal —
	// dwellSinceTS == 0 is NOT a safe proxy for "unset" here, since 0 is
	// also a perfectly legitimate real dwell-start timestamp (unix epoch,
	// or simply `now == 0` in a test); conflating the two would refuse to
	// arm a dwell that genuinely started at t=0.
	if s.dwellProject == "" {
		return false
	}
	if now-s.dwellSinceTS < s.autoAnchorMinSec {
		return false
	}
	applyEvent(s, tmuxctl.AnchorSet{
		Title:    s.dwellProject + autoAnchorSuffix,
		Project:  s.dwellProject,
		NowNanos: now * int64(1e9),
	}, nil)
	return true
}

// checkAutoAnchorAway detects the auto-anchor's OWN boundary condition —
// the one exit an explicit anchor never carries ("switching sessions does
// not move the anchor" stays true for a pick; only the ambient auto-anchor
// exits on sustained absence). While auto-anchored, sustained absence from
// the anchored project — attending a different session, or none, for at
// least s.autoAnchorAwayMinSec CONTINUOUSLY — fires the boundary via
// fireBoundary (same clear + notification + dwell-reset path finish/expiry
// use). A brief hop under the threshold is wandering: the instant
// attendance returns, the absence timer resets to zero with no memory of
// the hop — exactly like an explicit anchor tolerates wandering, just with
// this one extra exit layered on top.
//
// Explicit anchors, and the case where checkBoundary already cleared the
// anchor earlier this same pass (finish/expiry), both no-op here via the
// isAutoAnchor guard — s.anchor is nil or not an auto-anchor in both cases.
// autoAnchorAwayMinSec <= 0 disables the away-boundary (mirrors the other
// two dwell knobs' 0-means-disabled convention) — an auto-anchor then only
// ever ends via finish/expiry/explicit-clear, same as an explicit one.
func checkAutoAnchorAway(now int64, s *state, fire func(Notification)) bool {
	if s.anchor == nil || !isAutoAnchor(s.anchor) {
		s.autoAwaySinceTS = 0
		return false
	}
	if s.autoAnchorAwayMinSec <= 0 {
		s.autoAwaySinceTS = 0
		return false
	}
	if isClientAttended(s, proto.SessionKey(s.anchor.Project)) {
		s.autoAwaySinceTS = 0
		return false
	}
	if s.autoAwaySinceTS == 0 {
		s.autoAwaySinceTS = now
	}
	if now-s.autoAwaySinceTS < s.autoAnchorAwayMinSec {
		return false // wandering — not yet sustained
	}
	a := s.anchor
	fireBoundary(now, s, a, fire)
	s.autoAwaySinceTS = 0
	return true
}

// handleWorkingSignal implements phase 3E's two hook-informed refinements
// on top of a NotifSeen carrying Kind == proto.WaitKindWorking, called
// inline from applyEvent's NotifSeen case (state.go) — NOT from the
// publishPass heartbeat, because mechanism 1 below is explicitly "instant":
// waiting for the next pass would reintroduce the ceremony-adjacent lag the
// dwell auto-anchor already accepts, but a real-time hook signal need not.
//
//  1. Instant anchor (mechanism 1): a prompt landing on an ATTENDED,
//     UNANCHORED managed project auto-anchors it immediately — same
//     Title convention, same applyEvent(AnchorSet) path, same
//     never-overrides guard as the dwell arm (checkAutoAnchorArm).
//  2. Engagement refresh (mechanism 2b): a prompt landing on the ANCHOR's
//     OWN project while attended refreshes state.lastEngagedTS, so a
//     deeply-worked anchor never spuriously expires (boundary.go's
//     expiry check reads lastEngagedTS, not the anchor's original
//     SinceTS). See lastEngagedTS's doc comment (state.go) for mechanism
//     2a (anchor-set itself refreshing engagement) — that half lives in
//     applyEvent's AnchorSet case since it's common to every arming path.
//
// Both branches share one gate, checked first and commented ONCE: ev.Src
// must be EXACTLY "prompt" — the hub's own extraction (bin/zdev-notify) of
// the hook payload's `.hook_event_name == "UserPromptSubmit"`. Anything
// else — "heartbeat" (a PreToolUse mid-turn signal, once one exists), the
// empty string (an OLDER zdev-notify that predates Src, or any non-Claude
// agent), or any unrecognized value — does NOTHING here. This is the
// brief's explicit, load-bearing requirement: "an agent grinding for an
// hour while the operator is at lunch is not engagement," and a background
// loop's own heartbeat must never be mistaken for the operator's hand on
// the keyboard. The empty-string case is ALSO the back-compat contract
// (tmuxctl.NotifSeen.Src's doc comment): a daemon fed the old three-line
// notif format never instant-anchors and never refreshes engagement from
// it — it behaves exactly as it did before phase 3E existed.
func handleWorkingSignal(s *state, ev tmuxctl.NotifSeen) {
	if ev.Src != "prompt" {
		return
	}
	if !isClientAttended(s, ev.Session) {
		return // "zdev run" / an orchestrator firing prompts unattended — never anchor or refresh from these
	}
	if s.anchor == nil {
		tryInstantAnchor(s, ev)
		return
	}
	// Engagement refresh (mechanism 2b): only when the prompt landed on
	// the ANCHOR's OWN project — a prompt elsewhere (a different attended
	// session while anchored) says nothing about whether the ANCHORED work
	// is still being engaged with, so it must not reset that clock.
	if s.anchor.Project != "" && proto.SessionKey(s.anchor.Project) == ev.Session {
		// Monotonic (invariants review, 2026-08-03): the notif watcher's
		// Chmod subscription can replay a STALE prompt file; taking its
		// old timestamp unconditionally would drag engagement backwards
		// and expire an actively-engaged anchor early. Same guard the
		// lastVisitTS stamping already uses.
		if ev.Timestamp > s.lastEngagedTS {
			s.lastEngagedTS = ev.Timestamp
		}
	}
}

// tryInstantAnchor is mechanism 1's arming step, split out of
// handleWorkingSignal for the same reason checkAutoAnchorArm is its own
// function: one guard chain, one applyEvent call, independently testable.
// Preconditions (attended, Src=="prompt") are the caller's job; this
// function only re-checks what IT owns:
//
//   - s.anchor == nil — never overrides an existing anchor, explicit or
//     auto, same discipline as checkAutoAnchorArm.
//   - s.autoAnchorMinSec > 0 — the SAME master switch that disables the
//     dwell path also disables the instant path (see file header): one
//     knob for one feature, not two.
//   - ev.Session resolves to a MANAGED project (mirrors
//     soleAttendedManagedProject's resolution) — an unmanaged/synthetic
//     session names nothing to anchor to.
//
// On arming, sets Anchor{Title: "<project> (auto)", Project: <project>,
// SinceTS: ev.Timestamp} via the identical tmuxctl.AnchorSet path
// checkAutoAnchorArm uses — ev.Timestamp (the hook's own wall-clock
// reading, already captured by zdev-notify at prompt-submit time) stands
// in for `now` here, the same way PaneTitleChanged's handling elsewhere in
// applyEvent uses an already-sampled timestamp rather than re-reading the
// clock. Returns true when it armed.
// instantAnchorFreshnessSec bounds how old a prompt's file timestamp may
// be, relative to the watcher's receive time, and still instant-anchor.
// The notif watcher subscribes Chmod (deliberately), so a spurious Chmod
// can REPLAY a stale prompt file; anchoring off hours-old evidence would
// set SinceTS in the past and expire into a boundary blip on the next
// pass. 60s is generous for any real hook→fsnotify→hub path.
const instantAnchorFreshnessSec = 60

func tryInstantAnchor(s *state, ev tmuxctl.NotifSeen) bool {
	if s.anchor != nil {
		return false
	}
	if s.autoAnchorMinSec <= 0 {
		return false
	}
	// Freshness (invariants review, 2026-08-03): a replayed stale file
	// must never anchor. ReceivedNanos==0 means "unknown" (direct-built
	// test events) and is treated as fresh.
	if ev.ReceivedNanos > 0 && ev.ReceivedNanos/1e9-ev.Timestamp > instantAnchorFreshnessSec {
		return false
	}
	var project string
	for _, n := range s.projectListNames {
		if proto.SessionKey(n) == ev.Session {
			project = n
			break
		}
	}
	if project == "" {
		return false // attended session isn't a managed project
	}
	applyEvent(s, tmuxctl.AnchorSet{
		Title:    project + autoAnchorSuffix,
		Project:  project,
		NowNanos: ev.Timestamp * int64(1e9),
	}, nil)
	return true
}
