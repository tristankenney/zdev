package main

import (
	"strings"

	"github.com/tristankenney/zdev/zdevd/internal/render"
)

// boxWidth is the popup's fixed total border width in columns. The tmux
// binding opens `display-popup -w 60 -h 5` (config/zdev.tmux.conf); boxWidth
// leaves a small margin inside that so the border never wraps in a terminal
// with slightly different cell-width accounting. A park prompt has no
// business reflowing on tea.WindowSizeMsg the way a resizable pane would —
// it's a fixed-size popup, so the width is a constant, not state.
const boxWidth = 56

// View draws the park prompt entirely by hand: lipgloss may be imported
// ONLY by internal/render/lipgloss.go (scripts/check-no-lipgloss-scatter.sh
// enforces this at build time — lipgloss's own auto-detecting renderer
// silently strips color outside a real tty, which is exactly why that one
// pinned renderer exists), so the rounded border here is plain box-drawing
// characters plus internal/render's exported ANSI constants — the same
// "plain string tokens, no lipgloss.Style" path cmd/zdev-round's View
// already established.
//
// Width bookkeeping: bubbles/textinput pads its OWN View() output to
// exactly ti.Width visual columns (see textinput.Model.View — it measures
// the raw value/placeholder with uniseg/lipgloss.Width, which correctly
// ignores ANSI, and pads with plain spaces) as long as Prompt is left at its
// default "". That means this file never needs to measure inputView's
// rendered width itself — it just has to keep boxWidth and textInputWidth
// (park_model.go) in the fixed relationship boxWidth == textInputWidth + 4
// (border char + gutter space on each side) and place the input verbatim.
func (m *parkModel) View() string {
	label := " park "
	fill := boxWidth - 4 - len(label) // 4 = the two corner-adjacent dashes on each end
	if fill < 0 {
		fill = 0
	}
	top := render.Dim + "╭─" + render.Reset +
		render.Bold + label + render.Reset +
		render.Dim + strings.Repeat("─", fill) + "─╮" + render.Reset

	bottom := render.Dim + "╰" + strings.Repeat("─", boxWidth-2) + "╯" + render.Reset

	mid := render.Dim + "│ " + render.Reset + m.input.View() + render.Dim + " │" + render.Reset

	var b strings.Builder
	b.WriteString(top)
	b.WriteByte('\n')
	b.WriteString(mid)
	b.WriteByte('\n')
	b.WriteString(bottom)
	b.WriteByte('\n')
	b.WriteString(render.Dim + m.help.ShortHelpView(m.keys.ShortHelp()) + render.Reset)
	return b.String()
}
