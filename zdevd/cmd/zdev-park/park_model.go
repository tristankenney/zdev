package main

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// textInputWidth is the textinput's fixed content width in columns.
// park_view.go's box arithmetic depends on this exact value (boxWidth - 4)
// to keep the border's right edge landing in the same column every frame —
// see that file's comment for why.
const textInputWidth = boxWidth - 4

// parkKeyMap is the popup's two bindings — enter to park (or anchor, in
// -anchor mode), esc/ctrl+c to drop without acting. bubbles/help renders the
// footer legend from these; only the Enter binding's help text differs
// between modes.
type parkKeyMap struct {
	Enter key.Binding
	Esc   key.Binding
}

func (k parkKeyMap) ShortHelp() []key.Binding { return []key.Binding{k.Enter, k.Esc} }

func (k parkKeyMap) FullHelp() [][]key.Binding { return [][]key.Binding{{k.Enter, k.Esc}} }

var defaultParkKeys = parkKeyMap{
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "park")),
	Esc:   key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "drop")),
}

// defaultAnchorKeys is -anchor mode's keymap (phase 3B, "the by-hand anchor
// prompt" — docs/design/command-centre.md's anchor lifecycle path 3): same
// two bindings, only the Enter help text changes so the footer reads
// "enter anchor . esc drop" instead of "enter park . esc drop".
var defaultAnchorKeys = parkKeyMap{
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "anchor")),
	Esc:   key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "drop")),
}

// parkResultMsg carries the outcome of the deferred park call (parkCmd) back
// into Update. err is non-nil on a dial failure or an explicit daemon
// rejection (empty text slipping past the local check, a stopped hub, …).
type parkResultMsg struct{ err error }

// parkModel is the popup's whole state. Deliberately tiny — per the design
// ("there is nothing to browse, on purpose") there is exactly one field of
// real interest (the textinput's value) plus the bookkeeping needed to
// commit-then-quit exactly once.
type parkModel struct {
	input textinput.Model
	help  help.Model
	keys  parkKeyMap

	// label titles the box border ("park" or "anchor") — park_view.go reads
	// it directly rather than a hardcoded string, so the two modes share
	// one View().
	label string

	// compact is set from the popup's own geometry: when tmux opens this
	// as a thin bottom-edge strip (display-popup -h 2 -y S -B), a boxed
	// prompt cannot fit and — more to the point — should not be there. A
	// centred box makes capture feel like a MODE the operator entered,
	// which is the derailment the park exists to prevent (live feedback,
	// 2026-08-04). Short popup ⇒ one line, no border. Tall popup ⇒ the
	// original box, still correct for a hand-run `zdev-park`.
	compact bool
	// width is the terminal's column count, used only in compact mode to
	// size the input so the key legend stays on the same line.
	width int

	ctx context.Context
	// submitFn is the injectable seam for the actual daemon round-trip —
	// production wires it to socket.DialPark in park mode, or a
	// socket.DialAnchorSet wrapper in -anchor mode (main.go). Tests
	// substitute a fake so Update's submit-and-quit path is verifiable
	// without a socket. Named generically (not parkFn) because the same
	// field and the same Update logic now serve both modes — see
	// newAnchorModel's doc comment for why this is an extension of the
	// park popup rather than a third binary.
	submitFn func(context.Context, string) (bool, error)

	// initCmd is textinput.Focus()'s returned Cmd, captured at construction
	// time and replayed from Init — Focus() must run once to start cursor
	// blinking, and Init is the only place a Cmd can originate before the
	// first Update.
	initCmd tea.Cmd

	// parked/parkErr record the outcome for main.go's post-Run stderr
	// surface (see that file's comment on the one exception to "prints
	// nothing"). parked is set the instant Enter fires (before the daemon
	// round-trip resolves) so a torn-down popup mid-dial is still
	// recognizable as "an attempt happened", not silently indistinguishable
	// from esc/ctrl+c. The name predates -anchor mode but is kept as-is —
	// it means "the popup committed to a round-trip", park or anchor alike.
	parked  bool
	parkErr error
}

// newParkModel builds the park-mode popup ready to focus and blink
// immediately. ctx is threaded through to every parkCmd invocation so a
// SIGINT/SIGTERM during an in-flight dial cancels it rather than leaking
// past program exit.
func newParkModel(ctx context.Context, parkFn func(context.Context, string) (bool, error)) *parkModel {
	return newPromptModel(ctx, parkFn, "park", "park a thought…", defaultParkKeys)
}

// newAnchorModel builds the -anchor mode popup (phase 3B, "the by-hand
// anchor prompt" — docs/design/command-centre.md's anchor lifecycle path
// 3: "for work that lives in no list — a phone call, an ad-hoc favour").
// Same textinput popup, same Update logic as park mode; only the box
// label, placeholder, keymap help text, and the injected round-trip differ
// (anchorFn wraps socket.DialAnchorSet with an empty project — listless
// work, per the design note).
func newAnchorModel(ctx context.Context, anchorFn func(context.Context, string) (bool, error)) *parkModel {
	return newPromptModel(ctx, anchorFn, "anchor", "anchor on…", defaultAnchorKeys)
}

// newPromptModel is the shared constructor park and anchor mode both call —
// "extend cmd/zdev-park, do not fork a third prompt binary" per the brief.
func newPromptModel(ctx context.Context, submitFn func(context.Context, string) (bool, error), label, placeholder string, keys parkKeyMap) *parkModel {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = 240
	ti.Width = textInputWidth
	initCmd := ti.Focus()

	return &parkModel{
		input:    ti,
		help:     help.New(),
		keys:     keys,
		label:    label,
		ctx:      ctx,
		submitFn: submitFn,
		initCmd:  initCmd,
	}
}

func (m *parkModel) Init() tea.Cmd {
	return m.initCmd
}

// Update is a pure state transition — project-conventions: no field mutation
// escapes this call except on m itself (bubbletea's pointer-receiver
// convention, matching cmd/zdev-round's roundModel), and the one side effect
// this popup needs (the daemon round-trip) is always deferred to a returned
// tea.Cmd, never run inline here.
//
// The four behaviors the design and brief both call out:
//   - typing: delegated to the embedded textinput, unconditionally, for any
//     msg that isn't consumed by a binding below.
//   - enter with non-empty text: fires parkCmd (the round-trip) but does NOT
//     quit yet — quitting happens only once parkResultMsg confirms the round-
//     trip finished, so a killed process never races an in-flight write.
//   - esc/ctrl+c: quits immediately WITHOUT ever calling parkFn — dropping a
//     thought is silent and instant, no daemon involved.
//   - enter with an empty/whitespace-only value: a no-op: the popup stays
//     open, nothing is sent, matching the design's "there is nothing to
//     browse" — an accidental bare enter must not close the only chance to
//     capture the thought.
func (m *parkModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Geometry decides the form. Two lines or fewer is the bottom-edge
		// strip; anything taller keeps the box.
		m.width = msg.Width
		m.compact = msg.Height > 0 && msg.Height <= 2
		if m.compact {
			// Reserve the prefix ("  park › ") and the key legend so the
			// whole prompt stays on ONE line; textinput pads to its Width,
			// so this is what stops it shoving the legend off the edge.
			reserve := len(m.label) + 6 + len(m.help.ShortHelpView(m.keys.ShortHelp())) + 4
			w := msg.Width - reserve
			if w < 12 {
				w = 12
			}
			m.input.Width = w
		} else {
			m.input.Width = textInputWidth
		}
		return m, nil

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Esc):
			return m, tea.Quit

		case key.Matches(msg, m.keys.Enter):
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.parked = true
			return m, m.parkCmd(text)
		}

	case parkResultMsg:
		m.parkErr = msg.err
		return m, tea.Quit
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// parkCmd defers the actual daemon round-trip to a Cmd so Update stays
// I/O-free — same shape as cmd/zdev-round's jumpCmd/pollCmd. The name
// predates -anchor mode; it dispatches through m.submitFn regardless of
// which mode built this model.
func (m *parkModel) parkCmd(text string) tea.Cmd {
	fn := m.submitFn
	ctx := m.ctx
	return func() tea.Msg {
		_, err := fn(ctx, text)
		return parkResultMsg{err: err}
	}
}
