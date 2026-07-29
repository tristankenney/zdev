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
	}
	for _, c := range cases {
		if got := groupKey(c.name); got != c.want {
			t.Errorf("groupKey(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// ansiRE strips SGR/cursor escapes so header assertions can match the
// VISIBLE text — the header interleaves Dim/Bold/Reset between its glyphs,
// so raw-byte substring matches would be coupled to the exact styling.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripAnsi(b []byte) string { return ansiRE.ReplaceAllString(string(b), "") }

// groupTestSnapshot mirrors the user-visible shape: two prefixed groups
// with bare projects interleaved by sort order ("dotfiles" between the
// groups, "zdev" trailing — the orphan case: neither may render under a
// header it doesn't belong to).
func groupTestSnapshot() *proto.Snapshot {
	return &proto.Snapshot{Projects: []proto.Project{
		{Name: "ai-at-pay/pay-app", Status: "alive"},
		{Name: "ai-at-pay/pay-id", Status: "alive"},
		{Name: "dotfiles", Status: "alive"},
		{Name: "pay/pay-app", Status: "alive"},
		{Name: "pay/pay-id", Status: "absent"},
		{Name: "zdev", Status: "alive"},
	}}
}

func TestGroupHeaders(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)

	// Knob off: no header lines — byte-identical contract with pre-knob
	// output for ungrouped fleets is covered by every other render test
	// running with the "off" default; here just assert no headers appear.
	GroupMode = "off"
	off := stripAnsi(Render(groupTestSnapshot(), 50, NewAnimator(), fixedNowFn))
	if strings.Contains(off, "─ ai-at-pay ──") || strings.Contains(off, "─ pay ──") {
		t.Fatalf("GroupMode=off must not render group headers:\n%s", off)
	}

	// Knob on: one header per contiguous prefix run, in list order, and a
	// bare separator before each unprefixed run that follows a named group.
	GroupMode = "prefix"
	out := stripAnsi(Render(groupTestSnapshot(), 50, NewAnimator(), fixedNowFn))

	iAI := strings.Index(out, "─ ai-at-pay ──")
	iPay := strings.Index(out, "─ pay ──")
	if iAI < 0 || iPay < 0 {
		t.Fatalf("expected both group headers, got:\n%s", out)
	}
	if iAI > iPay {
		t.Errorf("ai-at-pay header must precede pay header (list order)")
	}
	if n := strings.Count(out, "─ ai-at-pay ──"); n != 1 {
		t.Errorf("ai-at-pay header count = %d, want 1", n)
	}
	if n := strings.Count(out, "─ pay ──"); n != 1 {
		t.Errorf("pay header count = %d, want 1", n)
	}
	// Orphan separators: "dotfiles" follows the ai-at-pay group and "zdev"
	// follows the pay group; each needs a bare "──────" line before it so
	// it doesn't read as a member of the preceding group.
	// Line-anchored: the 17-dash mood/demote dividers END in six dashes, so
	// an unanchored "──────" would match them; the bare separator is the
	// only line that is exactly two spaces + six dashes.
	const bareSep = "\n  ──────\n"
	iDotfiles := strings.Index(out, "dotfiles")
	iSep1 := strings.Index(out, bareSep)
	if iSep1 < 0 || iSep1 < iAI || iSep1 > iDotfiles {
		t.Errorf("expected bare separator between ai-at-pay group and dotfiles:\n%s", out)
	}
	iZdev := strings.LastIndex(out, "zdev")
	iSep2 := strings.LastIndex(out[:iZdev], bareSep)
	if iSep2 < 0 || iSep2 < iPay {
		t.Errorf("expected bare separator between pay group and trailing zdev:\n%s", out)
	}
}

// TestGroupHeadersRootRow: an initiative root (bare "ai-at-pay" immediately
// followed by "ai-at-pay/*" members) renders UNDER its group header, not
// orphaned above it in the ungrouped block.
func TestGroupHeadersRootRow(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "ai-at-pay", Status: "alive"}, // root/home row
		{Name: "ai-at-pay/pay-app", Status: "alive"},
		{Name: "dotfiles", Status: "alive"},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	iHeader := strings.Index(out, "─ ai-at-pay ──")
	iRoot := strings.Index(out, "· ai-at-pay\n")
	if iHeader < 0 || iRoot < 0 {
		t.Fatalf("expected header and root row:\n%s", out)
	}
	if iRoot < iHeader {
		t.Errorf("root row must render UNDER its header, not above it:\n%s", out)
	}
}

// TestGroupHeadersFoldRestatesBelowTheFold: a group straddling the demote
// divider re-states its header in the demoted block, so no demoted row
// renders under a header that stayed above the fold.
func TestGroupHeadersFoldRestatesBelowTheFold(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	defer func(m string) { DemoteMode = m }(DemoteMode)
	GroupMode = "prefix"
	DemoteMode = "fold"

	now := fixedNowFn()
	stale := now - int64(DemoteThresholdSec) - 10
	snap := &proto.Snapshot{Projects: []proto.Project{
		// Same group: one active, one demoted (stale, no attention).
		{Name: "pay/pay-app", Status: "alive", LastActivityTS: now},
		{Name: "pay/pay-id", Status: "alive", LastActivityTS: stale},
	}}
	out := stripAnsi(Render(snap, 50, NewAnimator(), fixedNowFn))
	if n := strings.Count(out, "─ pay ──"); n != 2 {
		t.Errorf("straddling group must render its header in BOTH blocks, got %d:\n%s", n, out)
	}
}
