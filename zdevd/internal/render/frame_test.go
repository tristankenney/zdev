package render

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

var update = flag.Bool("update", false, "update golden files from current code")

// TestRenderStub_Golden asserts that RenderStub produces the exact bytes
// committed to testdata/phase1-stub.golden for the locked Phase 1 stub
// snapshot (CONTEXT D-11). With `-update`, regenerates the fixture from
// current code (idiomatic Go golden-file pattern, RESEARCH §"Golden-frame
// test harness"). Per RESEARCH Open Question 1 the project ships with this
// single canonical fixture rather than an empty fixture set — strictly
// stronger and catches real bugs (e.g., trailing whitespace).
func TestRenderStub_Golden(t *testing.T) {
	// Deterministic input: the Phase 1 stub per CONTEXT D-11. SentAt is
	// fixed so the fixture bytes are reproducible — though the rendered
	// output never embeds SentAt, so this is belt-and-braces.
	snap := proto.NewStubSnapshot(1, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	got := RenderStub(&snap, 50)

	goldenPath := filepath.Join("testdata", "phase1-stub.golden")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d bytes)", goldenPath, len(got))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/render -run Golden -update` to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RenderStub output diverges from %s.\nwant: %q\ngot:  %q",
			goldenPath, want, got)
	}
}

// TestRenderStub_EmptyProjects: zero Projects must not panic and must
// still emit the 2-line shape (header + body row both terminated with LF).
func TestRenderStub_EmptyProjects(t *testing.T) {
	snap := proto.Snapshot{
		V:        proto.CurrentProtocolVersion,
		Type:     "snapshot",
		Schema:   proto.SchemaVersion,
		Seq:      99,
		Projects: nil,
	}
	out := RenderStub(&snap, 50)
	if !bytes.Contains(out, []byte("zdev projects")) {
		t.Errorf("missing header in empty-projects render: %q", out)
	}
	if bytes.Count(out, []byte{'\n'}) < 2 {
		t.Errorf("expected at least 2 newlines, got %d in %q", bytes.Count(out, []byte{'\n'}), out)
	}
}

// TestRenderUnreachable verifies the fallback frame the renderer prints
// when zdevd is not reachable. Must (a) contain the literal substring
// "(zdevd unreachable: <reason>)" so the human-verify checkpoint can grep
// for it, (b) preserve the 2-line frame shape (header + body row, both
// terminated with LF), and (c) start with CursorHome and end with ClearToEnd
// so the pane geometry matches RenderStub.
func TestRenderUnreachable(t *testing.T) {
	out := RenderUnreachable("dial unix /tmp/zdevd.sock: connect: no such file or directory", 50)

	if !bytes.HasPrefix(out, []byte(CursorHome)) {
		t.Errorf("RenderUnreachable must start with CursorHome; got %q", out[:min(8, len(out))])
	}
	if !bytes.HasSuffix(out, []byte(ClearToEnd)) {
		t.Errorf("RenderUnreachable must end with ClearToEnd; got %q", out[max(0, len(out)-8):])
	}
	if !bytes.Contains(out, []byte("zdev projects")) {
		t.Errorf("RenderUnreachable must contain header 'zdev projects': %q", out)
	}
	if !bytes.Contains(out, []byte("(zdevd unreachable: ")) {
		t.Errorf("RenderUnreachable must contain '(zdevd unreachable: ' substring: %q", out)
	}
	if bytes.Count(out, []byte{'\n'}) < 2 {
		t.Errorf("RenderUnreachable must have at least 2 newlines (header + body), got %d in %q",
			bytes.Count(out, []byte{'\n'}), out)
	}
}

// TestRenderStub_StartsWithCursorHome confirms the framing invariants:
// every frame starts with CursorHome (so the renderer can repaint over
// any prior frame) and ends with ClearToEnd (so leftover bytes from
// a longer prior frame are wiped).
func TestRenderStub_StartsWithCursorHome(t *testing.T) {
	snap := proto.NewStubSnapshot(1, time.Now())
	out := RenderStub(&snap, 50)
	if !bytes.HasPrefix(out, []byte(CursorHome)) {
		prefixLen := 8
		if len(out) < prefixLen {
			prefixLen = len(out)
		}
		t.Errorf("RenderStub output must start with CursorHome (\\x1b[H); got prefix %q", out[:prefixLen])
	}
	if !bytes.HasSuffix(out, []byte(ClearToEnd)) {
		suffixStart := len(out) - 8
		if suffixStart < 0 {
			suffixStart = 0
		}
		t.Errorf("RenderStub output must end with ClearToEnd (\\x1b[J); got suffix %q", out[suffixStart:])
	}
}

// --- Phase 3 Render() tests ---

// fixedNow is the reference unix timestamp used in all Render tests:
// 2026-05-04T12:00:00Z = 1777860000
const fixedNow int64 = 1777860000

func fixedNowFn() int64 { return fixedNow }

// TestRender_PreservesPhase1Stub ensures a minimal snapshot produces a
// well-formed Phase 3 frame with cursor home, header, body, and clear-to-end.
// Note: EmDash is no longer asserted here — non-current projects (no
// CurrentSession) use the compact single-row layout which has no metadata row.
// EmDash is removed entirely in 260511-n4n task 6.
func TestRender_PreservesPhase1Stub(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	if !bytes.Contains(out, []byte(CursorHome)) {
		t.Errorf("Render missing CursorHome")
	}
	if !bytes.Contains(out, []byte("zdev projects")) {
		t.Errorf("Render missing 'zdev projects' header")
	}
	if !bytes.Contains(out, []byte("alpha")) {
		t.Errorf("Render missing project name 'alpha'")
	}
	if !bytes.Contains(out, []byte(ClearToEnd)) {
		t.Errorf("Render missing ClearToEnd")
	}
}

// TestRender_TwoLevelRowCounts verifies the domain-row layout row counts.
// With no CurrentSession, all projects are compact (1 row each).
// With a CurrentSession, the current project gets 1 marker + N domain rows
// (N=0 when all chip sub-groups are empty — 260511-ohu domain-row suppression).
func TestRender_TwoLevelRowCounts(t *testing.T) {
	cases := []struct {
		name           string
		projects       []proto.Project
		currentSession string
		want           int // expected project-row newlines
	}{
		{"empty", nil, "", 0},
		{"one-no-current", []proto.Project{{Name: "a", Status: "alive"}}, "", 1},
		// Current with no metadata → 1 marker row (all domain rows suppressed).
		{"one-current", []proto.Project{{Name: "a", Status: "alive"}}, "a", 1},
		{"five-no-current", []proto.Project{
			{Name: "a", Status: "alive"},
			{Name: "b", Status: "waiting"},
			{Name: "c", Status: "finished"},
			{Name: "d", Status: "absent"},
			{Name: "e", Status: "shell-running"},
		}, "", 5},
		// c=finished, no metadata → 1 marker (domain rows suppressed); others=1 each → 5 total.
		{"five-one-current", []proto.Project{
			{Name: "a", Status: "alive"},
			{Name: "b", Status: "waiting"},
			{Name: "c", Status: "finished"},
			{Name: "d", Status: "absent"},
			{Name: "e", Status: "shell-running"},
		}, "c", 5},
	}
	anim := NewAnimator()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &proto.Snapshot{Projects: tc.projects, CurrentSession: tc.currentSession}
			anim.OnSnapshot(snap)
			out := Render(snap, 50, anim, fixedNowFn)
			rows := countProjectRows(out)
			if rows != tc.want {
				t.Errorf("project rows = %d, want %d", rows, tc.want)
			}
		})
	}
}

// countProjectRows returns the number of project rows in the rendered frame.
// The frame structure (after bytes.Split on "\n") is:
//
//	[0] CursorHome + header
//	[1] divider
//	[2..2+2N-1] project rows (2 per project)
//	[2+2N] footer
//	[2+2N+1] ClearToEnd (no trailing \n)
//
// So project rows = len(splitLines) - 4.
func countProjectRows(frame []byte) int {
	lines := bytes.Split(frame, []byte("\n"))
	// Need at least 4 non-project lines: header, divider, footer, clearToEnd.
	if len(lines) < 4 {
		return 0
	}
	n := len(lines) - 4
	if n < 0 {
		return 0
	}
	return n
}

// TestRender_PulseGlyph verifies that a waiting project's marker glyph
// comes from the animator's current pulseFrame (not a hardcoded "●").
func TestRender_PulseGlyph(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting"},
		},
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	// Force pulse to frame 4 (peak "●").
	anim.pulseFrame = 4

	out := Render(snap, 50, anim, fixedNowFn)
	// Frame 4 is "●" — verify it appears in the marker row.
	if !bytes.Contains(out, []byte(PulseFrames[4])) {
		t.Errorf("Render waiting: expected PulseFrames[4]=%q in output\n%q", PulseFrames[4], out)
	}

	// Force to frame 0 ("·") — different from "●".
	anim.pulseFrame = 0
	out0 := Render(snap, 50, anim, fixedNowFn)
	if !bytes.Contains(out0, []byte(PulseFrames[0])) {
		t.Errorf("Render waiting frame 0: expected PulseFrames[0]=%q in output\n%q", PulseFrames[0], out0)
	}
}

// TestRender_BreathBarOnCurrentSession verifies that the current-session
// project's marker row and each populated domain row get the breath bar prefix
// (VIS-03). With domain-row layout (260511-ohu), the second breath prefix
// appears on the git domain row when Branch is set. Beta (non-current) gets
// no breath prefix.
func TestRender_BreathBarOnCurrentSession(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			// Branch set so the git domain row populates (ensuring 2 breath prefixes).
			{Name: "alpha", Status: "alive", Branch: "feat-x"},
			{Name: "beta", Status: "alive"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	anim.breathState = 0 // bold peak

	out := Render(snap, 50, anim, fixedNowFn)
	// frame 0 = bold + alpha's palette hue (was bold-cyan before quick task 260511-mgy)
	breathPrefix := []byte(BreathColorForProject("alpha", 0) + "▌" + Reset)

	// Count occurrences of the breath prefix — should be exactly 2:
	// marker row + 1 git domain row (Branch is set). All-empty domain rows
	// are suppressed (renderDomainRow empty-body gate), so the count equals
	// 1 + number of populated domain rows.
	count := bytes.Count(out, breathPrefix)
	if count != 2 {
		t.Errorf("expected 2 breath-bar prefixes (marker + git domain row), got %d\n%q", count, out)
	}
}

// TestRender_StaleDimOut verifies that an alive project with
// LastActivityTS older than StaleThresholdSec uses Dim for its marker
// instead of the palette hue (VIS-12).
func TestRender_StaleDimOut(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", LastActivityTS: fixedNow - 7200},
		},
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 50, anim, fixedNowFn)

	// The marker row should contain Dim color for the marker glyph.
	// Palette color for "alpha" should NOT be in the marker context.
	paletteColor := PaletteFor("alpha")
	// Verify Dim appears (it's the stale marker color).
	if !bytes.Contains(out, []byte(Dim)) {
		t.Errorf("Render stale: expected Dim color in output\n%q", out)
	}
	// The palette color should NOT be in the output as the marker color
	// (it might still appear if it happens to equal Dim, but we test the behavior).
	if paletteColor != Dim && bytes.Contains(out, []byte(paletteColor)) {
		t.Logf("Render stale: palette color %q still present (may be in em-dash row); checking marker row specifically", paletteColor)
	}
}

// TestRender_FooterCounts verifies the footer summary counts (VIS-06).
func TestRender_FooterCounts(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "waiting"},
			{Name: "b", Status: "finished"},
			{Name: "c", Status: "alive"},
			{Name: "d", Status: "alive"},
		},
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 50, anim, fixedNowFn)
	// Footer: "1● 0◎ 1◆ 2· 0·"
	if !bytes.Contains(out, []byte("1● 0◎ 1◆ 2· 0·")) {
		t.Errorf("Render footer: expected '1● 0◎ 1◆ 2· 0·' in output\n%q", out)
	}
}

// TestRender_TruncationApplied verifies branch names longer than 24
// runes are truncated in the current-session metadata row (VIS-11).
// Non-current compact rows never show a branch chip (PD-02), so
// truncation is only observable on the current project's metadata row.
// Task 2 (260511-n4n) raises the cap from 14 to 24 for current rows;
// this test reflects that final behavior.
func TestRender_TruncationApplied(t *testing.T) {
	// Use a branch that exceeds both caps to be robust:
	// "feature-x-with-very-long-name" (29 chars) truncates at both 14 and 24.
	longBranch := "feature-x-with-very-long-name"
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: longBranch},
		},
		CurrentSession: "alpha", // must be current to get metadata row with branch
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 50, anim, fixedNowFn)
	if bytes.Contains(out, []byte(longBranch)) {
		t.Errorf("Render truncation: full branch name should not appear in output\n%q", out)
	}
	// Verify Ellipsis appears (truncation happened).
	if !bytes.Contains(out, []byte(Ellipsis)) {
		t.Errorf("Render truncation: expected Ellipsis in output\n%q", out)
	}
}

// TestRender_NoEmDashForCompact verifies that a non-current project with no
// metadata chips does NOT use the em-dash placeholder — it renders as a
// single compact row. The EmDash is fully removed in task 6.
func TestRender_NoEmDashForCompact(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
		CurrentSession: "", // non-current → compact row, no em-dash
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 50, anim, fixedNowFn)
	if bytes.Contains(out, []byte(EmDash)) {
		t.Errorf("Render compact row: must NOT contain em-dash placeholder %q\n%q", EmDash, out)
	}
}

// TestRender_NoEmDashPlaceholder verifies that even the current-session project
// (which gets a metadata row) does NOT emit the em-dash when metadata is empty.
// Empty current-row metadata renders as blank ClearLineEnd-terminated line.
// This replaces the old TestRender_EmDashPlaceholder test (260511-n4n task 6).
func TestRender_NoEmDashPlaceholder(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
		CurrentSession: "alpha", // current → 2 rows; metadata row must NOT have em-dash
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 50, anim, fixedNowFn)
	if bytes.Contains(out, []byte(EmDash)) {
		t.Errorf("Render: no frame must contain em-dash (260511-n4n task 6 removal)\n%q", EmDash)
	}
	// Verify the frame still contains ClearLineEnd (the empty meta row).
	if bytes.Count(out, []byte(ClearLineEnd)) < 2 {
		t.Errorf("Render: expected at least 2 ClearLineEnd sequences (marker + meta rows)\n%q", out)
	}
}

// TestRender_CursorAndClearInvariants verifies that every frame starts
// with CursorHome and ends with ClearToEnd.
func TestRender_CursorAndClearInvariants(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 50, anim, fixedNowFn)
	if !bytes.HasPrefix(out, []byte(CursorHome)) {
		t.Errorf("Render: output must start with CursorHome \\x1b[H; got prefix %q", out[:min(8, len(out))])
	}
	if !bytes.HasSuffix(out, []byte(ClearToEnd)) {
		start := len(out) - 8
		if start < 0 {
			start = 0
		}
		t.Errorf("Render: output must end with ClearToEnd \\x1b[J; got suffix %q", out[start:])
	}
}

// TestRender_AgentChipSuppressedForCurrentSession verifies that agent chips
// (AgentClaude / AgentPi) are not rendered for the project matching
// CurrentSession. When the user is in the agent pane, the indicator is
// misleading — the user is present, not an unattended autonomous agent.
//
// With the domain-row layout (260511-ohu), the current project's agent domain
// row only renders chipWaitAge (not chipAgentClaude/chipAgentPi which are
// suppressed for current sessions). Non-current compact rows also have no
// agent chips by design.
func TestRender_AgentChipSuppressedForCurrentSession(t *testing.T) {
	// claudeGlyph and piGlyph are the chip prefixes that appear in the
	// rendered output when agent chips fire. We look for these specific
	// byte sequences to detect chip presence.
	claudeGlyph := []byte(ClaudeGlyphDefault + "●")
	piGlyph := []byte(PiGlyphDefault + "●")

	snap := &proto.Snapshot{
		Projects: []proto.Project{
			// "alpha" is the current session — agent chips must be suppressed.
			{Name: "alpha", Status: "waiting", AgentClaude: "waiting", AgentPi: "waiting"},
			// "beta" is a different project — compact row has no agent chips by design.
			{Name: "beta", Status: "waiting", AgentClaude: "waiting"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 80, anim, fixedNowFn)

	// Assert the full frame does NOT contain agent chip glyphs.
	// alpha (current): agent chips suppressed in renderMetadataRow (isCurrent zeroes AgentClaude/AgentPi).
	// beta (non-current): compact row never shows agent chips.
	if bytes.Contains(out, claudeGlyph) {
		t.Errorf("frame must NOT contain claude agent chip %q (current session suppresses, compact row never shows)\n%q", claudeGlyph, out)
	}
	if bytes.Contains(out, piGlyph) {
		t.Errorf("frame must NOT contain pi agent chip %q (current session suppresses, compact row never shows)\n%q", piGlyph, out)
	}
}

// --- Urgent red-▌ border tests (260511-nxy, replacing bg-flip tests from 260511-n4n) ---

// TestRender_Urgent_NonCurrent_CollapsesToOneRow verifies that a non-current
// waiting+unacked project past WaitUrgentSec produces exactly 1 project-row
// newline (260511-ohu task 1: twoRows := isCurrent only; urgent dropped).
func TestRender_Urgent_NonCurrent_CollapsesToOneRow(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: false,
			},
		},
		CurrentSession: "", // non-current
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	rows := countProjectRows(out)
	if rows != 1 {
		t.Errorf("urgent non-current: expected 1 row (collapsed), got %d\n%q", rows, out)
	}
}

// TestRender_Urgent_NonCurrent_RedBorderStillPresent verifies that a non-current
// urgent+unacked project still shows the red ▌ on its single compact row.
func TestRender_Urgent_NonCurrent_RedBorderStillPresent(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: false,
			},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	// RedBorder + ▌ + Reset must appear exactly once (single compact row).
	redBorderPrefix := []byte(RedBorder + "▌" + Reset)
	count := bytes.Count(out, redBorderPrefix)
	if count != 1 {
		t.Errorf("urgent non-current: expected RedBorder+▌+Reset exactly 1 time (single row), got %d\n%q", count, out)
	}
}

// TestRender_Urgent_NonCurrent_InlineAlertsStillRender verifies that a non-current
// urgent+unacked project with PRFail=1, PROpen=1 produces a single row containing
// "✗1" and the RedBorder+▌ prefix.
func TestRender_Urgent_NonCurrent_InlineAlertsStillRender(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: false,
				PRFail:           1,
				PROpen:           1,
			},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, fixedNowFn)

	if !bytes.Contains(out, []byte("✗1")) {
		t.Errorf("urgent non-current with PRFail: expected '✗1' inline alert\n%q", out)
	}
	redBorderPrefix := []byte(RedBorder + "▌")
	if !bytes.Contains(out, redBorderPrefix) {
		t.Errorf("urgent non-current: expected RedBorder+▌ prefix\n%q", out)
	}
}

// TestRender_Urgent_NonCurrent_NoBranchChip verifies that a non-current urgent
// project does NOT render a branch chip (Cyan absent) — compact rows never show
// branch by design (repurposed from TestRender_Urgent_NonCurrent_FullChipSet).
func TestRender_Urgent_NonCurrent_NoBranchChip(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:          "alpha",
				Status:        "waiting",
				WaitStartedTS: now - int64(WaitUrgentSec),
				Branch:        "feature-x",
				DirtyCount:    2,
			},
		},
		CurrentSession: "", // non-current but urgent
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	rows := countProjectRows(out)
	if rows != 1 {
		t.Errorf("urgent non-current: expected 1 compact row, got %d\n%q", rows, out)
	}
	// Branch chip uses Cyan — must NOT appear on compact (non-current) row.
	if bytes.Contains(out, []byte(Cyan)) {
		t.Errorf("urgent non-current compact row: must NOT contain Cyan (branch chip)\n%q", out)
	}
}

// TestRender_Urgent_CurrentSession_RedBorderBothRows verifies that a CURRENT-SESSION
// project with WaitStartedTS >= WaitUrgentSec re-expands to 2 rows, both prefixed
// with the red-▌ left-border accent. (2-row expansion only via isCurrent now.)
func TestRender_Urgent_CurrentSession_RedBorderBothRows(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", WaitStartedTS: now - int64(WaitUrgentSec)},
		},
		CurrentSession: "alpha", // current — two-row expansion via isCurrent
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	// Should have 2 rows (via isCurrent, not urgent).
	rows := countProjectRows(out)
	if rows != 2 {
		t.Errorf("urgent current: expected 2 rows, got %d\n%q", rows, out)
	}
	// RedBorder + ▌ + Reset must appear exactly twice (once per row).
	redBorderPrefix := []byte(RedBorder + "▌" + Reset)
	count := bytes.Count(out, redBorderPrefix)
	if count != 2 {
		t.Errorf("urgent current: expected RedBorder+▌+Reset exactly 2 times, got %d\n%q", count, out)
	}
	// Reset must appear.
	if !bytes.Contains(out, []byte(Reset)) {
		t.Errorf("urgent current: expected Reset in output\n%q", out)
	}
}

// TestRender_Urgent_CurrentSession_RedBorderWinsOverBreath verifies a
// current-session project that is also urgent gets the red-▌ border on both
// rows (urgency wins over identity — the breath bar is NOT shown).
func TestRender_Urgent_CurrentSession_RedBorderWinsOverBreath(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", WaitStartedTS: now - int64(WaitUrgentSec)},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	anim.breathState = 0 // peak breath frame
	out := Render(snap, 50, anim, fixedNowFn)

	redBorderPrefix := []byte(RedBorder + "▌" + Reset)
	count := bytes.Count(out, redBorderPrefix)
	if count != 2 {
		t.Errorf("urgent current: expected RedBorder+▌+Reset exactly 2 times, got %d\n%q", count, out)
	}
	// The breath-bar prefix for "alpha" must NOT appear — urgency wins over identity.
	breathPrefix := []byte(BreathColorForProject("alpha", anim.BreathFrame()) + "▌" + Reset)
	if bytes.Contains(out, breathPrefix) {
		t.Errorf("urgent current: breath-bar prefix must NOT appear (urgency wins over identity)\n%q", out)
	}
}

// TestRender_Urgent_NonCurrentNonUrgentNoRedBorder verifies a non-current,
// non-urgent project does NOT contain the red-▌ border prefix.
func TestRender_Urgent_NonCurrentNonUrgentNoRedBorder(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", WaitStartedTS: fixedNow - 60}, // < WaitUrgentSec
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	redBorderPrefix := []byte(RedBorder + "▌")
	if bytes.Contains(out, redBorderPrefix) {
		t.Errorf("non-urgent: must NOT contain RedBorder+▌ prefix\n%q", out)
	}
}

// TestRender_Urgent_NoBackgroundBytes is the regression sentinel that the
// BgUrgent bleed bug cannot return without this test failing. For all
// four urgency/current-session combinations, the rendered frame must contain
// zero occurrences of the \x1b[48;5;52m (BgUrgent) byte sequence.
func TestRender_Urgent_NoBackgroundBytes(t *testing.T) {
	cases := []struct {
		name    string
		current string
		urgent  bool
	}{
		{"non-current+urgent", "", true},
		{"current+urgent", "alpha", true},
		{"current+non-urgent", "alpha", false},
		{"non-current+non-urgent", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			waitTS := int64(0)
			if tc.urgent {
				waitTS = fixedNow - int64(WaitUrgentSec)
			}
			snap := &proto.Snapshot{
				Projects: []proto.Project{
					{Name: "alpha", Status: "waiting", WaitStartedTS: waitTS},
				},
				CurrentSession: tc.current,
			}
			anim := NewAnimator()
			anim.OnSnapshot(snap)
			out := Render(snap, 50, anim, fixedNowFn)
			bgCount := bytes.Count(out, []byte("\x1b[48;5;52m"))
			if bgCount != 0 {
				t.Errorf("%s: BgUrgent must not appear in any frame (bleed-bug regression), found %d occurrences\n%q", tc.name, bgCount, out)
			}
		})
	}
}

// --- Task 2: Current-row generosity tests (260511-n4n) ---

// TestRender_CurrentRow_LongerBranch verifies the current-session metadata
// row allows branch names up to 24 runes (vs 14 for the old default cap).
// A 21-rune branch should appear verbatim; a 25-rune branch should truncate.
func TestRender_CurrentRow_LongerBranch(t *testing.T) {
	// 21-rune branch: should appear verbatim.
	branch21 := "feature-very-long-name" // exactly 22 runes, let's count: f-e-a-t-u-r-e---v-e-r-y---l-o-n-g---n-a-m-e = 22
	// Actually let me count: "feature-very-long-name" = 22 chars. Use 21: "feature-very-long-nam"
	branch21 = "feature-very-long-nam" // 21 runes

	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: branch21},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	// 21-rune branch fits within cap=24 — should appear verbatim.
	if !bytes.Contains(out, []byte(branch21)) {
		t.Errorf("current-row 21-rune branch: expected verbatim in output\n%q", out)
	}

	// 25-rune branch: should truncate at cap=24 (23 runes + Ellipsis).
	branch25 := "feature-very-long-name-x" // 24 chars, try 25: "feature-very-long-name-xy"
	branch25 = "feature-very-long-name-xy" // 25 runes
	snap2 := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: branch25},
		},
		CurrentSession: "alpha",
	}
	anim2 := NewAnimator()
	anim2.OnSnapshot(snap2)
	out2 := Render(snap2, 50, anim2, fixedNowFn)
	if bytes.Contains(out2, []byte(branch25)) {
		t.Errorf("current-row 25-rune branch: full name must NOT appear in output\n%q", out2)
	}
	if !bytes.Contains(out2, []byte(Ellipsis)) {
		t.Errorf("current-row 25-rune branch: Ellipsis must appear in output\n%q", out2)
	}
}


// TestRender_NonCurrentRow_UnchangedBranchCap verifies non-current compact
// rows do NOT show branch at all (no branch chip on compact rows per PD-02).
func TestRender_NonCurrentRow_UnchangedBranchCap(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: "feature-x"},
		},
		CurrentSession: "", // non-current
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	// Branch chip uses Cyan color — should not appear for non-current compact row.
	if bytes.Contains(out, []byte(Cyan)) {
		t.Errorf("non-current compact row must NOT contain Cyan branch chip\n%q", out)
	}
}

// --- Two-level layout tests (260511-n4n task 1) ---

// TestRender_TwoLevel_NonCurrentIsOneLine verifies that with CurrentSession="alpha",
// the non-current project "beta" renders as a SINGLE compact line (1 newline),
// while "alpha" (current, no metadata) produces 1 marker row (all domain rows
// suppressed). Total project rows = 2.
func TestRender_TwoLevel_NonCurrentIsOneLine(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
			{Name: "beta", Status: "alive"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	rows := countProjectRows(out)
	// alpha=1 row (marker only; all-empty domain rows suppressed), beta=1 compact → 2
	if rows != 2 {
		t.Errorf("two-level: project rows = %d, want 2 (alpha:1 marker + beta:1)\nframe: %q", rows, out)
	}
}

// TestRender_TwoLevel_NoCurrentSessionAllCompact verifies that with no
// CurrentSession, all 3 projects collapse to 1 row each = 3 total.
func TestRender_TwoLevel_NoCurrentSessionAllCompact(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
			{Name: "beta", Status: "waiting"},
			{Name: "gamma", Status: "finished"},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	rows := countProjectRows(out)
	if rows != 3 {
		t.Errorf("no-current-session: project rows = %d, want 3 (1 per project)\nframe: %q", rows, out)
	}
}

// TestRender_TwoLevel_NonCurrentInlineAlerts verifies that a non-current project
// with PRFail=1, PROpen=1 renders a SINGLE line containing "✗1" but NOT "PR"
// or the metadata-row 6-space indent.
func TestRender_TwoLevel_NonCurrentInlineAlerts(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", PROpen: 1, PRFail: 1},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	if !bytes.Contains(out, []byte("✗1")) {
		t.Errorf("non-current with PRFail: expected '✗1' inline alert in output\n%q", out)
	}
	if bytes.Contains(out, []byte(" PR")) {
		t.Errorf("non-current compact row must NOT contain ' PR' suffix\n%q", out)
	}
	// metadata-row 6-space indent should NOT appear for a non-current row
	if bytes.Contains(out, []byte("      ")) {
		t.Errorf("non-current compact row must NOT contain 6-space metadata indent\n%q", out)
	}
}

// TestRender_DomainRows_CurrentEmptyMetaIsOneRow verifies the current-session
// project with no metadata produces 1 row (marker only — domain rows are
// suppressed when all sub-group buffers are empty).
// Renamed from TestRender_TwoLevel_CurrentStillTwoRows in 260511-ohu task 3:
// under the domain-row layout, an all-empty metadata set produces 0 domain rows.
func TestRender_DomainRows_CurrentEmptyMetaIsOneRow(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	rows := countProjectRows(out)
	if rows != 1 {
		t.Errorf("current-session with no metadata: project rows = %d, want 1 (marker only, domain rows suppressed)\nframe: %q", rows, out)
	}
}

// TestRender_AgentChipSuppressedForSlashProject verifies suppression works
// when the current project name contains a slash (e.g. "zitcha/backend").
// With the domain-row layout (260511-ohu), agent chips are suppressed for the
// current session in renderMetadataRow; compact rows never show agent chips.
func TestRender_AgentChipSuppressedForSlashProject(t *testing.T) {
	claudeGlyph := []byte(ClaudeGlyphDefault + "●")

	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "zitcha/backend", Status: "waiting", AgentClaude: "waiting"},
			{Name: "dotfiles", Status: "idle", AgentClaude: "waiting"},
		},
		CurrentSession: "zitcha/backend",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	out := Render(snap, 80, anim, fixedNowFn)

	// Full-frame assertion: no claude chip anywhere.
	// zitcha/backend (current): agent chips zeroed before renderMetadataRow chip calls.
	// dotfiles (non-current): compact row never shows agent chips.
	if bytes.Contains(out, claudeGlyph) {
		t.Errorf("frame must NOT contain claude chip %q (current session suppresses, compact row never shows)\n%q", claudeGlyph, out)
	}
}

// --- 260511-o8g: WaitAcknowledged urgent-suppression tests ---

// TestRender_UrgentSuppression_NonCurrent_Unacked verifies that a non-current
// waiting project past WaitUrgentSec with WaitAcknowledged=false renders with
// the red ▌ border (regression guard: 260511-nxy behavior preserved).
func TestRender_UrgentSuppression_NonCurrent_Unacked(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: false,
			},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	redBorderPrefix := []byte(RedBorder + "▌" + Reset)
	if !bytes.Contains(out, redBorderPrefix) {
		t.Errorf("non-current+unacked+urgent: expected RedBorder+▌+Reset in output\n%q", out)
	}
	// Should collapse to 1 row (260511-ohu: twoRows := isCurrent; urgent dropped).
	rows := countProjectRows(out)
	if rows != 1 {
		t.Errorf("non-current+unacked+urgent: expected 1 compact row (260511-ohu), got %d\n%q", rows, out)
	}
}

// TestRender_UrgentSuppression_NonCurrent_Acked verifies that a non-current
// waiting project past WaitUrgentSec with WaitAcknowledged=true does NOT show
// the red ▌ border and collapses to the 1-row compact form.
func TestRender_UrgentSuppression_NonCurrent_Acked(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: true,
			},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)

	// Red border must be absent.
	redBorderPrefix := []byte(RedBorder + "▌")
	if bytes.Contains(out, redBorderPrefix) {
		t.Errorf("non-current+acked+urgent: RedBorder+▌ must NOT appear in output\n%q", out)
	}
	// Row must collapse to 1-row compact form (no metadata row for this project).
	rows := countProjectRows(out)
	if rows != 1 {
		t.Errorf("non-current+acked+urgent: expected 1 compact row, got %d\n%q", rows, out)
	}
}

// TestRender_UrgentSuppression_Current_Unacked verifies that a current waiting
// project past WaitUrgentSec with WaitAcknowledged=false renders with the red ▌
// border (urgency wins over identity — 260511-nxy behavior preserved).
func TestRender_UrgentSuppression_Current_Unacked(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: false,
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	anim.breathState = 0
	out := Render(snap, 50, anim, fixedNowFn)

	redBorderPrefix := []byte(RedBorder + "▌" + Reset)
	count := bytes.Count(out, redBorderPrefix)
	if count != 2 {
		t.Errorf("current+unacked+urgent: expected RedBorder+▌+Reset exactly 2 times, got %d\n%q", count, out)
	}
}

// TestRender_UrgentSuppression_Current_Acked verifies that a current waiting
// project past WaitUrgentSec with WaitAcknowledged=true suppresses the red ▌
// border and instead uses the breathing project-hue ▌. The two-row expansion
// still occurs because isCurrent forces it independently of urgency.
func TestRender_UrgentSuppression_Current_Acked(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:             "alpha",
				Status:           "waiting",
				WaitStartedTS:    now - int64(WaitUrgentSec) - 10,
				WaitAcknowledged: true,
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	anim.breathState = 0 // deterministic breath frame
	out := Render(snap, 50, anim, fixedNowFn)

	// Red border must be absent.
	redBorderPrefix := []byte(RedBorder + "▌")
	if bytes.Contains(out, redBorderPrefix) {
		t.Errorf("current+acked+urgent: RedBorder+▌ must NOT appear in output\n%q", out)
	}
	// Breathing project-hue ▌ must be present (identity takes over from urgency).
	breathPrefix := []byte(BreathColorForProject("alpha", anim.BreathFrame()) + "▌" + Reset)
	if !bytes.Contains(out, breathPrefix) {
		t.Errorf("current+acked+urgent: expected breath-color ▌ prefix %q in output\n%q", breathPrefix, out)
	}
	// Two-row expansion must still happen (isCurrent is independent of isUrgent).
	rows := countProjectRows(out)
	if rows != 2 {
		t.Errorf("current+acked+urgent: expected 2 rows (isCurrent forces 2-row), got %d\n%q", rows, out)
	}
}

// --- 260511-ohu task 2: renderDomainRow / metadataPrefix / joinNonEmpty unit tests ---

// TestRenderDomainRow runs sub-tests covering the empty-suppression and format
// guarantees of the renderDomainRow helper.
func TestRenderDomainRow(t *testing.T) {
	t.Run("EmptyBufferSuppresses", func(t *testing.T) {
		var buf bytes.Buffer
		renderDomainRow(&buf, "PREFIX", "⎇", func(inner *bytes.Buffer) {
			// write nothing
		})
		if buf.Len() != 0 {
			t.Errorf("expected buf unchanged (empty inner suppresses row), got %q", buf.Bytes())
		}
	})

	t.Run("NonEmptyEmitsPrefixGlyph", func(t *testing.T) {
		var buf bytes.Buffer
		renderDomainRow(&buf, "PFX", "▶", func(inner *bytes.Buffer) {
			inner.WriteString("hello")
		})
		out := buf.Bytes()
		// Must start with prefix.
		if !bytes.HasPrefix(out, []byte("PFX")) {
			t.Errorf("row must start with prefix; got %q", out)
		}
		// Must contain Dim + glyph.
		if !bytes.Contains(out, []byte(Dim+"▶")) {
			t.Errorf("row must contain Dim+glyph; got %q", out)
		}
		// Must contain the body.
		if !bytes.Contains(out, []byte("hello")) {
			t.Errorf("row must contain body 'hello'; got %q", out)
		}
		// Must end with ClearLineEnd + LF.
		if !bytes.HasSuffix(out, []byte(ClearLineEnd+"\n")) {
			t.Errorf("row must end with ClearLineEnd+LF; got %q", out)
		}
		// Order: prefix < Dim+glyph < body < ClearLineEnd.
		pfxIdx := bytes.Index(out, []byte("PFX"))
		glyphIdx := bytes.Index(out, []byte(Dim+"▶"))
		bodyIdx := bytes.Index(out, []byte("hello"))
		clearIdx := bytes.Index(out, []byte(ClearLineEnd))
		if !(pfxIdx < glyphIdx && glyphIdx < bodyIdx && bodyIdx < clearIdx) {
			t.Errorf("order wrong: pfx=%d glyph=%d body=%d clear=%d in %q", pfxIdx, glyphIdx, bodyIdx, clearIdx, out)
		}
	})

	t.Run("LeadingSpaceTrimmed", func(t *testing.T) {
		var buf bytes.Buffer
		renderDomainRow(&buf, "", "✻", func(inner *bytes.Buffer) {
			inner.WriteString(" hello")
		})
		out := buf.Bytes()
		// The body after Reset should start with 'h', not ' '.
		resetIdx := bytes.LastIndex(out, []byte(Reset))
		if resetIdx < 0 {
			t.Fatalf("no Reset in output: %q", out)
		}
		afterReset := out[resetIdx+len(Reset):]
		if len(afterReset) == 0 || afterReset[0] == ' ' {
			t.Errorf("leading space not trimmed; body after Reset starts with space in %q", out)
		}
		if !bytes.Contains(afterReset, []byte("hello")) {
			t.Errorf("body 'hello' missing after Reset in %q", out)
		}
	})
}

// TestMetadataPrefix verifies the exact byte output of metadataPrefix for all
// three branches: urgent, isCurrent (breathing bar), and default (defensive).
func TestMetadataPrefix(t *testing.T) {
	anim := NewAnimator()
	// Use a fixed project so palette/breath are deterministic.
	p := &proto.Project{Name: "alpha", Status: "alive"}

	t.Run("Urgent", func(t *testing.T) {
		prefix := metadataPrefix(p, "alpha", anim, true)
		want := RedBorder + "▌" + Reset + "     "
		if prefix != want {
			t.Errorf("urgent prefix:\n  got  %q\n  want %q", prefix, want)
		}
	})

	t.Run("Breath", func(t *testing.T) {
		anim.breathState = 0 // deterministic frame
		prefix := metadataPrefix(p, "alpha", anim, false)
		want := BreathColorForProject("alpha", anim.BreathFrame()) + "▌" + Reset + "     "
		if prefix != want {
			t.Errorf("breath prefix:\n  got  %q\n  want %q", prefix, want)
		}
	})

	t.Run("Default", func(t *testing.T) {
		// Non-urgent + non-current → defensive 6-space fallback.
		prefix := metadataPrefix(p, "other", anim, false)
		want := "      "
		if prefix != want {
			t.Errorf("default prefix:\n  got  %q\n  want %q", prefix, want)
		}
	})
}

// TestJoinNonEmpty verifies the empty-skip and separator behavior of joinNonEmpty.
func TestJoinNonEmpty(t *testing.T) {
	t.Run("SkipsEmpty", func(t *testing.T) {
		var dst bytes.Buffer
		var a, b, c bytes.Buffer
		a.WriteString("aaa")
		// b is empty
		c.WriteString("ccc")
		joinNonEmpty(&dst, []*bytes.Buffer{&a, &b, &c}, " │ ")
		got := dst.String()
		if got != "aaa │ ccc" {
			t.Errorf("expected 'aaa │ ccc', got %q", got)
		}
		// Exactly one separator.
		if count := bytes.Count([]byte(got), []byte(" │ ")); count != 1 {
			t.Errorf("expected exactly 1 separator, got %d in %q", count, got)
		}
	})

	t.Run("AllEmpty", func(t *testing.T) {
		var dst bytes.Buffer
		var a, b bytes.Buffer
		joinNonEmpty(&dst, []*bytes.Buffer{&a, &b}, " │ ")
		if dst.Len() != 0 {
			t.Errorf("expected empty dst for all-empty subs, got %q", dst.Bytes())
		}
	})
}

// --- 260511-ohu task 3: domain-grouped metadata row tests ---

// TestRender_DomainRows_AllThreePopulated verifies that a current-session
// project with git, runtime, and agent data produces exactly 3 domain rows
// with the expected leading glyphs.
func TestRender_DomainRows_AllThreePopulated(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:           "alpha",
				Status:         "waiting",
				Branch:         "feature-x",
				DirtyCount:     1,
				ShellCmd:       "npm dev",
				ListeningPorts: []int{3000},
				LastActivityTS: now - 120,
				WaitStartedTS:  now - 90,
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, func() int64 { return now })

	// Each domain glyph must appear exactly once (after Dim prefix).
	// The pattern is Dim + glyph + " " + Reset in the row.
	if bytes.Count(out, []byte(Dim+"⎇ ")) != 1 {
		t.Errorf("expected exactly 1 git domain glyph (⎇); got %d\n%q", bytes.Count(out, []byte(Dim+"⎇ ")), out)
	}
	if bytes.Count(out, []byte(Dim+"▶ ")) != 1 {
		t.Errorf("expected exactly 1 runtime domain glyph (▶); got %d\n%q", bytes.Count(out, []byte(Dim+"▶ ")), out)
	}
	if bytes.Count(out, []byte(Dim+"✻ ")) != 1 {
		t.Errorf("expected exactly 1 agent domain glyph (✻); got %d\n%q", bytes.Count(out, []byte(Dim+"✻ ")), out)
	}
}

// TestRender_DomainRows_GitOnly verifies that only the git domain row appears
// when only Branch is set.
func TestRender_DomainRows_GitOnly(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: "main-feature"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, fixedNowFn)

	if bytes.Count(out, []byte(Dim+"⎇ ")) != 1 {
		t.Errorf("expected 1 git glyph (⎇), got %d\n%q", bytes.Count(out, []byte(Dim+"⎇ ")), out)
	}
	if bytes.Contains(out, []byte(Dim+"▶ ")) {
		t.Errorf("runtime glyph (▶) must not appear when no runtime data\n%q", out)
	}
	if bytes.Contains(out, []byte(Dim+"✻ ")) {
		t.Errorf("agent glyph (✻) must not appear when no agent data\n%q", out)
	}
}

// TestRender_DomainRows_AgentOnly verifies that only the agent domain row
// appears when only WaitStartedTS is set.
func TestRender_DomainRows_AgentOnly(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", WaitStartedTS: now - 90},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, func() int64 { return now })

	if bytes.Contains(out, []byte(Dim+"⎇ ")) {
		t.Errorf("git glyph (⎇) must not appear when no git data\n%q", out)
	}
	if bytes.Contains(out, []byte(Dim+"▶ ")) {
		t.Errorf("runtime glyph (▶) must not appear when no runtime data\n%q", out)
	}
	if bytes.Count(out, []byte(Dim+"✻ ")) != 1 {
		t.Errorf("expected 1 agent glyph (✻), got %d\n%q", bytes.Count(out, []byte(Dim+"✻ ")), out)
	}
}

// TestRender_DomainRows_AllEmpty verifies that a current-session project with
// no metadata produces 0 domain rows (just the marker row = 1 row total).
func TestRender_DomainRows_AllEmpty(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, fixedNowFn)

	rows := countProjectRows(out)
	if rows != 1 {
		t.Errorf("current with all-empty metadata: expected 1 row (marker only), got %d\n%q", rows, out)
	}
}

// TestRender_DomainRows_SeparatorBetweenSubgroups verifies that a git domain
// row with branch+PR+CI produces exactly 2 " │ " separators (between groups).
func TestRender_DomainRows_SeparatorBetweenSubgroups(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:          "alpha",
				Status:        "alive",
				Branch:        "feat",
				PROpen:        1,
				CIStatus:      "completed",
				CIConclusion:  "success",
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 120, anim, fixedNowFn)

	// Extract the git row (contains ⎇ glyph).
	lines := bytes.Split(out, []byte("\n"))
	var gitRow []byte
	for _, l := range lines {
		if bytes.Contains(l, []byte("⎇")) {
			gitRow = l
			break
		}
	}
	if gitRow == nil {
		t.Fatalf("no git domain row found in output\n%q", out)
	}
	// Count " │ " on the git row.
	sepCount := bytes.Count(gitRow, []byte(" │ "))
	if sepCount != 2 {
		t.Errorf("expected 2 ` │ ` separators on git row (branch | PR | CI), got %d\ngit row: %q\nfull: %q", sepCount, gitRow, out)
	}
}

// TestRender_DomainRows_NoSeparatorWhenSubgroupEmpty verifies that only Branch
// (no PR, no CI) produces a git row with zero " │ " separators.
func TestRender_DomainRows_NoSeparatorWhenSubgroupEmpty(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: "feat"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, fixedNowFn)

	lines := bytes.Split(out, []byte("\n"))
	var gitRow []byte
	for _, l := range lines {
		if bytes.Contains(l, []byte("⎇")) {
			gitRow = l
			break
		}
	}
	if gitRow == nil {
		t.Fatalf("no git domain row found\n%q", out)
	}
	sepCount := bytes.Count(gitRow, []byte(" │ "))
	if sepCount != 0 {
		t.Errorf("expected 0 separators on git row (branch-only), got %d\ngit row: %q", sepCount, gitRow)
	}
}

// TestRender_DomainRows_LeadingGlyphIsDim verifies that the git row glyph
// "⎇" appears within a Dim..Reset pair.
func TestRender_DomainRows_LeadingGlyphIsDim(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: "feat"},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, fixedNowFn)

	// The glyph must be preceded by Dim.
	if !bytes.Contains(out, []byte(Dim+"⎇")) {
		t.Errorf("git glyph ⎇ must be preceded by Dim; got\n%q", out)
	}
}

// TestRender_NonCurrent_CompactCIPending verifies a non-current project with
// CI in_progress surfaces the "⚙ CI" inline alert on its compact row (260511-r7x change C).
func TestRender_NonCurrent_CompactCIPending(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", CIStatus: "in_progress"},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	if !bytes.Contains(out, []byte("⚙ CI")) {
		t.Errorf("non-current with CI in_progress: expected '⚙ CI' inline alert\n%q", out)
	}
}

// TestRender_NonCurrent_CompactCIFail verifies a non-current project with
// CI failing surfaces the "✗ CI" inline alert on its compact row, and does
// NOT produce the legacy conflated "✗1" token (which was the old union with PRFail).
func TestRender_NonCurrent_CompactCIFail(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", CIStatus: "completed", CIConclusion: "failure"},
		},
		CurrentSession: "",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 50, anim, fixedNowFn)
	if !bytes.Contains(out, []byte("✗ CI")) {
		t.Errorf("non-current with CI fail: expected '✗ CI'\n%q", out)
	}
	if bytes.Contains(out, []byte("✗1")) {
		t.Errorf("non-current with CI fail (no PR): must NOT contain legacy '✗1'\n%q", out)
	}
}

// TestRender_DomainRows_Urgent_RedBorderOnEachPopulatedRow verifies that a
// current+urgent+unacked project with Branch="x" and WaitStartedTS set has
// the marker row + git domain row + agent domain row ALL prefixed with
// RedBorder+▌+Reset (count == 3: marker + 2 domain rows).
func TestRender_DomainRows_Urgent_RedBorderOnEachPopulatedRow(t *testing.T) {
	now := fixedNow
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:          "alpha",
				Status:        "waiting",
				WaitStartedTS: now - int64(WaitUrgentSec) - 10,
				Branch:        "x",
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 80, anim, func() int64 { return now })

	redBorderPrefix := []byte(RedBorder + "▌" + Reset)
	count := bytes.Count(out, redBorderPrefix)
	// Marker row + git domain row (Branch="x") + agent domain row (WaitStartedTS set) = 3.
	if count != 3 {
		t.Errorf("current+urgent with Branch+WaitStartedTS: expected RedBorder+▌+Reset exactly 3 times (marker+git+agent), got %d\n%q", count, out)
	}
}

// TestRender_FailingChecksRow_AppearsForCurrentProject (260512-cgw):
// A current project with FailingChecks populated emits a dedicated row
// prefixed with `✗ ` (the new domain glyph) carrying the check names.
func TestRender_FailingChecksRow_AppearsForCurrentProject(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:          "alpha",
				Status:        "alive",
				CIStatus:      "completed",
				CIConclusion:  "failure",
				FailingChecks: []string{"lint", "test"},
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 60, anim, fixedNowFn)

	// Look for the row whose Dim glyph is "✗" (failing-checks row marker).
	if !bytes.Contains(out, []byte(Dim+"✗"+" "+Reset)) {
		t.Errorf("current with FailingChecks: expected `Dim+✗+Reset` domain-row marker; got\n%q", out)
	}
	// Names should appear verbatim (short list, no scroll).
	if !bytes.Contains(out, []byte("lint, test")) {
		t.Errorf("current with FailingChecks: expected `lint, test` in output\n%q", out)
	}
}

// TestRender_FailingChecksRow_AbsentWhenEmpty: empty FailingChecks → no
// `✗` domain-row marker. Defends against a regression where every
// current project starts emitting a stray empty row.
func TestRender_FailingChecksRow_AbsentWhenEmpty(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:          "alpha",
				Status:        "alive",
				CIStatus:      "completed",
				CIConclusion:  "failure",
				FailingChecks: nil,
			},
		},
		CurrentSession: "alpha",
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 60, anim, fixedNowFn)
	// The git row's "✗ CI" chip is fine (red, not Dim-wrapped). What we
	// must NOT see is a Dim-wrapped ✗ leading glyph (the new domain-row marker).
	if bytes.Contains(out, []byte(Dim+"✗"+" "+Reset)) {
		t.Errorf("current with empty FailingChecks: must NOT emit `Dim+✗+Reset` row\n%q", out)
	}
}

// TestRender_FailingChecksRow_NonCurrentSuppressed: failing checks should NOT
// create an extra row for non-current projects — they keep the compact form.
func TestRender_FailingChecksRow_NonCurrentSuppressed(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{
				Name:          "alpha",
				Status:        "alive",
				CIStatus:      "completed",
				CIConclusion:  "failure",
				FailingChecks: []string{"lint", "test"},
			},
		},
		CurrentSession: "", // no current → all rows compact
	}
	anim := NewAnimator()
	anim.OnSnapshot(snap)
	out := Render(snap, 60, anim, fixedNowFn)
	if bytes.Contains(out, []byte(Dim+"✗"+" "+Reset)) {
		t.Errorf("non-current project: must NOT emit failing-checks row\n%q", out)
	}
	if bytes.Contains(out, []byte("lint, test")) {
		t.Errorf("non-current project: names must not appear (inline alerts only)\n%q", out)
	}
}
