package render

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

func TestGroupKey(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"pay/pay-app", "pay"},
		{"ai-at-pay/pay-app", "ai-at-pay"},
		{"zdev", ""},
		{"dotfiles", ""},
		{"a/b/c", "a"}, // nested paths key on the FIRST segment only
		{"/odd", ""},   // leading slash: no non-empty first segment
		{"", ""},
		// The initiatives container: key is the INITIATIVE (second
		// segment), and the home row keys as its own name.
		{"initiatives/marketplace/pay-app", "marketplace"},
		{"initiatives/marketplace", "marketplace"},
		{"initiatives", ""},
		{"projects/pay-app", "projects"},
	}
	for _, c := range cases {
		if got := groupKey(c.name); got != c.want {
			t.Errorf("groupKey(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)

	GroupMode = "prefix"
	cases := []struct {
		name string
		want string
	}{
		{"initiatives/marketplace/pay-app", "pay-app"},
		{"initiatives/marketplace", "marketplace"}, // home (renders as header anyway)
		{"projects/pay-app", "pay-app"},
		{"pay/pay-app", "pay-app"}, // legacy prefix
		{"zdev", "zdev"},           // bare: unchanged
		// An initiative name that happens to occur inside "initiatives" —
		// the structural parse must not misfire the way a substring search
		// would.
		{"initiatives/tia/repo", "repo"},
	}
	for _, c := range cases {
		if got := displayName(c.name); got != c.want {
			t.Errorf("displayName(%q) = %q, want %q", c.name, got, c.want)
		}
	}

	GroupMode = "off"
	if got := displayName("initiatives/marketplace/pay-app"); got != "initiatives/marketplace/pay-app" {
		t.Errorf("GroupMode=off must not strip names, got %q", got)
	}
}

// ansiRE strips SGR/cursor escapes so assertions match the VISIBLE text.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripAnsi(b []byte) string { return ansiRE.ReplaceAllString(string(b), "") }

// groupTestSnapshot mirrors the live layout: two initiatives (home row +
// one clone each), the projects container, and bare singles. Order matches
// proto.GroupSort — the order the daemon publishes when grouping is on.
func groupTestSnapshot() *proto.Snapshot {
	names := []string{
		"zdev", "projects/pay-app", "initiatives/ai-at-pay",
		"initiatives/ai-at-pay/pay-app", "dotfiles",
		"initiatives/marketplace", "initiatives/marketplace/pay-app",
		"projects/onboarding",
	}
	proto.GroupSort(names)
	snap := &proto.Snapshot{}
	for _, n := range names {
		snap.Projects = append(snap.Projects, proto.Project{Name: n, Status: "alive"})
	}
	return snap
}

func TestGroupedFrame(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)

	// Knob off: no headers, full names.
	GroupMode = "off"
	off := stripAnsi(Render(groupTestSnapshot(), 50, NewAnimator(), fixedNowFn))
	if strings.Contains(off, "─ ai-at-pay ─") || strings.Contains(off, "╭ ai-at-pay") {
		t.Fatalf("GroupMode=off must not render group headers:\n%s", off)
	}
	if !strings.Contains(off, "initiatives/ai-at-pay/pay-app") {
		t.Fatalf("GroupMode=off must keep full names:\n%s", off)
	}

	GroupMode = "prefix"
	out := stripAnsi(Render(groupTestSnapshot(), 50, NewAnimator(), fixedNowFn))

	// Home rows render AS headers — exactly one ai-at-pay header line
	// (opened with the ╭ corner), and no plain home row anywhere.
	if n := strings.Count(out, "╭ ai-at-pay"); n != 1 {
		t.Errorf("ai-at-pay header count = %d, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "· ai-at-pay\n") || strings.Contains(out, "initiatives/ai-at-pay\n") {
		t.Errorf("home row must render as the header, not as a plain row:\n%s", out)
	}
	// projects has no home: synthetic header.
	if n := strings.Count(out, "╭ projects"); n != 1 {
		t.Errorf("projects header count = %d, want 1:\n%s", n, out)
	}
	// Members: leaf-only display, indented under their header.
	if strings.Contains(out, "initiatives/ai-at-pay/pay-app") || strings.Contains(out, "projects/pay-app") {
		t.Errorf("member rows must display leaf names only:\n%s", out)
	}
	// projects has two members: onboarding runs the gutter, pay-app (last)
	// closes the frame; single-member initiative groups close immediately.
	if !strings.Contains(out, "  │  · onboarding") {
		t.Errorf("non-last member rows must carry the │ gutter:\n%s", out)
	}
	if !strings.Contains(out, "  ╰  · pay-app") {
		t.Errorf("the last visible member must close the frame with ╰:\n%s", out)
	}
	// Order: groups first (alpha), then the bare separator, then singles.
	iAI := strings.Index(out, "╭ ai-at-pay")
	iMkt := strings.Index(out, "╭ marketplace")
	iProj := strings.Index(out, "╭ projects")
	iSep := strings.Index(out[iProj:], "\n\n")
	if iSep >= 0 {
		iSep += iProj
	}
	iDot := strings.Index(out, "· dotfiles")
	iZdev := strings.Index(out, "· zdev")
	if !(iAI < iMkt && iMkt < iProj && iProj < iSep && iSep < iDot && iDot < iZdev) {
		t.Errorf("expected groups (alpha) then separator then singles; got positions ai=%d mkt=%d proj=%d sep=%d dot=%d zdev=%d:\n%s",
			iAI, iMkt, iProj, iSep, iDot, iZdev, out)
	}
}

// A home row that is WAITING lights its header glyph instead of the dash.
func TestGroupedHomeAttention(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	now := fixedNowFn()
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "initiatives/ai-at-pay", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: now - 90},
		{Name: "initiatives/ai-at-pay/pay-app", Status: "alive"},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	// The header line carries a non-dash glyph before the name.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "ai-at-pay") && !strings.Contains(line, "pay-app") {
			if strings.HasPrefix(strings.TrimSpace(line), "╭") {
				t.Errorf("waiting home must light the header glyph, not the idle corner: %q", line)
			}
			return
		}
	}
	t.Fatalf("no header line found:\n%s", out)
}

// The current session's home row carries the ▌ marker — switching into an
// initiative home must be visible (live bug, 2026-07-30).
func TestGroupedHomeCurrentMarker(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	snap := &proto.Snapshot{
		CurrentSession: "initiatives/ai-at-pay",
		Projects: []proto.Project{
			{Name: "initiatives/ai-at-pay", Status: "alive"},
			{Name: "initiatives/ai-at-pay/pay-app", Status: "alive"},
		},
	}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if !strings.Contains(out, "▌ ╭ ai-at-pay") {
		t.Errorf("current home row must carry the ▌ marker:\n%s", out)
	}
}

// A group straddling the demote divider re-states its header below the fold
// (synthetic form — the home row, if any, stays wherever demotion put it).
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
		t.Errorf("straddling group must render its header in BOTH blocks, got %d:\n%s", n, out)
	}
}

// Collapsed member rows (phase4-v22): hidden from the frame, rolled up as a
// dim count on the home header; a working hidden member lights the corner.
func TestGroupedCollapse(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "initiatives/alpha", Status: "alive"},
		{Name: "initiatives/alpha/repo-a", Status: "alive", Collapsed: true},
		{Name: "initiatives/alpha/repo-b", Status: "alive", Collapsed: true},
		{Name: "initiatives/beta", Status: "alive"},
		{Name: "initiatives/beta/repo-c", Status: "alive"},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "repo-a") || strings.Contains(out, "repo-b") {
		t.Errorf("collapsed members must not render:\n%s", out)
	}
	if !strings.Contains(out, "▸ alpha ·2") {
		t.Errorf("fully folded home opens with ▸ and the rollup:\n%s", out)
	}
	if !strings.Contains(out, "╭ beta") {
		t.Errorf("expanded group keeps the ╭ corner:\n%s", out)
	}
	if !strings.Contains(out, "  ╰  · repo-c") {
		t.Errorf("expanded group's sole member closes the frame:\n%s", out)
	}
	if strings.Contains(out, "repo-b") {
		t.Errorf("hidden member must not render:\n%s", out)
	}
}

// A homeless container (projects/) folds entirely behind its synthetic
// header, which carries the rollup — its only remaining trace.
func TestGroupedCollapseContainer(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "projects/pay-app", Status: "alive", Collapsed: true},
		{Name: "projects/pay-id", Status: "absent", Collapsed: true},
		{Name: "zdev", Status: "alive"},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "pay-app") || strings.Contains(out, "pay-id") {
		t.Errorf("collapsed container members must not render:\n%s", out)
	}
	if !strings.Contains(out, "▸ projects ·2") {
		t.Errorf("fully folded container opens with ▸ and the rollup:\n%s", out)
	}
	if !strings.Contains(out, "· zdev") {
		t.Errorf("singles still render:\n%s", out)
	}
}
