package render

import (
	"io"
	"strconv"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Renderer is the ONE pinned lipgloss entry point for the whole zdevd
// module. lipgloss's own auto-detecting renderer probes its output file
// descriptor and silently degrades to termenv.Ascii — no color output at
// all — the moment that descriptor isn't a real tty. That is every `go
// test` run, every golden-frame capture (internal/render's own golden
// tests among them), and every renderer respawned inside a tmux
// control-mode pane rather than launched at a literal terminal. A spike on
// branch spike/lipgloss-gutters hit this directly: lipgloss reproduced
// this package's exact SGR bytes ONLY once the color profile was pinned
// explicitly to ANSI256; every no-tty context otherwise stripped every
// style to "".
//
// The fix is structural, not a convention to remember: exactly one
// renderer, constructed here, with its profile pinned. scripts/check-
// no-lipgloss-scatter.sh (wired into `make test`) fails the build if
// lipgloss or termenv is imported anywhere else in the module — so there
// is no second call site that could reintroduce auto-detection.
//
// ANSI256, not TrueColor: the hand-written SGR constants in ansi.go /
// theme.go / color.go were authored as xterm-256 codes (Icy = "\x1b[38;5;
// 117m", the ProjectPalette entries, etc). TrueColor would reinterpret /
// upsample those codes and break the byte-parity this file's tests (and
// any future golden-frame migration) depend on.
var Renderer = newPinnedRenderer()

func newPinnedRenderer() *lipgloss.Renderer {
	// io.Discard: this renderer is never written through directly — it
	// only turns Style values into strings via Render(), which callers
	// then write into their own buffers. The writer argument only matters
	// to lipgloss/termenv for terminal auto-detection, which SetColorProfile
	// overrides immediately below regardless of what's passed here.
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.ANSI256)
	return r
}

// paletteColor returns the lipgloss color for one xterm-256 code drawn from
// paletteXtermCodes (color.go). Shared by StyleGutter and StyleHomeName so
// both read the palette table the same way.
func paletteColor(xtermCode int) lipgloss.Color {
	return lipgloss.Color(strconv.Itoa(xtermCode))
}

// StyleDim is the pinned-renderer equivalent of the Dim constant
// (bright-black foreground, xterm color 8) — muted/inactive text: stale
// timestamps, idle chips, dash fills.
func StyleDim() lipgloss.Style {
	return lipgloss.NewStyle().Renderer(Renderer).Foreground(lipgloss.Color("8"))
}

// StyleBold is the pinned-renderer equivalent of the Bold constant —
// emphasis for section headers and titles.
func StyleBold() lipgloss.Style {
	return lipgloss.NewStyle().Renderer(Renderer).Bold(true)
}

// StyleGutter returns the stable per-project identity hue — the same
// xterm-256 slot PaletteIndex(name) selects out of paletteXtermCodes /
// ProjectPalette (color.go, theme.go). Named for its most visible job
// (groupGutter's hued frame glyph in frame.go) but it is the SAME color
// PaletteFor(name) already supplies to the initiative HOME row's name and
// to idle project chips — one identity hue, several call sites.
func StyleGutter(name string) lipgloss.Style {
	code := paletteXtermCodes[PaletteIndex(name)]
	return lipgloss.NewStyle().Renderer(Renderer).Foreground(paletteColor(code))
}

// StyleHomeName is the single-style equivalent of what renderHomeRow
// (frame.go) currently writes as TWO separate escape sequences — Bold then
// PaletteFor(name) — for an initiative's HOME row name. It exists for API
// completeness and is exercised only by the composite-divergence test in
// lipgloss_test.go; nothing imports it in production yet. See that test
// for why: lipgloss merges a Bold+Foreground style into ONE SGR sequence
// ("\x1b[1;38;5;<code>m"), while renderHomeRow's two Write calls
// concatenate two ("\x1b[1m\x1b[38;5;<code>m"). Semantically identical,
// byte-different on the wire — migrating renderHomeRow to this constructor
// will churn its golden fixture.
func StyleHomeName(name string) lipgloss.Style {
	code := paletteXtermCodes[PaletteIndex(name)]
	return lipgloss.NewStyle().Renderer(Renderer).Bold(true).Foreground(paletteColor(code))
}

// StyleIcy is the pinned-renderer equivalent of the Icy constant (xterm
// color 117) — the working/in-progress attention marker.
func StyleIcy() lipgloss.Style {
	return lipgloss.NewStyle().Renderer(Renderer).Foreground(lipgloss.Color("117"))
}

// StyleOrange is the pinned-renderer equivalent of the Orange constant
// (xterm color 208) — the warn tier: wait-age warnings, dirty/PR-pending
// counts.
func StyleOrange() lipgloss.Style {
	return lipgloss.NewStyle().Renderer(Renderer).Foreground(lipgloss.Color("208"))
}

// StyleRedPulse is the pinned-renderer equivalent of the RedPulse constant
// (bold + bright-red foreground) — the urgent/blocked tier. Unlike
// StyleHomeName, this one IS byte-identical to its existing constant:
// RedPulse was already authored as a single combined SGR sequence
// ("\x1b[1;91m"), and termenv's ANSI256 profile keeps low ANSI colors
// (0-15) in their compact 3/4-bit SGR form rather than upsampling them to
// "38;5;N" — so Bold+Color("9") merges to those exact bytes. See
// TestPinnedRenderer_ByteParity.
func StyleRedPulse() lipgloss.Style {
	return lipgloss.NewStyle().Renderer(Renderer).Bold(true).Foreground(lipgloss.Color("9"))
}

// StyleGaugeName returns a fixed-width, left-aligned style for the review
// gauge's (review_gauge.go, rose-pine mode) repo-name column: it right-pads
// a truncated name to `width` columns so the bar that follows starts at the
// SAME column on every row, regardless of how long each repo's name is.
// Deliberately colorless — Width/Align is pure layout, so it composes with
// the ANSI256-pinned Renderer at zero profile risk (unlike Foreground on a
// truecolor hex, which the renderer WOULD downsample; see thBar* in
// theme_rosepine.go for why bar-segment color stays off this path).
func StyleGaugeName(width int) lipgloss.Style {
	return lipgloss.NewStyle().Renderer(Renderer).Width(width)
}
