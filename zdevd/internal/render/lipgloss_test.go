package render

import (
	"strconv"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestPinnedRenderer_ByteParity is the load-bearing assertion behind this
// whole file: with the color profile explicitly pinned to ANSI256 (see
// Renderer in lipgloss.go), lipgloss emits the EXACT bytes the hand-written
// SGR constants in ansi.go/theme.go already do, for every single-attribute
// style this package exports a constructor for. If lipgloss's own
// auto-detecting default renderer were used instead, this test — running
// with no tty, like every `go test` invocation — would see every one of
// these sequences silently stripped to "" (termenv.Ascii). That is the
// exact failure mode the spike on branch spike/lipgloss-gutters hit, and
// the reason the pinned Renderer exists.
func TestPinnedRenderer_ByteParity(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"StyleDim", StyleDim().Render(""), Dim + Reset},
		{"StyleBold", StyleBold().Render(""), Bold + Reset},
		{"StyleIcy", StyleIcy().Render(""), Icy + Reset},
		{"StyleOrange", StyleOrange().Render(""), Orange + Reset},
		{"StyleRedPulse (bold+bright-red)", StyleRedPulse().Render(""), RedPulse + Reset},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: pinned-renderer output = %q; want %q (byte-identical to existing constant)", c.name, c.got, c.want)
		}
	}
}

// TestPaletteColor_ByteParity checks every one of the 15 project-hue
// slots — not just one — since StyleGutter/StyleHomeName can land on any
// of them depending on PaletteIndex(name). Each must round-trip through
// the pinned renderer byte-identically to its ProjectPalette entry.
func TestPaletteColor_ByteParity(t *testing.T) {
	for i, code := range paletteXtermCodes {
		got := lipgloss.NewStyle().Renderer(Renderer).Foreground(paletteColor(code)).Render("")
		want := ProjectPalette[i] + Reset
		if got != want {
			t.Errorf("palette slot %d (xterm %d): pinned-renderer output = %q; want %q", i, code, got, want)
		}
	}
}

// TestStyleGutter_WiresPaletteIndex confirms StyleGutter looks up the same
// slot PaletteIndex(name)/PaletteFor(name) do. TestPaletteColor_ByteParity
// above already proves every slot round-trips byte-identically, so this
// only needs to prove the wiring is correct, not re-prove every slot.
func TestStyleGutter_WiresPaletteIndex(t *testing.T) {
	for _, name := range []string{"alpha", "beta", "zdev", "projects", "initiatives"} {
		want := ProjectPalette[PaletteIndex(name)] + Reset
		if got := StyleGutter(name).Render(""); got != want {
			t.Errorf("StyleGutter(%q) = %q; want %q", name, got, want)
		}
	}
}

// TestStyleHomeName_CompositeDivergesFromConcatenation DOCUMENTS the one
// parity gap called out in this task's plan: lipgloss merges a composite
// (multi-attribute) style into ONE SGR sequence, while the current
// renderHomeRow (frame.go) — which this task does NOT migrate — writes
// Bold and PaletteFor(name) as TWO separate escape sequences via two
// buf.WriteString calls. Semantically identical (bold + the same
// xterm-256 fg code); byte-DIFFERENT on the wire.
//
// This is not a bug to fix here: the two cannot be made byte-equal without
// changing renderHomeRow's output, and this task ships no production
// output changes. When a future task migrates renderHomeRow to
// StyleHomeName, frame_test.go's golden fixtures for every initiative HOME
// row WILL need regenerating (`make golden UPDATE=1`) — the visible bytes
// go from two escapes to one merged escape. This test exists so that day
// isn't a surprise.
func TestStyleHomeName_CompositeDivergesFromConcatenation(t *testing.T) {
	name := "alpha"
	code := paletteXtermCodes[PaletteIndex(name)]

	// What renderHomeRow writes TODAY (frame.go: buf.WriteString(Bold);
	// buf.WriteString(PaletteFor(name))) — two concatenated SGR sequences.
	concatenated := Bold + PaletteFor(name) + Reset

	// What the pinned lipgloss renderer produces for the same two
	// attributes expressed as ONE style.
	merged := StyleHomeName(name).Render("")
	wantMerged := "\x1b[1;38;5;" + strconv.Itoa(code) + "m" + Reset
	if merged != wantMerged {
		t.Errorf("StyleHomeName(%q) = %q; want %q (single merged SGR sequence)", name, merged, wantMerged)
	}

	if merged == concatenated {
		t.Fatalf("expected the documented composite divergence (merged SGR != concatenated SGRs) but got equal output %q — either lipgloss changed how it merges attributes, or paletteXtermCodes/PaletteIndex drifted; re-verify the ANSI256 pin still holds before trusting this parity story", merged)
	}
	t.Logf("documented divergence for %q: production writes %q (2 escapes); StyleHomeName's pinned renderer emits %q (1 merged escape) — same semantics, different bytes", name, concatenated, merged)
}
