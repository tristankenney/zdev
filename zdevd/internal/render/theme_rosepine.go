// The Rose Pine theme (ZDEV_SIDEBAR_THEME=rose-pine).
//
// One knob, one look: Rose Pine Moon semantic colors, the right-aligned
// status column, the wait-age hue ramp, and the material polish (divider
// gradient, dim rollups) all ride ThemeMode together — they were designed
// as one visual system and dogfood/revert as one. classic (the default)
// returns every byte the goldens pin; nothing below executes.
//
// Truecolor: the operator's tmux negotiates RGB (terminal-overrides
// ",*:RGB" in config/zdev.tmux.conf; verified via #{client_termfeatures}),
// so tokens are exact Rose Pine hexes as 38;2;r;g;b SGR. These are plain
// strings, not lipgloss styles, on purpose: every call site in frame.go /
// chips.go concatenates color strings into a byte buffer, and a raw SGR
// token drops into that shape without restructuring. The pinned lipgloss
// renderer (lipgloss.go) remains the entry point for future COMPONENT
// work (borders, panels, joins) — color tokens just aren't where it earns
// anything.
package render

import "fmt"

// ThemeMode selects the sidebar's visual system. cmd/zdev-sidebar sets it
// from ZDEV_SIDEBAR_THEME at startup (before the first frame; never
// mutated after). "classic" — the byte-exact current look, the default.
// "rose-pine" — the Rose Pine Moon system described in the package doc.
var ThemeMode = "classic"

// rpRGB is one truecolor token. Fields kept (not just the escape string)
// because the breath bar and the divider gradient blend tokens at runtime.
type rpRGB struct{ r, g, b uint8 }

func (c rpRGB) fg() string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b) }

// fgBright returns the single COMBINED SGR "\x1b[<b>;38;2;r;g;bm" the
// breath bar requires (same one-sequence rule BreathColorForProject
// documents — two ESC sequences would break the FrameWriter byte-compare
// granularity assumptions in subtle ways and read as two attrs to tmux).
func (c rpRGB) fgBright(bright string) string {
	if bright == "" {
		return c.fg()
	}
	return fmt.Sprintf("\x1b[%s;38;2;%d;%d;%dm", bright, c.r, c.g, c.b)
}

// lerp blends toward `to` by t in [0,1] — the divider gradient and the
// extended identity palette both build on it.
func (c rpRGB) lerp(to rpRGB, t float64) rpRGB {
	f := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }
	return rpRGB{f(c.r, to.r), f(c.g, to.g), f(c.b, to.b)}
}

// Rose Pine Moon — https://rosepinetheme.com/palette (moon variant), the
// same variant the M-p switcher's fzf colors and docs/guide.html's dark
// mode already use, which is the point: one designed object.
var (
	rpBase  = rpRGB{0x23, 0x21, 0x36}
	rpMuted = rpRGB{0x6e, 0x6a, 0x86}
	rpText  = rpRGB{0xe0, 0xde, 0xf4}
	rpLove  = rpRGB{0xeb, 0x6f, 0x92}
	rpGold  = rpRGB{0xf6, 0xc1, 0x77}
	rpRose  = rpRGB{0xea, 0x9a, 0x97}
	rpPine  = rpRGB{0x3e, 0x8f, 0xb0}
	rpFoam  = rpRGB{0x9c, 0xcf, 0xd8}
	rpIris  = rpRGB{0xc4, 0xa7, 0xe7}
)

// rpIdentity is the per-project identity palette: the six Rose Pine
// accents plus a text-blended variant of each — 12 deterministic slots
// (vs classic's 15 xterm slots; fewer, but every one is ON-palette, which
// is what makes the theme read as designed rather than assigned).
var rpIdentity = [12]rpRGB{
	rpIris, rpFoam, rpRose, rpPine, rpGold, rpLove,
	rpIris.lerp(rpText, 0.45), rpFoam.lerp(rpText, 0.45), rpRose.lerp(rpText, 0.45),
	rpPine.lerp(rpText, 0.45), rpGold.lerp(rpText, 0.45), rpLove.lerp(rpText, 0.45),
}

func rpIdentityFor(name string) rpRGB {
	return rpIdentity[int(posixCksum([]byte(name)))%len(rpIdentity)]
}

// ---- the theme seam ----
//
// Every color decision frame.go/chips.go used to read from a constant now
// flows through one th* function. classic returns the EXACT prior bytes
// (goldens are the proof); rose-pine returns the token. New call sites
// must use these, never the raw constants — that is what keeps the knob a
// knob instead of a fork.

// thPalette is the identity hue for a project name (markers, gutters,
// home-row names).
func thPalette(name string) string {
	if ThemeMode == "rose-pine" {
		return rpIdentityFor(name).fg()
	}
	return PaletteFor(name)
}

// thDim is muted/receded material: stale rows, absent dots, drawer
// gutters, rollup counts.
func thDim() string {
	if ThemeMode == "rose-pine" {
		return rpMuted.fg()
	}
	return Dim
}

// thWaiting is the wait-age hue ramp: gold while fresh, rose through the
// warn tier, love at the urgent/STUCK tier — thresholds shared with the
// notify escalation (WaitWarnSec / WaitUrgentSec) so what the eye sees
// and what the voice says escalate together. Classic keeps the single
// RedPulse for all ages (the pulse cadence carries age there).
func thWaiting(ageSec int64) string {
	if ThemeMode != "rose-pine" {
		return RedPulse
	}
	switch {
	case ageSec >= int64(WaitUrgentSec):
		return rpLove.fgBright("1")
	case ageSec >= int64(WaitWarnSec):
		return rpRose.fg()
	default:
		return rpGold.fg()
	}
}

func thWorking() string {
	if ThemeMode == "rose-pine" {
		return rpFoam.fg()
	}
	return Icy
}

func thDone() string {
	if ThemeMode == "rose-pine" {
		return rpGold.fg()
	}
	return Yellow
}

func thDead() string {
	if ThemeMode == "rose-pine" {
		return rpLove.fgBright("1")
	}
	return RedPulse
}

// thHover is the hover-highlight token for a row's NAME under the mouse
// pointer (ZDEV_SIDEBAR_HOVER, tea engine only). FOREGROUND-ONLY by the
// same hard-won precedent as thUrgentBar's 260511-nxy comment — a bg fill
// bleeds across rows and corrupts the frame, so this spends text weight,
// never a background. It must read as distinct from both ▌ (current
// session) and ▶ (keyboard cursor), which live in the margin column this
// function never touches — callers apply it to the NAME only. Classic has
// no extra hue to spend without inventing an unpaletted 16th xterm slot,
// so Bold alone carries "this is different"; rose-pine spends its
// brightest neutral text token instead, which reads as "lit" without
// stealing an identity hue from PaletteFor/rpIdentityFor.
func thHover() string {
	if ThemeMode == "rose-pine" {
		return rpText.fg()
	}
	return Bold
}

// thAnchor is the focus loop's anchor row marker hue ("▶ now", phase 3C).
// Deliberately its own token rather than a reuse of thWorking — both read
// as the same working-hue FAMILY today (classic: Icy for both), but the
// anchor row is a one-of-a-kind fixture and a future palette change to
// "working in general" must not silently retint it too.
func thAnchor() string {
	if ThemeMode == "rose-pine" {
		return rpFoam.fg()
	}
	return Icy
}

// thUrgentBar is the urgent row's left-border ▌ accent.
func thUrgentBar() string {
	if ThemeMode == "rose-pine" {
		return rpLove.fgBright("1")
	}
	return RedBorder
}

// thBreath is the current-session breath bar: identity hue at the
// animator's brightness phase.
func thBreath(name string, frame int) string {
	if ThemeMode != "rose-pine" {
		return BreathColorForProject(name, frame)
	}
	return rpIdentityFor(name).fgBright(BreathBrightness[frame%len(BreathBrightness)])
}

func thDivider(moodClassic string, n int) string {
	if ThemeMode != "rose-pine" {
		out := moodClassic
		for i := 0; i < n; i++ {
			out += "─"
		}
		return out
	}
	var from rpRGB
	switch moodClassic {
	case MoodRed:
		from = rpLove
	case Orange:
		from = rpGold
	case MoodGreen:
		from = rpFoam
	default:
		from = rpMuted
	}
	out := ""
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1)
		out += from.lerp(rpBase, t*0.85).fg() + "─"
	}
	return out
}

// thChipAccent maps the inline-chip semantic colors (branch/CI/dirty
// alerts) onto the theme. Classic passes through untouched.
func thChipAccent(classic string) string {
	if ThemeMode != "rose-pine" {
		return classic
	}
	switch classic {
	case Green:
		return rpPine.fg()
	case Cyan:
		return rpFoam.fg()
	case Yellow:
		return rpGold.fg()
	case Orange:
		return rpRose.fg()
	case RedPulse, RedBorder:
		return rpLove.fg()
	case Icy:
		return rpFoam.fg()
	case Dim:
		return rpMuted.fg()
	case "\x1b[31m": // chipInlineAlerts' raw red
		return rpLove.fg()
	default:
		return classic
	}
}
