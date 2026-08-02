package render

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// ansiRE strips SGR/cursor escapes so assertions match the VISIBLE text.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripAnsi(b []byte) string { return ansiRE.ReplaceAllString(string(b), "") }

// runeCol returns the rune (screen-column) index of r in s, or -1. Byte
// offsets are useless for column assertions here — the frame glyphs, the
// markers, and the status dots are all multibyte.
func runeCol(s string, r rune) int {
	for i, c := range []rune(s) {
		if c == r {
			return i
		}
	}
	return -1
}

func TestDisplayName(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)

	GroupMode = "prefix"
	cases := map[string]string{
		"marketplace/pay-app": "pay-app",
		"projects/pay-app":    "pay-app",
		"marketplace":         "marketplace",
		"zdev":                "zdev",
	}
	for name, want := range cases {
		if got := displayName(name); got != want {
			t.Errorf("displayName(%q) = %q, want %q", name, got, want)
		}
	}

	GroupMode = "off"
	if got := displayName("marketplace/pay-app"); got != "marketplace/pay-app" {
		t.Errorf("GroupMode=off must not strip names, got %q", got)
	}
}

// flatSnapshot mirrors the flat-root layout in ALPHA order — the tree
// mirrors the disk: marked group (home row + members), unmarked group
// (members only), singles interleaved.
func flatSnapshot() *proto.Snapshot {
	names := []string{
		"alpha", "alpha/pay-app", "alpha/pay-id",
		"dotfiles",
		"projects/onboarding", "projects/pay-app",
		"zdev",
	}
	snap := &proto.Snapshot{}
	for _, n := range names {
		snap.Projects = append(snap.Projects, proto.Project{Name: n, Status: "alive"})
	}
	return snap
}

func TestGroupedFrameFlat(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)

	GroupMode = "off"
	off := stripAnsi(Render(flatSnapshot(), 50, NewAnimator(), fixedNowFn))
	if strings.Contains(off, "╭ alpha") {
		t.Fatalf("GroupMode=off must not render group headers:\n%s", off)
	}

	GroupMode = "prefix"
	out := stripAnsi(Render(flatSnapshot(), 50, NewAnimator(), fixedNowFn))

	// Marked group: its bare row renders AS the header.
	if n := strings.Count(out, "╭ alpha"); n != 1 {
		t.Errorf("alpha home-as-header count = %d, want 1:\n%s", n, out)
	}
	// Unmarked group: synthetic header.
	if n := strings.Count(out, "╭ projects"); n != 1 {
		t.Errorf("projects synthetic header count = %d, want 1:\n%s", n, out)
	}
	// Members: leaf display, gutter, closer on the last.
	if !strings.Contains(out, "  │  · pay-app") || !strings.Contains(out, "  ╰  · pay-id") {
		t.Errorf("alpha members must gutter and close:\n%s", out)
	}
	if !strings.Contains(out, "  ╰  · pay-app") {
		t.Errorf("projects' last member closes the frame:\n%s", out)
	}
	// ALPHA order preserved — dotfiles sits BETWEEN the groups, no
	// separators anywhere.
	iAlpha := strings.Index(out, "╭ alpha")
	iDot := strings.Index(out, "· dotfiles")
	iProj := strings.Index(out, "╭ projects")
	iZdev := strings.Index(out, "· zdev")
	if !(iAlpha < iDot && iDot < iProj && iProj < iZdev) {
		t.Errorf("alpha order must interleave singles: a=%d d=%d p=%d z=%d:\n%s",
			iAlpha, iDot, iProj, iZdev, out)
	}
	if strings.Contains(out, "\n  ──────\n") {
		t.Errorf("no bare separators in the flat layout:\n%s", out)
	}
}

// Folded groups: ▸ + rollup; per-row pierce keeps active members visible.
func TestGroupedCollapseFlat(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "alpha", Status: "alive"},
		{Name: "alpha/repo-a", Status: "alive", Collapsed: true},
		{Name: "alpha/repo-b", Status: "alive", Collapsed: true},
		{Name: "beta", Status: "alive"},
		{Name: "beta/repo-c", Status: "alive"},
		{Name: "projects/pay-app", Status: "alive", Collapsed: true},
		{Name: "projects/pay-id", Status: "shell-running", Attention: proto.AttWorking},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "repo-a") || strings.Contains(out, "repo-b") {
		t.Errorf("folded members must not render:\n%s", out)
	}
	if !strings.Contains(out, "▸ alpha ·2") {
		t.Errorf("fully folded marked group: ▸ + rollup:\n%s", out)
	}
	if !strings.Contains(out, "╭ beta") || !strings.Contains(out, "  ╰  · repo-c") {
		t.Errorf("open group renders frame + members:\n%s", out)
	}
	// projects: partially folded — working member visible, header keeps ╭
	// (a frame exists), rollup counts the hidden one.
	if !strings.Contains(out, "╭ projects ·1") {
		t.Errorf("partially folded unmarked group: ╭ + rollup:\n%s", out)
	}
	if !strings.Contains(out, "pay-id") || strings.Contains(out, "· pay-app") {
		t.Errorf("working member visible, quiet member hidden:\n%s", out)
	}
}

// The current session inside a group keeps the frame, with its ▌ marker in
// the LEFT MARGIN ahead of the gutter — not fused to the frame glyph.
func TestGroupedCurrentMemberGutter(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	snap := &proto.Snapshot{
		CurrentSession: "projects/pay-id",
		Projects: []proto.Project{
			{Name: "projects/pay-app", Status: "alive"},
			{Name: "projects/pay-id", Status: "alive", Branch: "main"},
			{Name: "projects/pay-ops", Status: "alive"},
		},
	}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if !strings.Contains(out, "▌ │") {
		t.Errorf("current member row must carry ▌ in the margin BEFORE the gutter:\n%s", out)
	}
	if strings.Contains(out, "│▌") {
		t.Errorf("▌ must never sit after the frame glyph (reads as one fused glyph):\n%s", out)
	}
}

// The selection/presence marker column must NOT depend on group membership:
// ▌ and ▶ always start at column 0, whether the row is a group member, a
// group home, or an ungrouped single. Before the flat-root marker fix the
// marker sat after the frame glyph on member rows, so it jumped between
// column 0 and column 3 as the cursor crossed a group boundary.
func TestMarkerColumnIsStableAcrossGroups(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"

	markedRow := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "▌") || strings.HasPrefix(l, "▶") {
				return l
			}
			if i := strings.IndexAny(l, "▌▶"); i >= 0 {
				t.Errorf("marker at column %d, want 0: %q", i, l)
				return l
			}
		}
		t.Errorf("no marked row rendered:\n%s", out)
		return ""
	}

	// Current session (▌) at each structural position.
	for _, cur := range []string{"alpha", "alpha/pay-app", "projects/onboarding", "zdev"} {
		snap := flatSnapshot()
		snap.CurrentSession = cur
		markedRow(stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn)))
	}

	// Keyboard cursor (▶) at each structural position: flat rows are
	// 0 alpha (home), 1 alpha/pay-app (marked member), 3 dotfiles (single),
	// 4 projects/onboarding (unmarked member).
	for _, row := range []int{0, 1, 3, 4} {
		snap := flatSnapshot()
		snap.CursorRow = row
		snap.CursorActive = true
		markedRow(stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn)))
	}
}

// The marker never eats the frame or shifts the content column: a marked
// member row must align its glyph with its unmarked siblings.
func TestMarkerPreservesFrameAndContentColumn(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"

	snap := flatSnapshot()
	snap.CursorRow = 1 // alpha/pay-app — a marked group's member
	snap.CursorActive = true
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))

	var marked, plain string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, "· pay-app") && strings.HasPrefix(l, "▶"):
			marked = l
		case strings.Contains(l, "· pay-id"):
			plain = l
		}
	}
	if marked == "" || plain == "" {
		t.Fatalf("need both a marked and a plain member row:\n%s", out)
	}
	if !strings.Contains(marked, "▶ │") {
		t.Errorf("cursor row lost its frame gutter: %q", marked)
	}
	// Compare RUNE columns — box-drawing glyphs and the marker are all
	// multibyte, so byte offsets are not screen columns.
	if got, want := runeCol(marked, '·'), runeCol(plain, '·'); got != want {
		t.Errorf("marker shifted the content column:\n  marked %q (· at col %d)\n  plain  %q (· at col %d)",
			marked, got, plain, want)
	}
}

// A home row that is WAITING lights the header glyph; current homes carry ▌.
func TestGroupedHomeStates(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	now := fixedNowFn()
	snap := &proto.Snapshot{
		CurrentSession: "beta",
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: now - 90},
			{Name: "alpha/repo-a", Status: "alive"},
			{Name: "beta", Status: "alive"},
			{Name: "beta/repo-b", Status: "alive"},
		},
	}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "alpha") && !strings.Contains(line, "repo") {
			if strings.HasPrefix(strings.TrimSpace(line), "╭") {
				t.Errorf("waiting home must light the header glyph: %q", line)
			}
		}
	}
	if !strings.Contains(out, "▌ ╭ beta") {
		t.Errorf("current home carries the ▌ marker:\n%s", out)
	}
}

// A group straddling the demote divider re-states its synthetic header.
func TestGroupHeadersFoldRestatesBelowTheFold(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	defer func(m string) { DemoteMode = m }(DemoteMode)
	GroupMode = "prefix"
	DemoteMode = "fold"

	now := fixedNowFn()
	stale := now - int64(DemoteThresholdSec) - 10
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "projects/pay-app", Status: "alive", LastActivityTS: now},
		{Name: "projects/pay-id", Status: "alive", LastActivityTS: stale},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if n := strings.Count(out, "╭ projects"); n != 2 {
		t.Errorf("straddling group must re-state its header, got %d:\n%s", n, out)
	}
}

// A declared group is a group: the drawer header and the initiative home
// render with the SAME weight — identity hue + bold — because the .zdev
// marker made group-ness explicit. The difference between them is semantic
// (metadata, journal, tooling), never a visual demotion. This replaces an
// earlier test that asserted the opposite, from when group-ness was
// inferred from the absence of .git and a drawer really was accidental.
func TestDrawerHeaderMatchesInitiativeHome(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"

	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "alpha", Status: "alive"},
		{Name: "alpha/repo", Status: "alive"},
		{Name: "projects/api", Status: "alive"},
	}}
	out := string(Render(snap, 50, NewAnimator(), fixedNowFn))

	var drawer, home string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(stripAnsi([]byte(l)), "projects"):
			drawer = l
		case strings.Contains(stripAnsi([]byte(l)), "alpha") && !strings.Contains(stripAnsi([]byte(l)), "repo"):
			home = l
		}
	}
	if drawer == "" || home == "" {
		t.Fatalf("need both headers:\n%s", stripAnsi([]byte(out)))
	}
	for _, c := range []struct {
		label, row, hue string
	}{{"drawer", drawer, PaletteFor("projects")}, {"home", home, PaletteFor("alpha")}} {
		if !strings.Contains(c.row, Bold) {
			t.Errorf("%s header must be bold: %q", c.label, c.row)
		}
		if !strings.Contains(c.row, c.hue) {
			t.Errorf("%s header must carry its own identity hue: %q", c.label, c.row)
		}
		if strings.Contains(c.row, Dim) {
			t.Errorf("%s header must not be dimmed — no group is scaffolding: %q", c.label, c.row)
		}
	}
}
