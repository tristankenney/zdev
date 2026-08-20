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
	rpBase   = rpRGB{0x23, 0x21, 0x36}
	rpMuted  = rpRGB{0x6e, 0x6a, 0x86}
	rpSubtle = rpRGB{0x90, 0x8c, 0xaa}
	rpText   = rpRGB{0xe0, 0xde, 0xf4}
	rpLove   = rpRGB{0xeb, 0x6f, 0x92}
	rpGold   = rpRGB{0xf6, 0xc1, 0x77}
	rpRose   = rpRGB{0xea, 0x9a, 0x97}
	rpPine   = rpRGB{0x3e, 0x8f, 0xb0}
	rpFoam   = rpRGB{0x9c, 0xcf, 0xd8}
	rpIris   = rpRGB{0xc4, 0xa7, 0xe7}
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

// thSubtle is the quiet-structure tone — stream labels: one step brighter
// than thDim so a place name reads above the dim rows it labels, well
// below text. Classic has no third grey, so it falls back to Dim.
func thSubtle() string {
	if ThemeMode == "rose-pine" {
		return rpSubtle.fg()
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

// thWorkingBreath is thWorking()'s hue with a shared brightness breath —
// the SAME BreathBrightness cycle the current-session ▌ uses, but no hue
// shift (that's reserved for the current session specifically; every
// working row uses this plain intensity cycle). One shared frame means
// every working row in the fleet breathes in the SAME phase together —
// motion without reintroducing per-project identity color, the thing
// live feedback rejected the same day (2026-08-20). Classic is untouched:
// flat Icy regardless of frame, same as thWorking().
//
// Deliberately its own function rather than a thWorking(frame) signature
// change — memberGlyph's Agent Teams working glyph has no animator frame
// threaded to it (out of scope for this whole delight arc) and keeps
// calling the plain thWorking().
func thWorkingBreath(frame int) string {
	if ThemeMode != "rose-pine" {
		return Icy
	}
	return rpFoam.fgBright(BreathBrightness[frame%len(BreathBrightness)])
}

// thDone is Iris (purple) in rose-pine — deliberately NOT rpGold, which
// thWaiting's fresh tier already owns. Sharing a hue there meant "just
// finished, nothing left to do" and "just started waiting, will need you
// soon" were color-identical (only the glyph, ◆ vs ●, told them apart) —
// two states with opposite implications for whether you need to look.
// Semantic colors were the explicit ask (operator feedback, 2026-08-20)
// that also reverted the working-state identity-hue experiment; this is
// the other half of that same audit. Classic is untouched (Yellow, and
// waiting there is a flat RedPulse regardless of age, so it never
// collided in the first place).
func thDone() string {
	if ThemeMode == "rose-pine" {
		return rpIris.fg()
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
//
// Warmer, not brighter (delight pass, 2026-08-20) — this ▌ is the single
// most-looked-at pixel in the whole interface, you sit on it all day, and
// a bare brightness step read as mechanical rather than considered. At the
// breath's peak (frame 0, already bold) the identity hue also blends
// toward Gold — a glow, not just a harder flash. At the trough (frame 2,
// already dim) it eases a little toward Base instead — receding, not just
// darker. Frames 1 and 3 (normal weight) stay the pure identity hue
// untouched, so TestThemeIdentityCoherence's "breath at normal phase IS
// the identity hue" contract holds exactly.
func thBreath(name string, frame int) string {
	if ThemeMode != "rose-pine" {
		return BreathColorForProject(name, frame)
	}
	hue := rpIdentityFor(name)
	switch frame {
	case 0:
		hue = hue.lerp(rpGold, 0.35)
	case 2:
		hue = hue.lerp(rpBase, 0.15)
	}
	return hue.fgBright(BreathBrightness[frame%len(BreathBrightness)])
}

// frame is the animator's breath index (0..3). The ACTIVE tiers breathe
// with it (delight, 2026-08-20): the gradient's fade compresses at the
// breath's peak (colors sit closer to the mood hue — the divider glows)
// and relaxes at the trough, on the SAME clock as the current-session ▌,
// so the whole frame inhales together. Cell 0 (t=0) is untouched by
// construction — the exact mood hue is the semantic anchor and never
// moves. The idle tier ignores frame entirely: nothing happening stays
// perfectly still; motion means life. Classic ignores frame (flat color).
func thDivider(moodClassic string, n int, frame int) string {
	if ThemeMode != "rose-pine" {
		out := moodClassic
		for i := 0; i < n; i++ {
			out += "─"
		}
		return out
	}
	// The divider is the one place a flat, near-zero-information line is
	// deliberately spent on pure richness (delight pass, 2026-08-20) — it's
	// a single fixed row, never multiplied per-project, so enriching it
	// doesn't reopen the color-budget problem (many rows competing for
	// attention). But the mood tier IS real information (MoodFor's doc
	// comment: it replaced a whole header row), so cell 0 stays the exact
	// mood hue and idle keeps its original subdued single-stop fade — only
	// the three ACTIVE tiers (red/orange/green) get a second stop, a
	// complementary hue the gradient passes through before settling to
	// Muted, instead of racing straight to background. Nothing happening
	// stays quiet; something happening gets to be pretty.
	if moodClassic == MoodIdle {
		out := ""
		for i := 0; i < n; i++ {
			t := float64(i) / float64(n-1)
			out += rpMuted.lerp(rpBase, t*0.85).fg() + "─"
		}
		return out
	}
	var from, mid rpRGB
	switch moodClassic {
	case MoodRed:
		from, mid = rpLove, rpRose
	case Orange:
		from, mid = rpGold, rpRose
	default: // MoodGreen
		from, mid = rpFoam, rpPine
	}
	// Breath phase: how much the fade compresses toward the mood hue.
	// Indexed by the same 0..3 cycle as BreathBrightness (0 = peak,
	// 2 = trough), scaling t multiplicatively so t=0 stays exactly 0.
	phase := [4]float64{0.22, 0.10, 0, 0.10}[((frame%4)+4)%4]
	out := ""
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1) * (1 - phase)
		var c rpRGB
		if t <= 0.5 {
			c = from.lerp(mid, t*2)
		} else {
			c = mid.lerp(rpMuted, (t-0.5)*2)
		}
		out += c.fg() + "─"
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
