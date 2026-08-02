package render

import (
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// The row map's whole value is that it cannot disagree with the pixels: for
// every entry, the frame line at that Y must actually display that target.
// This test asserts the correspondence directly against the rendered frame,
// so any future section that shifts the geometry (a new strip, a divider, a
// second metadata row) fails here rather than silently sending clicks to
// the wrong project.
func assertRowMapMatchesFrame(t *testing.T, frame []byte, rows []RowRef) {
	t.Helper()
	lines := strings.Split(stripAnsi(frame), "\n")

	// Ys must be unique, in order, and inside the frame: two targets on one
	// line means a click is ambiguous.
	prevY := -1
	for _, r := range rows {
		if r.Y < 0 || r.Y >= len(lines) {
			t.Errorf("RowRef{Y:%d, Name:%q} points outside the frame (%d lines)", r.Y, r.Name, len(lines))
			continue
		}
		if r.Y <= prevY {
			t.Errorf("line %d claimed twice or out of order (target %q)", r.Y, r.Name)
		}
		prevY = r.Y
	}

	// A target may own a RUN of lines (a project row plus its metadata
	// rows). The run's FIRST line must display the name — that's the row
	// the operator aims at; the continuation lines are its chips and need
	// not repeat it.
	last := ""
	for _, r := range rows {
		if r.Y < 0 || r.Y >= len(lines) {
			continue
		}
		if r.Name == last {
			continue // continuation line of the same target
		}
		last = r.Name
		leaf := r.Name
		if i := strings.LastIndex(leaf, "/"); i >= 0 && GroupMode != "off" {
			leaf = leaf[i+1:]
		}
		// A run may open on the member's leaf OR on its GROUP's name — a
		// synthetic drawer header ("╭ projects") is clickable and targets
		// the group's first member, so the line displays the group, not
		// the member.
		group := ""
		if i := strings.IndexByte(r.Name, '/'); i > 0 {
			group = r.Name[:i]
		}
		if !strings.Contains(lines[r.Y], leaf) && (group == "" || !strings.Contains(lines[r.Y], group)) {
			t.Errorf("line %d is %q, but the map opens that run with %q", r.Y, lines[r.Y], r.Name)
		}
	}
}

func TestRowMapMatchesRenderedFrame(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)

	for _, mode := range []string{"off", "prefix"} {
		GroupMode = mode
		snap := flatSnapshot()
		// A current session renders TWO rows (project + metadata); both must
		// map to it, or clicking the branch chip would miss.
		snap.CurrentSession = "alpha/pay-app"
		snap.Projects[1].Branch = "flat-root"
		snap.Projects[1].DirtyCount = 3

		frame, rows := RenderWithRows(snap, 50, NewAnimator(), fixedNowFn)
		assertRowMapMatchesFrame(t, frame, rows)

		if len(rows) == 0 {
			t.Fatalf("mode %s: no rows mapped", mode)
		}
		// Every project in the snapshot is reachable by a click.
		for _, p := range snap.Projects {
			found := false
			for _, r := range rows {
				if r.Name == p.Name {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("mode %s: %q has no clickable line", mode, p.Name)
			}
		}
		// The current session owns more than one line (its metadata row).
		n := 0
		for _, r := range rows {
			if r.Name == "alpha/pay-app" {
				n++
			}
		}
		if n < 2 {
			t.Errorf("mode %s: current session maps %d lines, want >=2 (row + metadata)", mode, n)
		}
	}
}

// Folded rows own no screen line, so they must own no click target — a
// click on a folded group's header must never switch to a hidden member.
func TestRowMapSkipsCollapsedRows(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"

	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "alpha", Status: "alive"},
		{Name: "alpha/repo-a", Status: "alive", Collapsed: true},
		{Name: "alpha/repo-b", Status: "alive", Collapsed: true},
		{Name: "zdev", Status: "alive"},
	}}
	frame, rows := RenderWithRows(snap, 50, NewAnimator(), fixedNowFn)
	assertRowMapMatchesFrame(t, frame, rows)

	for _, r := range rows {
		if strings.HasPrefix(r.Name, "alpha/") {
			t.Errorf("folded member %q is clickable at line %d", r.Name, r.Y)
		}
	}
	// The home row itself stays clickable — it is what's on screen.
	if !hasTarget(rows, "alpha") || !hasTarget(rows, "zdev") {
		t.Errorf("visible rows must stay clickable, got %+v", rows)
	}
}

// A folded DRAWER (unmarked group) keeps its synthetic header clickable —
// it is the group's only line, and an inert one made every click on
// "▸ projects ·N" a silent no-op. The header targets the group's first
// member, mirroring the M-p header contract.
func TestRowMapDrawerHeaderClickable(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"

	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "projects/api", Status: "alive", Collapsed: true},
		{Name: "projects/web", Status: "alive", Collapsed: true},
		{Name: "zdev", Status: "alive"},
	}}
	frame, rows := RenderWithRows(snap, 50, NewAnimator(), fixedNowFn)
	assertRowMapMatchesFrame(t, frame, rows)
	if !hasTarget(rows, "projects/api") {
		t.Errorf("folded drawer header must target the first member, got %+v", rows)
	}
	if hasTarget(rows, "projects/web") {
		t.Errorf("only the header's target may be reachable while folded, got %+v", rows)
	}
}

// The triage strip and review gauge shift every project row down. The map
// must follow them — this is the regression the old fixed-divisor click
// math could not survive.
func TestRowMapFollowsSectionOffsets(t *testing.T) {
	defer func(s bool, g bool, m string) {
		TriageStripEnabled, ReviewGaugeEnabled, GroupMode = s, g, m
	}(TriageStripEnabled, ReviewGaugeEnabled, GroupMode)
	GroupMode = "prefix"

	base := func() *proto.Snapshot {
		return &proto.Snapshot{Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: fixedNowFn() - 120},
			{Name: "zdev", Status: "alive"},
		},
			Triage: []string{"alpha"},
		}
	}

	TriageStripEnabled, ReviewGaugeEnabled = false, false
	_, plain := RenderWithRows(base(), 50, NewAnimator(), fixedNowFn)

	TriageStripEnabled = true
	frame, shifted := RenderWithRows(base(), 50, NewAnimator(), fixedNowFn)
	assertRowMapMatchesFrame(t, frame, shifted)

	if yOf(plain, "zdev") == yOf(shifted, "zdev") {
		t.Errorf("the triage strip must shift the project rows down; zdev stayed at line %d", yOf(plain, "zdev"))
	}
}

func hasTarget(rows []RowRef, name string) bool {
	for _, r := range rows {
		if r.Name == name {
			return true
		}
	}
	return false
}

func yOf(rows []RowRef, name string) int {
	for _, r := range rows {
		if r.Name == name {
			return r.Y
		}
	}
	return -1
}
