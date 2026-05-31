package tmuxctl

import "testing"

func TestClassifyPaneTitleWaiting(t *testing.T) {
	cases := []string{"● claude", "● claude bench-test", "● ", "● anything"}
	for _, c := range cases {
		if got := ClassifyPaneTitle(c); got != StatusWaiting {
			t.Errorf("ClassifyPaneTitle(%q) = %q, want %q", c, got, StatusWaiting)
		}
	}
}

func TestClassifyPaneTitleFinished(t *testing.T) {
	cases := []string{"◆ pi", "◆ done", "◆ "}
	for _, c := range cases {
		if got := ClassifyPaneTitle(c); got != StatusFinished {
			t.Errorf("ClassifyPaneTitle(%q) = %q, want %q", c, got, StatusFinished)
		}
	}
}

func TestClassifyPaneTitleShellRunning(t *testing.T) {
	cases := []string{"◎ npm run dev", "◎ go test", "◎ "}
	for _, c := range cases {
		if got := ClassifyPaneTitle(c); got != StatusShellRunning {
			t.Errorf("ClassifyPaneTitle(%q) = %q, want %q", c, got, StatusShellRunning)
		}
	}
}

func TestClassifyPaneTitleAlive(t *testing.T) {
	cases := []string{"zsh", "vim", "bash command", "no marker here"}
	for _, c := range cases {
		if got := ClassifyPaneTitle(c); got != StatusAlive {
			t.Errorf("ClassifyPaneTitle(%q) = %q, want %q", c, got, StatusAlive)
		}
	}
}

func TestClassifyPaneTitleEmpty(t *testing.T) {
	if got := ClassifyPaneTitle(""); got != StatusAlive {
		t.Errorf("ClassifyPaneTitle(\"\") = %q, want %q", got, StatusAlive)
	}
}

// TestClassifyPaneTitleByteExact verifies that an incomplete UTF-8 byte
// (e.g., the lead byte 0xE2 of ● without the continuation bytes) does NOT
// false-match. Go strings are byte sequences and HasPrefix is byte-exact.
func TestClassifyPaneTitleByteExact(t *testing.T) {
	// 0xE2 alone is the first byte of ●; without the rest of the glyph
	// and the trailing space, it should NOT classify as waiting.
	if got := ClassifyPaneTitle("\xE2"); got != StatusAlive {
		t.Errorf("ClassifyPaneTitle(\\xE2) = %q, want %q (incomplete UTF-8)", got, StatusAlive)
	}
	// Just the glyph without the space — should NOT match (we require
	// the trailing space to disambiguate from titles that start with the
	// glyph but represent something else).
	if got := ClassifyPaneTitle("●claude"); got != StatusAlive {
		t.Errorf("ClassifyPaneTitle(\"●claude\") = %q, want %q (no trailing space)", got, StatusAlive)
	}
}

// --- New Claude Code v2.1+ title format tests ---

// TestClassifyPaneTitleNewClaudeWaiting verifies that ✳ (U+2733) titles with
// a task description after the marker are classified as StatusWaiting — these
// are real permission-waits (Claude has produced output and is awaiting the
// user's response).
func TestClassifyPaneTitleNewClaudeWaiting(t *testing.T) {
	cases := []string{
		"✳ Update API documentation",
		"✳ Two confirmed bugs in the daemon",
		"✳ ",
	}
	for _, c := range cases {
		if got := ClassifyPaneTitle(c); got != StatusWaiting {
			t.Errorf("ClassifyPaneTitle(%q) = %q, want %q (real ✳ wait)", c, got, StatusWaiting)
		}
	}
}

// TestClassifyPaneTitleNewClaudeIdle verifies that the literal "✳ Claude Code"
// title (Claude Code v2.1+ idle prompt) is classified as StatusAlive rather
// than StatusWaiting. Without this distinction, every idle Claude session
// pulses "needs input" on daemon restart simply because every Claude pane
// sits at this title before any task runs. Alive renders as a subtle ·
// marker rather than the louder shell-running ◎.
func TestClassifyPaneTitleNewClaudeIdle(t *testing.T) {
	if got := ClassifyPaneTitle("✳ Claude Code"); got != StatusAlive {
		t.Errorf("ClassifyPaneTitle(\"✳ Claude Code\") = %q, want %q (idle prompt)", got, StatusAlive)
	}
}

// TestClassifyPaneTitleBrailleSpinner verifies that Braille pattern characters
// (U+2800–U+28FF) followed by a space are classified as StatusShellRunning,
// matching the "Claude is working/generating" state in Claude Code v2.1+.
func TestClassifyPaneTitleBrailleSpinner(t *testing.T) {
	cases := []string{
		"⠂ Debug zdev daemon connection issue", // U+2802
		"⠐ Check backfill progress status",    // U+2810
		"⠋ Building...",                        // U+280B
		"⠙ Analyzing...",                       // U+2819
		"⠹ Running tests",                      // U+2839
		"⠸ Compiling",                          // U+2838
		"⠼ Fetching",                           // U+283C
		"⠴ Processing",                         // U+2834
		"⠦ Generating",                         // U+2826
		"⠧ Completing",                         // U+2827
		"⠇ Finishing",                          // U+2807
		"⠏ Loading",                            // U+280F
		"⠁ Task",                               // U+2801
	}
	for _, c := range cases {
		if got := ClassifyPaneTitle(c); got != StatusShellRunning {
			t.Errorf("ClassifyPaneTitle(%q) = %q, want %q (Braille spinner = working)", c, got, StatusShellRunning)
		}
	}
}

// TestClassifyPaneTitleBrailleNoSpace verifies that a Braille character NOT
// followed by a space does NOT classify as StatusShellRunning. The space is
// part of the spinner format.
func TestClassifyPaneTitleBrailleNoSpace(t *testing.T) {
	// Braille char with no trailing space — should be StatusAlive (default).
	if got := ClassifyPaneTitle("⠂nospace"); got != StatusAlive {
		t.Errorf("ClassifyPaneTitle(\"⠂nospace\") = %q, want %q (no space after Braille char)", got, StatusAlive)
	}
	// Bare Braille char only.
	if got := ClassifyPaneTitle("⠂"); got != StatusAlive {
		t.Errorf("ClassifyPaneTitle(\"⠂\") = %q, want %q (bare Braille char)", got, StatusAlive)
	}
}

