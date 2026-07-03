package main

import (
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// Test A: normalizeProjectName converts slash-form to dash-form.
func TestNormalizeProjectName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"example/agora", "example-agora"},
		{"example-agora", "example-agora"},
		{"foo", "foo"},
		{"", ""},
		{"a/b/c", "a-b-c"},
	}
	for _, tt := range tests {
		got := normalizeProjectName(tt.input)
		if got != tt.want {
			t.Errorf("normalizeProjectName(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
}

// Test B: findProject matches both slash-form and dash-form names.
func TestFindProject(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "example/agora", Status: "waiting"},
			{Name: "example-backend", Status: "alive"},
			{Name: "other-project", Status: "alive"},
		},
	}

	tests := []struct {
		target    string
		wantName  string
		wantFound bool
	}{
		{"example-agora", "example/agora", true},     // dash-form target matches slash-form project
		{"example/agora", "example/agora", true},     // slash normalized to dash matches slash-form project
		{"example-backend", "example-backend", true}, // dash-form target matches dash-form project
		{"nonexistent", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		p := findProject(snap, normalizeProjectName(tt.target))
		if tt.wantFound {
			if p == nil {
				t.Errorf("findProject(snap, %q) = nil; want project named %q", tt.target, tt.wantName)
				continue
			}
			if p.Name != tt.wantName {
				t.Errorf("findProject(snap, %q).Name = %q; want %q", tt.target, p.Name, tt.wantName)
			}
		} else {
			if p != nil {
				t.Errorf("findProject(snap, %q) = %q; want nil", tt.target, p.Name)
			}
		}
	}
}

// Test C: formatShow renders dim header + verbatim WaitContext.
func TestFormatShow_WithContext(t *testing.T) {
	p := &proto.Project{
		Name:        "example/agora",
		WaitContext: "line1\nline2",
	}
	got := formatShow(p)

	// Must start with the dim header.
	if !strings.HasPrefix(got, dim) {
		t.Errorf("formatShow output does not start with dim escape; got: %q", got)
	}
	// Must contain the project name.
	if !strings.Contains(got, "example/agora") {
		t.Errorf("formatShow output does not contain project name; got: %q", got)
	}
	// Must contain the reset escape after the header.
	if !strings.Contains(got, reset) {
		t.Errorf("formatShow output does not contain reset escape; got: %q", got)
	}
	// Body must appear verbatim.
	if !strings.Contains(got, "line1\nline2") {
		t.Errorf("formatShow output does not contain verbatim WaitContext; got: %q", got)
	}
	// Must end with a newline.
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("formatShow output does not end with newline; got: %q", got)
	}
}

// Test D: formatShow with empty WaitContext returns the no-context fallback.
func TestFormatShow_EmptyWaitContext(t *testing.T) {
	p := &proto.Project{
		Name:        "example/agora",
		WaitContext: "",
	}
	got := formatShow(p)
	want := "(no waiting context for example/agora)\n"
	if got != want {
		t.Errorf("formatShow empty WaitContext = %q; want %q", got, want)
	}
}

// Test E: formatList shows only waiting projects with a one-line preview.
func TestFormatList(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:        "waiting-with-context",
				Status:      "waiting",
				WaitContext: "\nfirst non-empty line\nsecond line",
			},
			{
				Name:        "waiting-no-context",
				Status:      "waiting",
				WaitContext: "",
			},
			{
				Name:   "alive-project",
				Status: "alive",
			},
		},
	}
	got := formatList(snap)

	// Must include both waiting projects.
	if !strings.Contains(got, "waiting-with-context") {
		t.Errorf("formatList output missing waiting-with-context; got: %q", got)
	}
	if !strings.Contains(got, "waiting-no-context") {
		t.Errorf("formatList output missing waiting-no-context; got: %q", got)
	}
	// Must NOT include alive project.
	if strings.Contains(got, "alive-project") {
		t.Errorf("formatList output includes non-waiting project alive-project; got: %q", got)
	}
	// The preview for waiting-with-context should be the first non-empty line.
	if !strings.Contains(got, "first non-empty line") {
		t.Errorf("formatList output missing first non-empty line preview; got: %q", got)
	}
	// No-context project should show fallback.
	if !strings.Contains(got, "(no captured context)") {
		t.Errorf("formatList output missing (no captured context) for waiting-no-context; got: %q", got)
	}
}

// TestFormatTeamsTSV verifies the M-p switcher feed: one tab line per team
// MEMBER (never a project row), sourced from TeamGroups, with in-process
// members (no window) carrying an empty window_id — the parity with the
// sidebar that a tmux `@zdev-team` tag scrape can't provide.
func TestFormatTeamsTSV(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "org/lead", Status: "alive"},
			{Name: "org/solo", Status: "alive"}, // no team → no rows
		},
		TeamGroups: []proto.TeamGroup{
			{
				Name:        "team-x",
				LeadProject: "org/lead",
				Members: []proto.TeamMember{
					{Name: "windowed", Status: "working", WindowID: "@42"},
					{Name: "headless", Status: "", InProcess: true}, // in-process: no window
				},
			},
		},
	}
	got := formatTeamsTSV(snap)

	want := []string{
		"org/lead\twindowed\tworking\t@42\n",
		"org/lead\theadless\t\t\n", // empty status + empty window_id
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("teams TSV missing line %q; got:\n%q", w, got)
		}
	}
	// Project rows must never leak into the feed — it is members only.
	if strings.Contains(got, "org/solo") {
		t.Errorf("teams TSV leaked a project row (org/solo); got:\n%q", got)
	}
	if n := strings.Count(got, "\n"); n != 2 {
		t.Errorf("teams TSV should have exactly 2 member lines, got %d:\n%q", n, got)
	}
}

// TestFormatTeamsTSV_NoTeam verifies an empty feed when no team is live — the
// switcher then falls back to a plain project list.
func TestFormatTeamsTSV_NoTeam(t *testing.T) {
	snap := &proto.Snapshot{Projects: []proto.Project{{Name: "org/solo", Status: "alive"}}}
	if got := formatTeamsTSV(snap); got != "" {
		t.Errorf("teams TSV should be empty with no live team, got: %q", got)
	}
}

// TestFormatList_NoWaiting verifies the empty-state message when no projects are waiting.
func TestFormatList_NoWaiting(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
			{Name: "beta", Status: "finished"},
		},
	}
	got := formatList(snap)
	want := "(no projects currently waiting)\n"
	if got != want {
		t.Errorf("formatList no-waiting = %q; want %q", got, want)
	}
}

// TestFormatList_PreviewTruncation verifies that previews longer than 80 chars
// are truncated with "...".
func TestFormatList_PreviewTruncation(t *testing.T) {
	longLine := strings.Repeat("x", 100)
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", WaitContext: longLine},
		},
	}
	got := formatList(snap)
	// The preview portion (after the name and reset) should be ≤80 chars + "..." truncation.
	// We can't easily extract the preview portion exactly, but we can check
	// that the truncated marker appears and the full 100-char string does NOT.
	if strings.Contains(got, longLine) {
		t.Errorf("formatList did not truncate 100-char preview; got: %q", got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("formatList truncation marker '...' not found; got: %q", got)
	}
}

// TestFormatAgentsFromRegistry verifies the registry-list output shape used
// by bin/zdev: one line per agent (declaration order), <binary>\t<launch>,
// detection-only agents (empty Launch) skipped.
func TestFormatAgentsFromRegistry(t *testing.T) {
	r := agents.NewRegistry([]agents.Spec{
		{Name: "claude", Glyph: "✻", Launch: "claude --dangerously-skip-permissions --continue"},
		{Name: "opencode", Glyph: "○", Launch: "opencode"},
		{Name: "detect-only", Glyph: "•"}, // empty Launch — must be skipped
	})
	got := formatAgentsFromRegistry(r)
	want := "claude\tclaude --dangerously-skip-permissions --continue\n" +
		"opencode\topencode\n"
	if got != want {
		t.Errorf("formatAgentsFromRegistry =\n%q\nwant:\n%q", got, want)
	}
}

// TestFormatAgentsFromRegistry_Empty verifies an empty registry produces
// no output (consumers must treat empty output as "no auto-launch").
func TestFormatAgentsFromRegistry_Empty(t *testing.T) {
	r := agents.NewRegistry(nil)
	if got := formatAgentsFromRegistry(r); got != "" {
		t.Errorf("formatAgentsFromRegistry(empty) = %q; want empty", got)
	}
}

// TestFormatLegend_AgentTeams verifies the legend documents the Agent Teams
// vocabulary the sidebar actually renders (chipTeamBadge): the ⊛ lead-row
// badge, the busy/idle/waiting bullet states, and the ZDEV_TEAM_WINDOWS knob
// that surfaces nested member rows. The legend is the source of truth for
// "what does this glyph mean" — if these drift from render, fix both.
func TestFormatLegend_AgentTeams(t *testing.T) {
	out := formatLegend()
	for _, want := range []string{
		"Agent Teams",
		"⊛",
		"◦",              // idle/available member bullet
		"identity color", // busy/idle bullets carry the member's color
		"ZDEV_TEAM_WINDOWS=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("legend missing %q", want)
		}
	}
}
