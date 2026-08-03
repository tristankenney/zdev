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

// parkKeyMap is the popup's two bindings — enter to park, esc/ctrl+c to drop
// without parking. bubbles/help renders the footer legend from these.
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

	ctx context.Context
	// parkFn is the injectable seam for the actual daemon round-trip —
	// production wires it to socket.DialPark (main.go); tests substitute a
	// fake so Update's park-and-quit path is verifiable without a socket.
	parkFn func(context.Context, string) (bool, error)

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
	// from esc/ctrl+c.
	parked  bool
	parkErr error
}

// newParkModel builds the popup ready to focus and blink immediately. ctx is
// threaded through to every parkCmd invocation so a SIGINT/SIGTERM during an
// in-flight dial cancels it rather than leaking past program exit.
func newParkModel(ctx context.Context, parkFn func(context.Context, string) (bool, error)) *parkModel {
	ti := textinput.New()
	ti.Placeholder = "park a thought…"
	ti.CharLimit = 240
	ti.Width = textInputWidth
	initCmd := ti.Focus()

	return &parkModel{
		input:   ti,
		help:    help.New(),
		keys:    defaultParkKeys,
		ctx:     ctx,
		parkFn:  parkFn,
		initCmd: initCmd,
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
// I/O-free — same shape as cmd/zdev-round's jumpCmd/pollCmd.
func (m *parkModel) parkCmd(text string) tea.Cmd {
	fn := m.parkFn
	ctx := m.ctx
	return func() tea.Msg {
		_, err := fn(ctx, text)
		return parkResultMsg{err: err}
	}
}
