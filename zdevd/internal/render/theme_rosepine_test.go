package render

import (
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

func withTheme(t *testing.T, mode string) {
	t.Helper()
	prev := ThemeMode
	ThemeMode = mode
	t.Cleanup(func() { ThemeMode = prev })
}

// classic must be a PASSTHROUGH, not a reimplementation: every th* function
// returns the exact pre-theme bytes. The goldens prove this end-to-end for
// whole frames; this pins it per-function so a divergence names its culprit.
func TestThemeClassicIsPassthrough(t *testing.T) {
	withTheme(t, "classic")
	if thPalette("marketplace") != PaletteFor("marketplace") {
		t.Error("thPalette must pass through PaletteFor")
	}
	if thDim() != Dim || thWorking() != Icy || thDone() != Yellow ||
		thDead() != RedPulse || thUrgentBar() != RedBorder || thAnchor() != Icy {
		t.Error("classic tokens must equal the raw constants")
	}
	for _, age := range []int64{0, int64(WaitWarnSec), int64(WaitUrgentSec) + 1} {
		if thWaiting(age) != RedPulse {
			t.Errorf("classic waiting is RedPulse at every age, got %q at %ds", thWaiting(age), age)
		}
	}
	if thBreath("zdev", 2) != BreathColorForProject("zdev", 2) {
		t.Error("classic breath must pass through BreathColorForProject")
	}
	if got, want := thDivider(MoodGreen, 3), MoodGreen+"───"; got != want {
		t.Errorf("classic divider: got %q, want %q", got, want)
	}
	for _, c := range []string{Green, Cyan, Yellow, Orange, Dim, RedPulse, Icy} {
		if thChipAccent(c) != c {
			t.Errorf("classic chip accent must pass through, got %q for %q", thChipAccent(c), c)
		}
	}
}

// The wait ramp escalates with the SAME thresholds the notify tiers use —
// gold fresh, rose at warn, bold love at urgent.
func TestThemeWaitRamp(t *testing.T) {
	withTheme(t, "rose-pine")
	fresh := thWaiting(5)
	warn := thWaiting(int64(WaitWarnSec))
	urgent := thWaiting(int64(WaitUrgentSec))
	if fresh == warn || warn == urgent || fresh == urgent {
		t.Errorf("ramp tiers must differ: fresh=%q warn=%q urgent=%q", fresh, warn, urgent)
	}
	if !strings.HasPrefix(urgent, "\x1b[1;") {
		t.Errorf("urgent tier must be bold (single combined SGR), got %q", urgent)
	}
	for _, tok := range []string{fresh, warn, urgent} {
		if !strings.Contains(tok, "38;2;") {
			t.Errorf("rose-pine tokens are truecolor, got %q", tok)
		}
	}
}

// Identity: deterministic per name, on-palette, and the breath bar uses the
// same hue as the marker — identity is ONE color per project everywhere.
func TestThemeIdentityCoherence(t *testing.T) {
	withTheme(t, "rose-pine")
	if thPalette("zdev") != thPalette("zdev") {
		t.Error("identity must be deterministic")
	}
	hue := rpIdentityFor("zdev")
	if got := thBreath("zdev", 1); got != hue.fg() { // frame 1 = normal brightness
		t.Errorf("breath (normal phase) must be the identity hue: got %q want %q", got, hue.fg())
	}
}

// The right-aligned status column: a waiting member's age lands flush with
// the pane edge, and rows never exceed the width budget.
func TestThemeRightAlignedStatus(t *testing.T) {
	defer func(m string) { GroupMode = m }(GroupMode)
	GroupMode = "prefix"
	withTheme(t, "rose-pine")

	snap := flatSnapshot()
	snap.Projects[1].Attention = proto.AttWaiting
	snap.Projects[1].Status = "waiting"
	snap.Projects[1].WaitStartedTS = fixedNowFn() - 120

	const width = 50
	out := stripAnsi(Render(snap, width, NewAnimator(), fixedNowFn))
	var row string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "pay-app") && strings.Contains(l, "2m") {
			row = l
			break
		}
	}
	if row == "" {
		t.Fatalf("no waiting row with an age rendered:\n%s", out)
	}
	// The member row renders under a 3-column gutter, so its budget is
	// width-3; the age must END the row at exactly that column.
	if got := len([]rune(row)); got != width {
		t.Errorf("row must fill the width budget exactly (%d), got %d: %q", width, got, row)
	}
	if !strings.HasSuffix(row, "2m") {
		t.Errorf("age must be flush right: %q", row)
	}

	// Classic mode: same snapshot, cluster stays inline (short row).
	withTheme(t, "classic")
	outC := stripAnsi(Render(snap, width, NewAnimator(), fixedNowFn))
	for _, l := range strings.Split(outC, "\n") {
		if strings.Contains(l, "pay-app") && strings.Contains(l, "2m") {
			if len([]rune(strings.TrimRight(l, " "))) == width {
				t.Errorf("classic must not right-align: %q", l)
			}
		}
	}
}

// The divider gradient keeps the mood hue at the LEFT end and fades toward
// base — and stays exactly n glyphs wide.
func TestThemeDividerGradient(t *testing.T) {
	withTheme(t, "rose-pine")
	d := thDivider(MoodGreen, 17)
	if got := strings.Count(d, "─"); got != 17 {
		t.Errorf("divider must stay 17 glyphs, got %d", got)
	}
	if !strings.HasPrefix(d, rpFoam.fg()) {
		t.Errorf("gradient must open at the full mood hue")
	}
	segs := strings.Split(d, "─")
	if segs[0] == segs[15] {
		t.Errorf("gradient must actually fade: first and last hues identical")
	}
}
