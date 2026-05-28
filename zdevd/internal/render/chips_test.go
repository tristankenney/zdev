package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// ---- chipBranch ----

func TestChipBranch_Empty(t *testing.T) {
	var buf bytes.Buffer
	chipBranch(&buf, "", 0, 0)
	if buf.Len() != 0 {
		t.Errorf("want empty output for empty branch, got %q", buf.Bytes())
	}
}

func TestChipBranch_DefaultBranch(t *testing.T) {
	for _, branch := range []string{"main", "master", "develop", "trunk"} {
		var buf bytes.Buffer
		chipBranch(&buf, branch, 2, 1)
		if buf.Len() != 0 {
			t.Errorf("default branch %q should be suppressed, got %q", branch, buf.Bytes())
		}
	}
}

func TestChipBranch_NoDeltas(t *testing.T) {
	var buf bytes.Buffer
	chipBranch(&buf, "feature-x", 0, 0)
	want := []byte(Cyan + "feature-x" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipBranch no-deltas\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipBranch_AheadOnly(t *testing.T) {
	var buf bytes.Buffer
	chipBranch(&buf, "feature-x", 3, 0)
	want := []byte(Cyan + "feature-x ↑3" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipBranch ahead-only\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipBranch_BehindOnly(t *testing.T) {
	var buf bytes.Buffer
	chipBranch(&buf, "feature-x", 0, 2)
	want := []byte(Cyan + "feature-x ↓2" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipBranch behind-only\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipBranch_WithDeltas(t *testing.T) {
	var buf bytes.Buffer
	chipBranch(&buf, "feature-x", 2, 1)
	want := []byte(Cyan + "feature-x ↑2↓1" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipBranch with-deltas\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipBranch_Truncated(t *testing.T) {
	var buf bytes.Buffer
	chipBranch(&buf, "feature-x-with-a-very-long-name", 0, 0)
	want := []byte(Cyan + "feature-x-wit" + Ellipsis + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipBranch truncated\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// ---- chipDirty ----

func TestChipDirty_Zero(t *testing.T) {
	var buf bytes.Buffer
	chipDirty(&buf, 0)
	if buf.Len() != 0 {
		t.Errorf("chipDirty 0 should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipDirty_One(t *testing.T) {
	var buf bytes.Buffer
	chipDirty(&buf, 1)
	want := []byte(Orange + "+1" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipDirty 1\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipDirty_Five(t *testing.T) {
	var buf bytes.Buffer
	chipDirty(&buf, 5)
	want := []byte(Orange + "+5" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipDirty 5\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// ---- chipShellCmd ----

func TestChipShellCmd_Empty(t *testing.T) {
	var buf bytes.Buffer
	chipShellCmd(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("chipShellCmd empty should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipShellCmd_Short(t *testing.T) {
	var buf bytes.Buffer
	chipShellCmd(&buf, "npm test")
	want := []byte(Icy + "▶ npm test" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipShellCmd short\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipShellCmd_LongTruncated(t *testing.T) {
	var buf bytes.Buffer
	chipShellCmd(&buf, "very-long-command-here")
	want := []byte(Icy + "▶ very-long-com" + Ellipsis + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipShellCmd long\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// ---- chipPRAggregate ----

func TestChipPRAggregate_ZeroOpen(t *testing.T) {
	var buf bytes.Buffer
	chipPRAggregate(&buf, 0, 0, 0, false)
	if buf.Len() != 0 {
		t.Errorf("chipPRAggregate 0 open should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipPRAggregate_WithFail(t *testing.T) {
	var buf bytes.Buffer
	chipPRAggregate(&buf, 2, 1, 0, false)
	want := []byte("\x1b[31m✗ 1" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipPRAggregate with-fail\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipPRAggregate_WithPend(t *testing.T) {
	var buf bytes.Buffer
	chipPRAggregate(&buf, 2, 0, 1, false)
	want := []byte(Orange + "⊙ 1" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipPRAggregate with-pend\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipPRAggregate_AllSuccess(t *testing.T) {
	var buf bytes.Buffer
	chipPRAggregate(&buf, 2, 0, 0, false)
	want := []byte(Green + "✓ 2" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipPRAggregate all-success\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipPRAggregate_Celebrating(t *testing.T) {
	var buf bytes.Buffer
	chipPRAggregate(&buf, 2, 0, 0, true)
	if buf.Len() != 0 {
		t.Errorf("chipPRAggregate while celebrating should be suppressed, got %q", buf.Bytes())
	}
}

// ---- chipCelebrate ----

func TestChipCelebrate_InWindow(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	got := chipCelebrate(&buf, now+100, now)
	if !got {
		t.Errorf("chipCelebrate: expected true (in window)")
	}
	want := []byte(Bold + Green + "✨ merged" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipCelebrate in-window\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipCelebrate_Expired(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	got := chipCelebrate(&buf, now-1, now)
	if got {
		t.Errorf("chipCelebrate: expected false (expired)")
	}
	if buf.Len() != 0 {
		t.Errorf("chipCelebrate expired should produce no output, got %q", buf.Bytes())
	}
}

func TestChipCelebrate_AtExactBoundary(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	got := chipCelebrate(&buf, now, now) // celebrateUntil == now → expired
	if got {
		t.Errorf("chipCelebrate: expected false (at boundary celebrateUntil == now)")
	}
}

// ---- chipPorts ----

func TestChipPorts_Empty(t *testing.T) {
	var buf bytes.Buffer
	chipPorts(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("chipPorts empty should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipPorts_OnePort(t *testing.T) {
	var buf bytes.Buffer
	chipPorts(&buf, []int{3000})
	want := []byte(Dim + ":3000" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipPorts 1 port\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipPorts_FourPorts(t *testing.T) {
	var buf bytes.Buffer
	chipPorts(&buf, []int{3000, 3001, 3002, 3003})
	want := []byte(Dim + ":3000 :3001 :3002 :3003" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipPorts 4 ports\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipPorts_FivePortsTruncated(t *testing.T) {
	var buf bytes.Buffer
	chipPorts(&buf, []int{3000, 3001, 3002, 3003, 3004})
	want := []byte(Dim + ":3000 :3001 :3002 :3003" + Reset) // max 4
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipPorts 5 ports (truncated to 4)\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// ---- chipAgentClaude / chipAgentPi ----

func TestChipAgentClaude_Empty(t *testing.T) {
	var buf bytes.Buffer
	chipAgentClaude(&buf, "", ClaudeGlyphDefault)
	if buf.Len() != 0 {
		t.Errorf("chipAgentClaude empty should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipAgentClaude_Waiting(t *testing.T) {
	var buf bytes.Buffer
	chipAgentClaude(&buf, "waiting", ClaudeGlyphDefault)
	want := []byte(RedPulse + ClaudeGlyphDefault + "●" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipAgentClaude waiting\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipAgentClaude_Finished(t *testing.T) {
	var buf bytes.Buffer
	chipAgentClaude(&buf, "finished", ClaudeGlyphDefault)
	want := []byte(Yellow + ClaudeGlyphDefault + "◆" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipAgentClaude finished\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipAgentClaude_OverriddenGlyph(t *testing.T) {
	var buf bytes.Buffer
	chipAgentClaude(&buf, "waiting", "✦")
	want := []byte(RedPulse + "✦" + "●" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipAgentClaude overridden glyph\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipAgentPi_Empty(t *testing.T) {
	var buf bytes.Buffer
	chipAgentPi(&buf, "", PiGlyphDefault)
	if buf.Len() != 0 {
		t.Errorf("chipAgentPi empty should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipAgentPi_Waiting(t *testing.T) {
	var buf bytes.Buffer
	chipAgentPi(&buf, "waiting", PiGlyphDefault)
	want := []byte(RedPulse + PiGlyphDefault + "●" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipAgentPi waiting\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipAgentPi_Finished(t *testing.T) {
	var buf bytes.Buffer
	chipAgentPi(&buf, "finished", PiGlyphDefault)
	want := []byte(Yellow + PiGlyphDefault + "◆" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipAgentPi finished\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// ---- chipWaitAge ----

func TestChipWaitAge_Zero(t *testing.T) {
	var buf bytes.Buffer
	chipWaitAge(&buf, 0, 1000000)
	if buf.Len() != 0 {
		t.Errorf("chipWaitAge TS=0 should be suppressed, got %q", buf.Bytes())
	}
}

func TestChipWaitAge_30Seconds_Dim(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	chipWaitAge(&buf, now-30, now)
	want := []byte(Dim + "30s" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipWaitAge 30s (dim)\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipWaitAge_60Seconds_Orange(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	chipWaitAge(&buf, now-60, now)
	want := []byte(Orange + "1m" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipWaitAge 60s (orange)\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipWaitAge_299Seconds_Orange(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	chipWaitAge(&buf, now-299, now)
	want := []byte(Orange + "4m" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipWaitAge 299s (orange)\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// TestChipWaitAge_300Seconds_Red: urgent tier restored in 260511-nxy.
// Expects RedPulse + "! " prefix (removed in 260511-n4n, now back because
// the red-▌ left border replaces the BgUrgent bg fill as the row-level signal).
func TestChipWaitAge_300Seconds_Red(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	chipWaitAge(&buf, now-300, now)
	want := []byte(RedPulse + "! 5m" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipWaitAge 300s (urgent tier restored 260511-nxy)\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

func TestChipWaitAge_600Seconds_Red(t *testing.T) {
	now := int64(1000000)
	var buf bytes.Buffer
	chipWaitAge(&buf, now-600, now)
	want := []byte(RedPulse + "! 10m" + Reset)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("chipWaitAge 600s (urgent tier restored 260511-nxy)\nwant: %q\ngot:  %q", want, buf.Bytes())
	}
}

// ---- PaletteFor ----

func TestPaletteFor_KnownNames(t *testing.T) {
	// Verify PaletteFor delegates to PaletteIndex correctly.
	// These names are arbitrary; we just verify the output is a valid
	// ANSI escape from ProjectPalette (not an empty string or panic).
	for _, name := range []string{"alpha", "beta", "zitcha-frontend"} {
		got := PaletteFor(name)
		if got == "" {
			t.Errorf("PaletteFor(%q) returned empty string", name)
		}
		// Verify it's one of the 15 palette entries.
		found := false
		for _, p := range ProjectPalette {
			if p == got {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("PaletteFor(%q) = %q, not in ProjectPalette", name, got)
		}
	}
}

// ---- MarkerFor ----

func TestMarkerFor_StatusCoverage(t *testing.T) {
	anim := NewAnimator()
	tests := []struct {
		status    string
		wantGlyph string
		wantColor string
	}{
		{"waiting", anim.PulseGlyph(), RedPulse},
		{"shell-running", "◎", Icy},
		{"finished", "◆", Yellow},
		{"absent", "·", Dim},
		{"unknown", "·", Dim},
	}
	for _, tc := range tests {
		p := proto.Project{Name: "test", Status: tc.status}
		glyph, color := MarkerFor(p, anim)
		if glyph != tc.wantGlyph {
			t.Errorf("MarkerFor status=%q glyph: want %q, got %q", tc.status, tc.wantGlyph, glyph)
		}
		if color != tc.wantColor {
			t.Errorf("MarkerFor status=%q color: want %q, got %q", tc.status, tc.wantColor, color)
		}
	}
}

func TestMarkerFor_Alive_UsesPalette(t *testing.T) {
	anim := NewAnimator()
	p := proto.Project{Name: "myproject", Status: "alive"}
	_, color := MarkerFor(p, anim)
	expected := PaletteFor("myproject")
	if color != expected {
		t.Errorf("MarkerFor alive: want PaletteFor result %q, got %q", expected, color)
	}
}

// ---- MoodFor ----

func TestMoodFor_ThreePlusWaiting(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "waiting"},
			{Name: "b", Status: "waiting"},
			{Name: "c", Status: "waiting"},
		},
	}
	got := MoodFor(snap, func() int64 { return 1000000 })
	want := MoodRed + MoodBlock + Reset
	if got != want {
		t.Errorf("MoodFor 3+ waiting: want %q, got %q", want, got)
	}
}

func TestMoodFor_OneWaiting(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "waiting"},
		},
	}
	got := MoodFor(snap, func() int64 { return 1000000 })
	want := Orange + MoodBlock + Reset
	if got != want {
		t.Errorf("MoodFor 1 waiting: want %q, got %q", want, got)
	}
}

func TestMoodFor_OneFinished(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "finished"},
			{Name: "b", Status: "alive"},
		},
	}
	got := MoodFor(snap, func() int64 { return 1000000 })
	want := MoodGreen + MoodBlock + Reset
	if got != want {
		t.Errorf("MoodFor 1 finished: want %q, got %q", want, got)
	}
}

func TestMoodFor_AllAlive(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "alive"},
			{Name: "b", Status: "alive"},
		},
	}
	got := MoodFor(snap, func() int64 { return 1000000 })
	want := MoodIdle + MoodBlock + Reset
	if got != want {
		t.Errorf("MoodFor all alive: want %q, got %q", want, got)
	}
}

func TestMoodFor_UrgentWaitAge(t *testing.T) {
	now := int64(1000000)
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "waiting", WaitStartedTS: now - 300},
		},
	}
	got := MoodFor(snap, func() int64 { return now })
	want := MoodRed + MoodBlock + Reset
	if got != want {
		t.Errorf("MoodFor urgent wait age: want %q, got %q", want, got)
	}
}

func TestMoodFor_ShellRunning_HappyMood(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "a", Status: "shell-running"},
		},
	}
	got := MoodFor(snap, func() int64 { return 1000000 })
	want := MoodGreen + MoodBlock + Reset
	if got != want {
		t.Errorf("MoodFor shell-running: want %q, got %q", want, got)
	}
}

func TestMoodFor_BlockIsExactlyOneCell(t *testing.T) {
	// MoodFor must return a block that is exactly 1 rune wide after stripping SGR.
	snap := &proto.Snapshot{
		Projects: []proto.Project{{Name: "a", Status: "alive"}},
	}
	result := MoodFor(snap, func() int64 { return 1000000 })
	// Strip all \x1b[...m sequences.
	stripped := strings.Builder{}
	inEscape := false
	for _, r := range result {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		stripped.WriteRune(r)
	}
	runeCount := len([]rune(stripped.String()))
	if runeCount != 1 {
		t.Errorf("MoodFor block must be exactly 1 cell (rune), got %d runes: %q (stripped: %q)", runeCount, result, stripped.String())
	}
}

// ---- chipBranchWithCap (260511-n4n task 2) ----

func TestChipBranchCurrent_Cap24_Under(t *testing.T) {
	// 21-rune branch: fits within cap=24, no truncation.
	var buf bytes.Buffer
	branch := "feature-very-long-namex" // 23 runes
	chipBranchWithCap(&buf, branch, 0, 0, 24)
	got := buf.String()
	if !strings.Contains(got, branch) {
		t.Errorf("chipBranchWithCap cap=24 23-rune: expected verbatim branch in %q", got)
	}
	if strings.Contains(got, string([]rune(Ellipsis))) {
		t.Errorf("chipBranchWithCap cap=24 23-rune: must NOT contain ellipsis in %q", got)
	}
}

func TestChipBranchCurrent_Cap24_Boundary(t *testing.T) {
	// 24-rune branch: exactly at cap, no truncation.
	var buf bytes.Buffer
	branch := "feature-very-long-namexy" // 24 runes
	chipBranchWithCap(&buf, branch, 0, 0, 24)
	got := buf.String()
	if !strings.Contains(got, branch) {
		t.Errorf("chipBranchWithCap cap=24 24-rune: expected verbatim in %q", got)
	}
}

func TestChipBranchCurrent_Cap24_Over(t *testing.T) {
	// 25-rune branch: truncates to 23 runes + Ellipsis.
	var buf bytes.Buffer
	branch := "feature-very-long-namexyz" // 25 runes
	chipBranchWithCap(&buf, branch, 0, 0, 24)
	got := buf.String()
	if strings.Contains(got, branch) {
		t.Errorf("chipBranchWithCap cap=24 25-rune: full branch must NOT appear in %q", got)
	}
	if !strings.Contains(got, Ellipsis) {
		t.Errorf("chipBranchWithCap cap=24 25-rune: must contain Ellipsis in %q", got)
	}
}

func TestChipBranchCurrent_Cap24_Unicode(t *testing.T) {
	// Multi-byte unicode: 10 runes, each 2 bytes — fits within cap=24.
	var buf bytes.Buffer
	branch := "ñoño-ñoño" // 10 runes (some are multi-byte)
	chipBranchWithCap(&buf, branch, 0, 0, 24)
	got := buf.String()
	if !strings.Contains(got, branch) {
		t.Errorf("chipBranchWithCap cap=24 unicode: expected verbatim in %q", got)
	}
}

// ---- chipInlineAlerts (260511-n4n task 1) ----

func TestChipInlineAlerts_None(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive"}
	chipInlineAlerts(&buf, p)
	if buf.Len() != 0 {
		t.Errorf("chipInlineAlerts none: expected empty output, got %q", buf.Bytes())
	}
}

func TestChipInlineAlerts_FailOnly(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", PROpen: 2, PRFail: 1}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "✗1") {
		t.Errorf("chipInlineAlerts fail: expected '✗1' in %q", got)
	}
	if strings.Contains(got, "⊙") {
		t.Errorf("chipInlineAlerts fail: must NOT contain pend glyph ⊙ in %q", got)
	}
}

func TestChipInlineAlerts_PendOnly(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", PROpen: 2, PRPend: 1}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "⊙1") {
		t.Errorf("chipInlineAlerts pend: expected '⊙1' in %q", got)
	}
}

func TestChipInlineAlerts_DirtyOnly(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", DirtyCount: 3}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "+3") {
		t.Errorf("chipInlineAlerts dirty: expected '+3' in %q", got)
	}
}

func TestChipInlineAlerts_FailAndDirty(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", PROpen: 1, PRFail: 1, DirtyCount: 2}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "✗1") {
		t.Errorf("chipInlineAlerts fail+dirty: expected '✗1' in %q", got)
	}
	if !strings.Contains(got, "+2") {
		t.Errorf("chipInlineAlerts fail+dirty: expected '+2' in %q", got)
	}
}

func TestChipInlineAlerts_CIFailCounts(t *testing.T) {
	// Updated 260511-r7x: CI fail and PR fail are now SEPARATE tokens.
	// CI alone with no PR fail should render "✗ CI", not "✗1".
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", CIStatus: "completed", CIConclusion: "failure"}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "✗ CI") {
		t.Errorf("chipInlineAlerts CI fail (no PR): expected '✗ CI', got %q", got)
	}
	if strings.Contains(got, "✗1") {
		t.Errorf("chipInlineAlerts CI fail (no PR): must NOT contain conflated '✗1', got %q", got)
	}
}

// ---- chipInlineAlerts CI tokens (260511-r7x change C) ----

func TestChipInlineAlerts_CIFail_NoPR(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", CIStatus: "completed", CIConclusion: "failure"}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "✗ CI") {
		t.Errorf("CI fail no PR: expected '✗ CI' token, got %q", got)
	}
	// The old conflated rendering produced "✗1" — assert it doesn't appear.
	if strings.Contains(got, "✗1") {
		t.Errorf("CI fail no PR: must NOT contain conflated '✗1' (legacy token), got %q", got)
	}
}

func TestChipInlineAlerts_CIPending_Queued(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", CIStatus: "queued"}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	want := " " + Cyan + "⚙ CI" + Reset
	if got != want {
		t.Errorf("CI queued: want %q, got %q", want, got)
	}
}

func TestChipInlineAlerts_CIPending_InProgress(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", CIStatus: "in_progress"}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	want := " " + Cyan + "⚙ CI" + Reset
	if got != want {
		t.Errorf("CI in_progress: want %q, got %q", want, got)
	}
}

func TestChipInlineAlerts_CISuccess_Suppressed(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", CIStatus: "completed", CIConclusion: "success"}
	chipInlineAlerts(&buf, p)
	if buf.Len() != 0 {
		t.Errorf("CI success: expected suppression, got %q", buf.Bytes())
	}
}

func TestChipInlineAlerts_CIAmbiguous_Suppressed(t *testing.T) {
	ambiguous := []string{"", "cancelled", "skipped", "neutral"}
	for _, c := range ambiguous {
		t.Run("conclusion_"+c, func(t *testing.T) {
			var buf bytes.Buffer
			p := &proto.Project{Name: "alpha", Status: "alive", CIStatus: "completed", CIConclusion: c}
			chipInlineAlerts(&buf, p)
			if buf.Len() != 0 {
				t.Errorf("CI ambiguous %q: expected suppression, got %q", c, buf.Bytes())
			}
		})
	}
}

func TestChipInlineAlerts_PRFailAndCIFail(t *testing.T) {
	var buf bytes.Buffer
	p := &proto.Project{Name: "alpha", Status: "alive", PROpen: 1, PRFail: 1, CIStatus: "completed", CIConclusion: "failure"}
	chipInlineAlerts(&buf, p)
	got := buf.String()
	if !strings.Contains(got, "✗1") {
		t.Errorf("PR+CI fail: expected '✗1' (PR count) in %q", got)
	}
	if !strings.Contains(got, "✗ CI") {
		t.Errorf("PR+CI fail: expected '✗ CI' (CI token) in %q", got)
	}
	// Order: PR fail comes first (higher priority for count).
	idxPR := strings.Index(got, "✗1")
	idxCI := strings.Index(got, "✗ CI")
	if idxPR > idxCI {
		t.Errorf("PR+CI fail: '✗1' must precede '✗ CI', got order PR=%d CI=%d in %q", idxPR, idxCI, got)
	}
}

// ---- chipCI (260509-gfz) ----

func TestChipCI(t *testing.T) {
	cases := []struct {
		name, status, conclusion, want string
	}{
		{"empty", "", "", ""},
		{"queued", "queued", "", "\x1b[36m⚙ CI\x1b[0m"},
		{"in_progress", "in_progress", "", "\x1b[36m⚙ CI\x1b[0m"},
		{"success", "completed", "success", "\x1b[32m✓ CI\x1b[0m"},
		{"failure", "completed", "failure", "\x1b[31m✗ CI\x1b[0m"},
		{"timed_out", "completed", "timed_out", "\x1b[31m✗ CI\x1b[0m"},
		{"action_required", "completed", "action_required", "\x1b[31m✗ CI\x1b[0m"},
		{"cancelled", "completed", "cancelled", ""},
		{"skipped", "completed", "skipped", ""},
		{"neutral", "completed", "neutral", ""},
		{"completed_no_conclusion", "completed", "", ""},
		{"unknown_status", "weird", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			chipCI(&buf, c.status, c.conclusion)
			if got := buf.String(); got != c.want {
				t.Errorf("chipCI(%q,%q) = %q; want %q", c.status, c.conclusion, got, c.want)
			}
		})
	}
}

// ---- renderFailingChecksRow + failingChecksScrollWindow (260512-cgw) ----

func TestFailingChecksScrollWindow_Empty(t *testing.T) {
	if got := failingChecksScrollWindow(nil, 40, 0); got != "" {
		t.Errorf("nil names → %q; want empty", got)
	}
	if got := failingChecksScrollWindow([]string{"lint"}, 0, 0); got != "" {
		t.Errorf("zero width → %q; want empty", got)
	}
}

func TestFailingChecksScrollWindow_FitsStatic(t *testing.T) {
	// "lint, test" is 10 runes; width=40 is comfortably larger so the function
	// must return the joined list verbatim regardless of `nowMs`.
	for _, nowMs := range []int64{0, 5, 100, 1_000_000} {
		got := failingChecksScrollWindow([]string{"lint", "test"}, 40, nowMs)
		if got != "lint, test" {
			t.Errorf("nowMs=%d → %q; want %q (no scroll when joined fits)", nowMs, got, "lint, test")
		}
	}
}

func TestFailingChecksScrollWindow_AdvancesByOneRunePerScrollStep(t *testing.T) {
	// joined = "lint, test, build, integration, e2e"  (35 runes)
	// cycle  = joined + "   "                          (38 runes)
	// width=20 → must scroll. Each rune step lands every
	// failingChecksScrollMs (200ms) of wall-clock.
	names := []string{"lint", "test", "build", "integration", "e2e"}
	joined := "lint, test, build, integration, e2e"
	cycle := joined + "   "
	cycleLen := int64(len([]rune(cycle)))
	width := 20
	step := failingChecksScrollMs

	at0 := failingChecksScrollWindow(names, width, 0)
	atSub := failingChecksScrollWindow(names, width, step-1) // still within step 0
	at1 := failingChecksScrollWindow(names, width, step)
	at5 := failingChecksScrollWindow(names, width, 5*step)
	atWrap := failingChecksScrollWindow(names, width, cycleLen*step)

	if at0 != atSub {
		t.Errorf("sub-step movement: nowMs=0 (%q) and nowMs=step-1 (%q) should be identical", at0, atSub)
	}
	if at0 == at1 {
		t.Errorf("nowMs=0 and nowMs=step (%d) produced same window %q; expected scroll by 1 rune", step, at0)
	}
	doubled := []rune(cycle + cycle)
	want1 := string(doubled[1 : 1+width])
	if at1 != want1 {
		t.Errorf("nowMs=step window = %q; want %q (shift-by-1 of cycle)", at1, want1)
	}
	want5 := string(doubled[5 : 5+width])
	if at5 != want5 {
		t.Errorf("nowMs=5*step window = %q; want %q", at5, want5)
	}
	// Wrap: nowMs == cycleLen*step should produce same output as nowMs=0.
	if atWrap != at0 {
		t.Errorf("wrap mismatch: nowMs=cycleLen*step(%d) = %q; want nowMs=0 = %q", cycleLen*step, atWrap, at0)
	}
}

func TestFailingChecksScrollWindow_NegativeNowSafe(t *testing.T) {
	// Pin a deterministic offset even when wall-clock arithmetic produces a
	// negative input (clock skew, test fixture). Must never panic or return a
	// short window.
	names := []string{"lint", "test", "build", "integration", "e2e"}
	width := 20
	got := failingChecksScrollWindow(names, width, -failingChecksScrollMs*3)
	if len([]rune(got)) != width {
		t.Errorf("negative now: window len = %d runes; want %d", len([]rune(got)), width)
	}
}

func TestRenderFailingChecksRow(t *testing.T) {
	var buf bytes.Buffer
	ok := renderFailingChecksRow(&buf, []string{"lint", "test"}, 40, 0)
	if !ok {
		t.Fatal("renderFailingChecksRow with non-empty names returned false")
	}
	got := buf.String()
	want := "\x1b[31mlint, test\x1b[0m"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestRenderFailingChecksRow_EmptySkips(t *testing.T) {
	var buf bytes.Buffer
	if renderFailingChecksRow(&buf, nil, 40, 0) {
		t.Error("renderFailingChecksRow with nil names returned true; want false")
	}
	if buf.Len() != 0 {
		t.Errorf("buf populated despite empty names: %q", buf.String())
	}
}

// ---- formatAge (internal) ----

func TestFormatAge(t *testing.T) {
	tests := []struct {
		secs int64
		want string
	}{
		{0, "0s"},
		{1, "1s"},
		{59, "59s"},
		{60, "1m"},
		{300, "5m"},
		{3599, "59m"},
		{3600, "1h"},
		{7200, "2h"},
		{86399, "23h"},
		{86400, "1d"},
		{259200, "3d"},
	}
	for _, tc := range tests {
		got := formatAge(tc.secs)
		if got != tc.want {
			t.Errorf("formatAge(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}
