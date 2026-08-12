// Tests for the focus loop's sidebar half (phase 3C,
// docs/design/command-centre.md): the anchor row, the holding counter, and
// damped rendering while anchored. See focus.go for the implementation and
// frame.go's RenderWithOpts for the call sites.
package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// withFocus flips FocusEnabled for the duration of the test and restores it
// — mirrors withTheme in theme_rosepine_test.go.
func withFocus(t *testing.T, on bool) {
	t.Helper()
	prev := FocusEnabled
	FocusEnabled = on
	t.Cleanup(func() { FocusEnabled = prev })
}

// rawLineWithName returns the FIRST line of frame (ANSI intact) whose
// stripped text contains name. For a marked group's home+member pair this
// reliably returns the HOME/header row, since it renders before any
// "<name>/…" member line.
func rawLineWithName(frame []byte, name string) string {
	for _, l := range bytes.Split(frame, []byte("\n")) {
		if strings.Contains(stripAnsi(l), name) {
			return string(l)
		}
	}
	return ""
}

// The knob's whole job: an anchored, held-arrivals snapshot renders
// BYTE-IDENTICAL to the same snapshot with no Anchor/Held at all, as long as
// ZDEV_SIDEBAR_FOCUS is off. This must hold even once the daemon starts
// setting these fields — the loop wins by being picked, never by default.
func TestFocusKnobOffByteIdentical(t *testing.T) {
	withFocus(t, false)

	withAnchor := flatSnapshot()
	withAnchor.Anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "zdev", SinceTS: fixedNow - 60}
	withAnchor.Held = []proto.HeldItem{{ID: "p1", Kind: "parked", Title: "call back Sam", SinceTS: fixedNow}}

	plain := flatSnapshot()

	gotFrame, gotRows := RenderWithRows(withAnchor, 50, NewAnimator(), fixedNowFn)
	wantFrame, wantRows := RenderWithRows(plain, 50, NewAnimator(), fixedNowFn)

	if !bytes.Equal(gotFrame, wantFrame) {
		t.Errorf("knob off must ignore Anchor/Held entirely\ngot:  %q\nwant: %q", gotFrame, wantFrame)
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("row count changed with the knob off: got %d, want %d", len(gotRows), len(wantRows))
	}
	for i := range wantRows {
		if gotRows[i] != wantRows[i] {
			t.Errorf("row %d = %+v, want %+v", i, gotRows[i], wantRows[i])
		}
	}
}

// The anchor row is the frame's first line, "▶ now  <title> · <elapsed>",
// with the elapsed time derived from now - Anchor.SinceTS.
func TestAnchorRowRendersFirstWithElapsed(t *testing.T) {
	withFocus(t, true)

	snap := &proto.Snapshot{
		Anchor:   &proto.Anchor{Title: "IMP-97 validate deploy", SinceTS: fixedNow - 32*60},
		Projects: []proto.Project{{Name: "zdev", Status: "alive"}},
	}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatalf("empty frame")
	}
	if want := "▶ now  IMP-97 validate deploy · 32m"; lines[0] != want {
		t.Errorf("anchor row = %q, want %q", lines[0], want)
	}
}

// An auto-anchor (phase 3D, docs/design/command-centre.md — "the dwell
// auto-anchor") is encoded as a Title-naming convention with NO schema
// field (see zdevd/internal/hub/autoanchor.go's isAutoAnchor) — the
// renderer needs no code change at all, since it already draws Title
// verbatim. This test exists to VERIFY that at a representative sidebar
// width (50 cols) the "(auto)" suffix reads fine and never gets truncated
// away, which would silently misrepresent an inferred anchor as an
// explicit pick.
func TestAnchorRowRendersAutoSuffixAtStandardWidth(t *testing.T) {
	withFocus(t, true)

	snap := &proto.Snapshot{
		Anchor:   &proto.Anchor{Title: "example/backend (auto)", Project: "example/backend", SinceTS: fixedNow - 18*60},
		Projects: []proto.Project{{Name: "example/backend", Status: "alive"}},
	}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatalf("empty frame")
	}
	if want := "▶ now  example/backend (auto) · 18m"; lines[0] != want {
		t.Errorf("anchor row = %q, want %q", lines[0], want)
	}
}

// A scheduled anchor (design amendment, docs/design/command-centre.md —
// "The scheduled anchor and the push surface") is ALSO a v1 Title-naming
// convention with NO schema field (hub/scheduledanchor.go's
// isScheduledAnchor), same trick as the "(auto)" suffix above — the
// renderer needs no code change here either. Read-only: this just pins
// that the "(scheduled)" suffix reads fine and survives verbatim at the
// standard sidebar width.
func TestAnchorRowRendersScheduledSuffixAtStandardWidth(t *testing.T) {
	withFocus(t, true)

	snap := &proto.Snapshot{
		Anchor:   &proto.Anchor{Title: "IMP-97 stand-up (scheduled)", Project: "marketplace/pay-ops", SinceTS: fixedNow - 12*60},
		Projects: []proto.Project{{Name: "marketplace/pay-ops", Status: "alive"}},
	}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatalf("empty frame")
	}
	if want := "▶ now  IMP-97 stand-up (scheduled) · 12m"; lines[0] != want {
		t.Errorf("anchor row = %q, want %q", lines[0], want)
	}
}

// A title too long for the pane truncates with an ellipsis — the elapsed
// time is the one thing that must never be pushed off the edge.
func TestAnchorRowTitleTruncatesAtNarrowWidth(t *testing.T) {
	withFocus(t, true)

	snap := &proto.Snapshot{
		Anchor:   &proto.Anchor{Title: "a very long anchor title that will not fit in a narrow pane at all", SinceTS: fixedNow - 5},
		Projects: []proto.Project{{Name: "zdev", Status: "alive"}},
	}
	const width = 30
	out := stripAnsi(Render(snap, width, NewAnimator(), fixedNowFn))
	line := strings.Split(out, "\n")[0]
	if got := len([]rune(line)); got > width {
		t.Errorf("anchor row exceeds the width budget: %d runes (want <= %d): %q", got, width, line)
	}
	if !strings.Contains(line, "…") {
		t.Errorf("a too-long title must truncate with an ellipsis: %q", line)
	}
	if !strings.HasSuffix(line, "5s") {
		t.Errorf("elapsed must survive truncation (only the title should shrink): %q", line)
	}
}

// The holding counter is the ONLY thing gated on len(Held) > 0 — an anchor
// with nothing held draws no second line at all (no "holding 0").
func TestHoldingCounterOnlyWhenHeld(t *testing.T) {
	withFocus(t, true)

	base := func() *proto.Snapshot {
		return &proto.Snapshot{
			Anchor:   &proto.Anchor{Title: "x", SinceTS: fixedNow},
			Projects: []proto.Project{{Name: "zdev", Status: "alive"}},
		}
	}

	empty := base()
	out := stripAnsi(Render(empty, 50, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "holding") {
		t.Errorf("an empty held set must draw no counter:\n%s", out)
	}

	held := base()
	held.Held = []proto.HeldItem{
		{ID: "a", Kind: "parked", Title: "t1", SinceTS: fixedNow},
		{ID: "b", Kind: "wait", Title: "t2", SinceTS: fixedNow},
	}
	out2 := stripAnsi(Render(held, 50, NewAnimator(), fixedNowFn))
	lines := strings.Split(out2, "\n")
	// Hue-coded counter (calibration 2026-08-03): held WAITS get their own
	// accented count — one parked + one wait renders "┊ holding 2 · ●1".
	if len(lines) < 2 || lines[1] != "  ┊ holding 2 · ●1" {
		t.Errorf("holding counter must be \"  ┊ holding 2 · ●1\", got %+v", lines)
	}
	// And a parked-only held set stays a flat dim count — no accent.
	parkedOnly := base()
	parkedOnly.Held = []proto.HeldItem{{ID: "a", Kind: "parked", Title: "t1", SinceTS: fixedNow}}
	out3 := stripAnsi(Render(parkedOnly, 50, NewAnimator(), fixedNowFn))
	if !strings.Contains(out3, "┊ holding 1\n") || strings.Contains(out3, "●") {
		t.Errorf("parked-only counter must have no wait accent, got:\n%s", out3)
	}
}

// The anchor row claims a RowRef to its Project (a click jumps there, same
// as the design's "picking IS anchoring" boundary-review contract) — and
// claims NOTHING when Project is empty (a listless pick, e.g. a phone
// call).
func TestAnchorRowClaimsProjectOrNothing(t *testing.T) {
	withFocus(t, true)

	withProject := &proto.Snapshot{
		Anchor:   &proto.Anchor{Title: "IMP-97", Project: "zdev", SinceTS: fixedNow},
		Projects: []proto.Project{{Name: "zdev", Status: "alive"}},
	}
	_, rows := RenderWithRows(withProject, 50, NewAnimator(), fixedNowFn)
	if len(rows) == 0 || rows[0].Y != 0 || rows[0].Name != "zdev" {
		t.Errorf("anchor row must claim its project at Y=0, got %+v", rows)
	}

	noProject := &proto.Snapshot{
		Anchor:   &proto.Anchor{Title: "IMP-97", SinceTS: fixedNow}, // Project == ""
		Projects: []proto.Project{{Name: "zdev", Status: "alive"}},
	}
	_, rows2 := RenderWithRows(noProject, 50, NewAnimator(), fixedNowFn)
	for _, r := range rows2 {
		if r.Y == 0 {
			t.Errorf("a Project-less anchor must claim nothing, got %+v at Y=0", r)
		}
	}
}

// The anchor row + holding counter shift every project row down by exactly
// their own line count — proto.FlatRows/the daemon cursor are unaffected
// (renderer-only lines, same discipline as the triage strip); the row map
// must follow, and a folded drawer's header stays clickable throughout.
func TestFocusShiftsRowMapAndKeepsDrawerHeaderClickable(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	withFocus(t, true)

	snap := &proto.Snapshot{
		// Project == "" so the anchor row itself claims no RowRef — it
		// displays free-text Title, not a project/group name, which would
		// otherwise trip assertRowMapMatchesFrame's leaf/group content check.
		Anchor: &proto.Anchor{Title: "IMP-97 validate deploy", SinceTS: fixedNow - 60},
		Held:   []proto.HeldItem{{ID: "a", Kind: "parked", Title: "t", SinceTS: fixedNow}},
		Projects: []proto.Project{
			{Name: "projects/api", Status: "alive", Collapsed: true},
			{Name: "projects/web", Status: "alive", Collapsed: true},
			{Name: "zdev", Status: "alive"},
		},
	}
	frame, rows := RenderWithRows(snap, 50, NewAnimator(), fixedNowFn)
	assertRowMapMatchesFrame(t, frame, rows)
	if !hasTarget(rows, "projects/api") {
		t.Errorf("folded drawer header must still be clickable while anchored:\n%s", stripAnsi(frame))
	}

	FocusEnabled = false
	_, plainRows := RenderWithRows(snap, 50, NewAnimator(), fixedNowFn)
	FocusEnabled = true

	if got, want := yOf(rows, "zdev"), yOf(plainRows, "zdev")+2; got != want {
		t.Errorf("anchor row (1 line) + holding counter (1 line) must shift project rows down by 2: zdev at %d, want %d", got, want)
	}
}

// focusFleetSnapshot is an anchored fleet with every attention represented
// off the anchor: a fresh wait, a dead agent, an urgent-tier wait, and a
// working row that is NOT the anchor's project — the exact shape the torn-out
// damped mode used to mute.
func focusFleetSnapshot() *proto.Snapshot {
	return &proto.Snapshot{
		Anchor: &proto.Anchor{Title: "IMP-97 validate deploy", Project: "zdev", SinceTS: fixedNow - 60},
		Projects: []proto.Project{
			{Name: "zdev", Status: "shell-running", Attention: proto.AttWorking},
			{Name: "alpha", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: fixedNow - 5},
			{Name: "beta", Attention: proto.AttDead, WaitStartedTS: fixedNow - 500},
			{Name: "gamma", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: fixedNow - int64(WaitUrgentSec) - 5},
			{Name: "delta", Status: "shell-running", Attention: proto.AttWorking},
		},
	}
}

// Regression, dogfood 2026-08-06 ("pip is currently working, but showing a
// stalled spinner"): being anchored somewhere else must not mute a single
// row. Phase 3C's damped mode froze non-anchor markers, and a held spinner
// frame reads as a HUNG process, so the sidebar libelled healthy agents as
// stuck. The bug hid behind the anchor's own project, which pierced damping
// and animated correctly — the one row the operator spot-checked was always
// the one row that was right. Every row animates now; focus is won by making
// the anchor louder, never by making the fleet quieter.
func TestAnchoredFleetKeepsAnimatingEveryRow(t *testing.T) {
	withFocus(t, true)

	snap := focusFleetSnapshot()
	animA, animB := NewAnimator(), NewAnimator()
	for i := 0; i < 6; i++ {
		animB.Tick()
	}
	outA := Render(snap, 50, animA, fixedNowFn)
	outB := Render(snap, 50, animB, fixedNowFn)

	// "delta" is the reported case: working, not the anchor, no urgency to
	// earn it a pierce — under damping it froze. gamma (urgent) is NOT in
	// this list: since the 2026-08-09 urgency redesign an urgent row is a
	// deliberately static red ! + red name — a form, not a motion, so it
	// cannot read as a stalled spinner.
	for _, name := range []string{"delta", "alpha", "zdev"} {
		if a, b := rawLineWithName(outA, name), rawLineWithName(outB, name); a == b {
			t.Errorf("%s must keep animating while anchored elsewhere; frames were identical: %q", name, a)
		}
	}

	// And it animates through the real spinner, not a held frame.
	if line := stripAnsi([]byte(rawLineWithName(outA, "delta"))); !strings.ContainsAny(line, "◐◓◑◒") {
		t.Errorf("a non-anchor working row must carry a live spinner frame, got %q", line)
	}
	// Full working hue, never dimmed to peripheral vision.
	if line := rawLineWithName(outA, "delta"); !strings.Contains(line, thWorking()) {
		t.Errorf("a non-anchor working row must keep its working hue: %q", line)
	}
}
