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
	if len(lines) < 2 || lines[1] != "  ┊ holding 2" {
		t.Errorf("holding counter must be the second line \"  ┊ holding 2\", got %+v", lines)
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

// focusDampSnapshot fixes the four cases damped mode must tell apart: the
// anchor's own project (full treatment), a quiet wait and a quiet working
// row (both receded), a dead agent and an urgent-tier wait (both FIRES —
// pierce with full color).
func focusDampSnapshot() *proto.Snapshot {
	return &proto.Snapshot{
		Anchor: &proto.Anchor{Title: "IMP-97 validate deploy", Project: "zdev", SinceTS: fixedNow - 60},
		Projects: []proto.Project{
			{Name: "zdev", Status: "shell-running", Attention: proto.AttWorking},
			{Name: "alpha", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: fixedNow - 5},
			{Name: "beta", Attention: proto.AttDead, WaitStartedTS: fixedNow - 500},
			{Name: "gamma", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: fixedNow - int64(WaitUrgentSec) - 5},
		},
	}
}

// Quiet rows recede (marker+name dim, frozen glyph); the anchor's project
// and the FIRES list (dead, urgent wait) keep full color.
func TestDampedModeRecedesQuietRowsAndFiresKeepColor(t *testing.T) {
	withFocus(t, true)

	out := Render(focusDampSnapshot(), 50, NewAnimator(), fixedNowFn)

	alpha := rawLineWithName(out, "alpha")
	if !strings.Contains(alpha, Dim) {
		t.Errorf("receded row (alpha, fresh wait) must carry Dim, got %q", alpha)
	}
	if !strings.Contains(stripAnsi([]byte(alpha)), "·") {
		t.Errorf("receded waiting row's marker must freeze to the resting glyph ·, got %q", alpha)
	}

	beta := rawLineWithName(out, "beta")
	if strings.Contains(beta, Dim) {
		t.Errorf("dead row (beta) is a FIRE — must not be dimmed, got %q", beta)
	}
	if !strings.Contains(beta, RedPulse) {
		t.Errorf("dead row (beta) must keep its full RedPulse color, got %q", beta)
	}

	gamma := rawLineWithName(out, "gamma")
	if strings.Contains(gamma, Dim) {
		t.Errorf("urgent-tier wait (gamma) is a FIRE — must not be dimmed, got %q", gamma)
	}
	if !strings.Contains(gamma, RedPulse) {
		t.Errorf("urgent-tier wait (gamma) must keep its full RedPulse color, got %q", gamma)
	}

	zdev := rawLineWithName(out, "zdev")
	if strings.Contains(zdev, Dim) {
		t.Errorf("the anchor's OWN project must keep full treatment, got %q", zdev)
	}
}

// The no-animation assertion: a receded row's line must be BYTE-IDENTICAL
// across two different animator states (the pulse/spinner frozen), while a
// piercing row (urgent wait) and the anchor's own project keep animating.
func TestDampedModeFreezesAnimationOnRecededRowsOnly(t *testing.T) {
	withFocus(t, true)

	snap := focusDampSnapshot()

	animA := NewAnimator()
	animB := NewAnimator()
	for i := 0; i < 6; i++ {
		animB.Tick()
	}

	outA := Render(snap, 50, animA, fixedNowFn)
	outB := Render(snap, 50, animB, fixedNowFn)

	if a, b := rawLineWithName(outA, "alpha"), rawLineWithName(outB, "alpha"); a != b {
		t.Errorf("receded row (alpha) must be byte-identical across animator states:\nA: %q\nB: %q", a, b)
	}
	if a, b := rawLineWithName(outA, "gamma"), rawLineWithName(outB, "gamma"); a == b {
		t.Errorf("piercing row (gamma, urgent) must keep animating; frames were identical: %q", a)
	}
	if a, b := rawLineWithName(outA, "zdev"), rawLineWithName(outB, "zdev"); a == b {
		t.Errorf("the anchor's own project must keep animating; frames were identical: %q", a)
	}
}

// Hover is feedback, not attention — it must survive damping. A receded row
// under the pointer still shows the › marker.
func TestFocusHoverStillShowsOnRecededRow(t *testing.T) {
	withFocus(t, true)

	out, _ := RenderWithOpts(focusDampSnapshot(), 50, NewAnimator(), fixedNowFn, RenderOpts{Hover: "alpha"})
	line := rawLineWithName(out, "alpha")
	if !strings.HasPrefix(stripAnsi([]byte(line)), "›") {
		t.Errorf("hovering a receded row must still show the › marker, got %q", stripAnsi([]byte(line)))
	}
}

// Group headers keep their STRUCTURE (glyph, Bold, rollup) but lose their
// identity hue when nothing inside the group is the anchor or a fire —
// tested directly against writeGroupHeader/renderHomeRow rather than a full
// Render, since the header's own receded flag is the GROUP's aggregate, not
// the header row's individual state.
func TestWriteGroupHeaderRecededLosesHueKeepsStructure(t *testing.T) {
	var full, dim bytes.Buffer
	writeGroupHeader(&full, "alpha", 50, 0, false, false)
	writeGroupHeader(&dim, "alpha", 50, 0, false, true)

	if !strings.Contains(full.String(), PaletteFor("alpha")) {
		t.Errorf("non-receded header must carry its identity hue, got %q", full.String())
	}
	if strings.Contains(dim.String(), PaletteFor("alpha")) {
		t.Errorf("receded header must NOT carry its identity hue, got %q", dim.String())
	}
	if !strings.Contains(dim.String(), thDim()) {
		t.Errorf("receded header must carry thDim(), got %q", dim.String())
	}
	// Structure (the ╭ corner, the name, Bold) survives regardless.
	for _, out := range []string{full.String(), dim.String()} {
		if !strings.Contains(out, "╭") || !strings.Contains(out, "alpha") || !strings.Contains(out, Bold) {
			t.Errorf("receded/full headers must both keep structure (╭, name, Bold): %q", out)
		}
	}
}

func TestRenderHomeRowRecededLosesHueKeepsStructure(t *testing.T) {
	p := &proto.Project{Name: "alpha", Status: "alive"}
	anim := NewAnimator()

	var full, dim bytes.Buffer
	renderHomeRow(&full, p, 50, anim, fixedNowFn, false, false, false, false, 0, false)
	renderHomeRow(&dim, p, 50, anim, fixedNowFn, false, false, false, true, 0, false)

	if !strings.Contains(full.String(), PaletteFor("alpha")) {
		t.Errorf("non-receded home row must carry its identity hue, got %q", full.String())
	}
	if strings.Contains(dim.String(), PaletteFor("alpha")) {
		t.Errorf("receded home row must NOT carry its identity hue, got %q", dim.String())
	}
	if !strings.Contains(dim.String(), "╭") || !strings.Contains(dim.String(), "alpha") {
		t.Errorf("receded home row must keep its structure (╭, name): %q", dim.String())
	}
}

// Integration-level check that the per-group aggregate (frame.go's
// groupFull) actually drives the header through a full Render: a quiet
// group (nothing inside is the anchor or a fire) dims; a group holding the
// anchor's own project keeps its hue even though the header ROW itself
// isn't the anchor.
func TestDampedGroupHeaderAggregate(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	withFocus(t, true)

	quiet := &proto.Snapshot{
		Anchor: &proto.Anchor{Title: "x", Project: "zdev", SinceTS: fixedNow},
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
			{Name: "alpha/repo", Status: "alive"},
			{Name: "zdev", Status: "alive"},
		},
	}
	out := Render(quiet, 50, NewAnimator(), fixedNowFn)
	header := rawLineWithName(out, "alpha")
	if !strings.Contains(header, Dim) {
		t.Errorf("a quiet group's header must lose its hue while anchored elsewhere: %q", header)
	}

	anchoredInside := &proto.Snapshot{
		Anchor: &proto.Anchor{Title: "x", Project: "alpha/repo", SinceTS: fixedNow},
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
			{Name: "alpha/repo", Status: "alive"},
		},
	}
	out2 := Render(anchoredInside, 50, NewAnimator(), fixedNowFn)
	header2 := rawLineWithName(out2, "alpha")
	if !strings.Contains(header2, PaletteFor("alpha")) {
		t.Errorf("a group holding the anchor's project must keep its header hue: %q", header2)
	}
}
