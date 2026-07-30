package render

import (
	"fmt"
	"regexp"
	"testing"
)

func TestPulseFrames_Verbatim(t *testing.T) {
	want := [8]string{"·", "∙", "•", "●", "●", "•", "∙", "·"}
	for i, w := range want {
		if PulseFrames[i] != w {
			t.Errorf("PulseFrames[%d] = %q; want %q", i, PulseFrames[i], w)
		}
	}
}

func TestBreathBrightness_Values(t *testing.T) {
	want := [4]string{"1", "", "2", ""}
	for i, w := range want {
		if BreathBrightness[i] != w {
			t.Errorf("BreathBrightness[%d] = %q; want %q", i, BreathBrightness[i], w)
		}
	}
}

func TestBreathColorForProject_Deterministic(t *testing.T) {
	// Same name + same frame must always return the same string.
	for frame := 0; frame < 4; frame++ {
		a := BreathColorForProject("alpha", frame)
		b := BreathColorForProject("alpha", frame)
		if a != b {
			t.Errorf("frame %d: not deterministic: %q vs %q", frame, a, b)
		}
	}

	// Different names with different PaletteIndex must produce different strings.
	// Find two names with different palette indices.
	name1 := "alpha"
	idx1 := PaletteIndex(name1)
	name2 := "beta"
	idx2 := PaletteIndex(name2)
	if idx1 != idx2 {
		c1 := BreathColorForProject(name1, 0)
		c2 := BreathColorForProject(name2, 0)
		if c1 == c2 {
			t.Errorf("different names (%q idx=%d, %q idx=%d) produced same BreathColorForProject: %q",
				name1, idx1, name2, idx2, c1)
		}
	}

	// Frame values outside [0,3] are folded by mod: frame=4 equals frame=0.
	if got, want := BreathColorForProject("alpha", 4), BreathColorForProject("alpha", 0); got != want {
		t.Errorf("frame 4 should equal frame 0: got %q, want %q", got, want)
	}
	if got, want := BreathColorForProject("alpha", 7), BreathColorForProject("alpha", 3); got != want {
		t.Errorf("frame 7 should equal frame 3: got %q, want %q", got, want)
	}
}

func TestBreathColorForProject_Format(t *testing.T) {
	name := "alpha"
	code := paletteXtermCodes[PaletteIndex(name)]

	cases := []struct {
		frame     int
		wantFmt   string // format template with bright and code
		hasBright bool
	}{
		{0, "\x1b[1;38;5;%dm", true}, // bold peak
		{1, "\x1b[38;5;%dm", false},  // default brightness
		{2, "\x1b[2;38;5;%dm", true}, // dim trough
		{3, "\x1b[38;5;%dm", false},  // default brightness return
	}
	for _, tc := range cases {
		var want string
		if tc.hasBright {
			bright := BreathBrightness[tc.frame]
			want = fmt.Sprintf("\x1b[%s;38;5;%dm", bright, code)
		} else {
			want = fmt.Sprintf("\x1b[38;5;%dm", code)
		}
		got := BreathColorForProject(name, tc.frame)
		if got != want {
			t.Errorf("BreathColorForProject(%q, %d) = %q; want %q", name, tc.frame, got, want)
		}
	}
}

func TestProjectPalette_VerbatimXterm(t *testing.T) {
	codes := []int{39, 45, 51, 75, 81, 87, 105, 111, 141, 147, 177, 183, 207, 213, 219}
	if len(ProjectPalette) != 15 {
		t.Fatalf("ProjectPalette len = %d; want 15", len(ProjectPalette))
	}
	for i, code := range codes {
		want := "\x1b[38;5;" + itoa(code) + "m"
		if ProjectPalette[i] != want {
			t.Errorf("ProjectPalette[%d] = %q; want %q", i, ProjectPalette[i], want)
		}
	}
}

// itoa avoids strconv import — simple decimal stringification for the test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestThresholds(t *testing.T) {
	cases := []struct {
		name      string
		got, want int
	}{
		{"StaleThresholdSec", StaleThresholdSec, 3600},
		{"WaitWarnSec", WaitWarnSec, 60},
		{"WaitUrgentSec", WaitUrgentSec, 300},
		{"PRCelebrationFrames", PRCelebrationFrames, 60},
		{"PulseHold", PulseHold, 1},
		{"BreathHold", BreathHold, 30},
		{"FrameSleepMS", FrameSleepMS, 66},
		{"IdleSleepMS", IdleSleepMS, 200},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d; want %d", c.name, c.got, c.want)
		}
	}
}

func TestEmDashAndEllipsis_Bytes(t *testing.T) {
	if len(EmDash) != 3 ||
		EmDash[0] != 0xE2 || EmDash[1] != 0x80 || EmDash[2] != 0x94 {
		t.Errorf("EmDash bytes = % x; want e2 80 94", []byte(EmDash))
	}
	if len(Ellipsis) != 3 ||
		Ellipsis[0] != 0xE2 || Ellipsis[1] != 0x80 || Ellipsis[2] != 0xA6 {
		t.Errorf("Ellipsis bytes = % x; want e2 80 a6", []byte(Ellipsis))
	}
}

func TestDefaultBranchesRE(t *testing.T) {
	re := regexp.MustCompile(DefaultBranchesRE)
	matches := []string{"main", "master", "develop", "trunk"}
	for _, b := range matches {
		if !re.MatchString(b) {
			t.Errorf("DefaultBranchesRE should match %q", b)
		}
		if !IsDefaultBranch(b) {
			t.Errorf("IsDefaultBranch(%q) = false; want true", b)
		}
	}
	nonMatches := []string{"feature-x", "release/1.2", "develop-staging", "main-feature", "trunky"}
	for _, b := range nonMatches {
		if re.MatchString(b) {
			t.Errorf("DefaultBranchesRE should NOT match %q", b)
		}
	}
}
